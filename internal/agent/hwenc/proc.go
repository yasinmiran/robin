//go:build linux

package hwenc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yasinmiran/robin/internal/tail"
)

// proc is one run of an encoder tool: NV12 frames in on stdin, framed access
// units out on stdout, exactly one per frame.
type proc struct {
	cmd    *exec.Cmd
	stdin  *os.File
	stdout *os.File
	aus    chan []byte
	// readErr says why aus closed; it is set before the close.
	readErr     error
	stderr      *stderrTail
	stderrFatal bool

	bitrate int           // what the tool was started with, bits per second
	started time.Duration // stream time of the first frame
	frames  int
}

// startProc runs r for frames of c's size. The tool runs in its own process
// group, killed whole on close, and is also killed by the kernel if the agent
// dies first.
func startProc(r *route, c config) (*proc, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	// A frame is megabytes; the default 64 KiB pipe would take dozens of
	// wakeups to cross. 1 MiB is the unprivileged limit; failing to grow it
	// only costs speed.
	if raw, err := inW.SyscallConn(); err == nil {
		raw.Control(func(fd uintptr) { unix.FcntlInt(fd, unix.F_SETPIPE_SZ, 1<<20) })
	}

	cmd := exec.Command(r.path, r.args(c)...)
	cmd.Env = r.environ()
	cmd.Stdin = inR
	cmd.Stdout = outW
	stderr := newStderrTail()
	cmd.Stderr = stderr
	// Pdeathsig fires when the thread that forked exits, which the Go runtime
	// does not do to threads it has not locked, so in practice it means the
	// agent died. Close kills the process group either way.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	// Wait also waits for stderr to drain, which a grandchild that escaped
	// the group could hold open.
	cmd.WaitDelay = time.Second
	err = cmd.Start()
	inR.Close()
	outW.Close()
	if err != nil {
		inW.Close()
		outR.Close()
		return nil, fmt.Errorf("start %s: %w", r.name, err)
	}
	p := &proc{
		cmd: cmd, stdin: inW, stdout: outR, aus: make(chan []byte, 2), stderr: stderr,
		stderrFatal: r.stderrFatal, bitrate: c.bitrate,
	}
	go p.read(r.framing)
	return p, nil
}

func (p *proc) read(f framing) {
	s := f(p.stdout)
	for {
		au, err := s.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = errors.New("exited")
			}
			p.readErr = err
			close(p.aus)
			return
		}
		p.aus <- au
	}
}

// errorGrace is how long an access unit may still take once a tool whose
// stderr carries only errors has printed one. ffmpeg can report a failed
// encoder and then wait on its input forever.
const errorGrace = 100 * time.Millisecond

// encode writes one frame and returns its access unit, giving up at
// deadline. After an error the proc is done: kill it.
func (p *proc) encode(y, uv []byte, deadline time.Time) ([]byte, error) {
	p.stdin.SetWriteDeadline(deadline)
	for _, b := range [][]byte{y, uv} {
		if _, err := p.stdin.Write(b); err != nil {
			return nil, p.failure(fmt.Errorf("write frame: %w", err))
		}
	}
	var printed <-chan struct{}
	if p.stderrFatal {
		printed = p.stderr.written
	}
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	for {
		select {
		case au, ok := <-p.aus:
			if !ok {
				return nil, p.failure(p.readErr)
			}
			p.frames++
			return au, nil
		case <-printed:
			printed = nil
			t.Reset(min(time.Until(deadline), errorGrace))
		case <-t.C:
			return nil, p.failure(errors.New("no access unit in time"))
		}
	}
}

// failure adds what the tool printed on stderr, which is where the real
// reason usually is.
func (p *proc) failure(err error) error {
	if s := strings.TrimSpace(p.stderr.String()); s != "" {
		return fmt.Errorf("%w: %s", err, s)
	}
	return err
}

// kill stops the tool and everything it started and reaps it.
func (p *proc) kill() {
	syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	p.stdin.Close()
	p.cmd.Wait()
	p.stdout.Close()
}

// stderrTail keeps the end of a tool's stderr. written is closed by the
// first write, which for a tool whose stderr carries only errors is the
// first sign of a failure.
type stderrTail struct {
	tail.Buffer
	written chan struct{}
	once    sync.Once
}

func newStderrTail() *stderrTail { return &stderrTail{written: make(chan struct{})} }

func (t *stderrTail) Write(b []byte) (int, error) {
	n, err := t.Buffer.Write(b)
	t.once.Do(func() { close(t.written) })
	return n, err
}
