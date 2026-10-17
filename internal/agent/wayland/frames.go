//go:build linux

package wayland

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yasinmiran/robin/internal/agent/desktop"
)

// firstFrameTimeout bounds the wait for the stream's first picture.
// pipewiresrc repeats the last frame every keepalive-time, so even a static
// screen produces one well inside it.
const firstFrameTimeout = 10 * time.Second

// frames reads I420 pictures from a gst-launch-1.0 subprocess that pulls
// the portal's PipeWire stream. y4menc frames the byte stream and states the
// size, and it copes with whatever row padding the converter leaves.
//
// Three buffers rotate: the reader fills back, publishes it as latest, and
// capture hands latest out as front, which the caller owns until its next
// capture.
type frames struct {
	cmd    *exec.Cmd
	out    io.ReadCloser
	stderr *tail
	ready  chan struct{} // closed once the size is known and buffers exist
	exited chan struct{} // closed once gst has exited and been reaped

	width, height int

	mu                  sync.Mutex
	front, latest, back []byte
	fresh               bool // latest holds a frame capture has not handed out
	frame               desktop.Frame
	err                 error // why reading stopped
	stopping            bool
}

// pipeline builds the gst-launch-1.0 arguments for node, read from the
// PipeWire connection passed as fd 3. width and height, when set, scale the
// picture to that size.
func pipeline(node uint32, width, height, fps int) []string {
	caps := "video/x-raw,format=I420,colorimetry=bt601"
	if width > 0 && height > 0 {
		caps += fmt.Sprintf(",width=%d,height=%d,pixel-aspect-ratio=1/1", width, height)
	}
	return []string{
		"-q",
		// keepalive-time resends the last frame on a static screen, so the
		// first frame cannot wait for the screen to change.
		"pipewiresrc", "fd=3", "path=" + strconv.FormatUint(uint64(node), 10), "keepalive-time=100",
		// max-framerate goes into the format offer, so the compositor paces
		// the stream itself; one over the target leaves room for jitter.
		"!", fmt.Sprintf("video/x-raw,max-framerate=%d/1", fps+1),
		"!", "videoconvert", "!", "videoscale", "!", caps,
		"!", "y4menc", "!", "fdsink", "fd=1", "sync=false",
	}
}

func startFrames(remote *os.File, node uint32, width, height, fps int) (*frames, error) {
	cmd := exec.Command("gst-launch-1.0", pipeline(node, width, height, fps)...)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+runtimeDir())
	cmd.ExtraFiles = []*os.File{remote}
	// An orphaned gst would keep the stream, and the screen sharing
	// indicator on the host, alive.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	f := &frames{cmd: cmd, stderr: &tail{max: 4 << 10}, ready: make(chan struct{}), exited: make(chan struct{})}
	cmd.Stderr = f.stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	f.out = out
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, errors.New("gst-launch-1.0 is not installed on the host; it reads the screen from PipeWire")
		}
		return nil, fmt.Errorf("start gst-launch-1.0: %w", err)
	}
	go f.run()

	select {
	case <-f.ready:
	case <-f.exited:
		return nil, f.reason()
	case <-time.After(firstFrameTimeout):
		f.stop()
		return nil, fmt.Errorf("no picture from the screen within %s%s", firstFrameTimeout, f.stderrNote())
	}
	return f, nil
}

func (f *frames) run() {
	defer close(f.exited)
	err := f.read(bufio.NewReaderSize(f.out, 1<<20))
	werr := f.cmd.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.stopping:
		f.err = errors.New("stopped")
	case werr != nil:
		f.err = fmt.Errorf("gst-launch-1.0 exited: %w%s", werr, f.stderrNote())
	default:
		f.err = fmt.Errorf("screen stream ended: %w%s", err, f.stderrNote())
	}
}

func (f *frames) read(r *bufio.Reader) error {
	header, err := r.ReadString('\n')
	if err != nil {
		return err
	}
	w, h, err := parseY4MHeader(header)
	if err != nil {
		return err
	}
	size := w*h + 2*((w+1)/2)*((h+1)/2)
	f.mu.Lock()
	f.width, f.height = w, h
	f.front, f.latest, f.back = make([]byte, size), make([]byte, size), make([]byte, size)
	f.frame = desktop.Frame{Width: w, Height: h, Stride: w, Format: desktop.I420}
	f.mu.Unlock()
	readyOnce := sync.OnceFunc(func() { close(f.ready) })
	for {
		line, err := r.ReadSlice('\n')
		if err != nil {
			return err
		}
		if !bytes.HasPrefix(line, []byte("FRAME")) {
			return fmt.Errorf("y4m: expected FRAME, got %q", line)
		}
		if _, err := io.ReadFull(r, f.back); err != nil {
			return err
		}
		f.mu.Lock()
		f.back, f.latest = f.latest, f.back
		f.fresh = true
		f.mu.Unlock()
		readyOnce()
	}
}

// parseY4MHeader reads the size from a YUV4MPEG2 stream header and checks
// the chroma layout is 4:2:0.
func parseY4MHeader(header string) (w, h int, err error) {
	fields := strings.Fields(header)
	if len(fields) == 0 || fields[0] != "YUV4MPEG2" {
		return 0, 0, fmt.Errorf("y4m: bad header %q", header)
	}
	for _, f := range fields[1:] {
		switch f[0] {
		case 'W':
			w, _ = strconv.Atoi(f[1:])
		case 'H':
			h, _ = strconv.Atoi(f[1:])
		case 'C':
			if !strings.HasPrefix(f, "C420") {
				return 0, 0, fmt.Errorf("y4m: chroma %s, want 4:2:0", f[1:])
			}
		}
	}
	if w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("y4m: no size in header %q", header)
	}
	return w, h, nil
}

// capture returns the newest frame. changed is false when no frame arrived
// since the last capture or the new one is identical to it, which is what
// pipewiresrc's keepalive sends on a static screen.
func (f *frames) capture() (*desktop.Frame, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	changed := false
	if f.fresh {
		f.front, f.latest = f.latest, f.front
		f.fresh = false
		changed = !bytes.Equal(f.front, f.latest)
	}
	if f.err != nil && !changed {
		return nil, false, f.err
	}
	f.frame.Pix = f.front
	return &f.frame, changed, nil
}

func (f *frames) reason() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *frames) stderrNote() string {
	if s := strings.TrimSpace(f.stderr.String()); s != "" {
		return ": " + s
	}
	return ""
}

// stop ends gst. Our end of its stdout closes first: gst-launch handles
// SIGTERM by draining to end of stream, which blocks in fdsink for ever if
// nobody reads.
func (f *frames) stop() {
	f.mu.Lock()
	f.stopping = true
	f.mu.Unlock()
	f.out.Close()
	f.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-f.exited:
	case <-time.After(2 * time.Second):
		f.cmd.Process.Kill()
		<-f.exited
	}
}

// tail keeps the last max bytes written to it.
type tail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
