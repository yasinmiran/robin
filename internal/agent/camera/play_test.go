//go:build linux && cgo && openh264

package camera

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"

	"github.com/yasinmiran/robin/internal/agent/h264"
	"github.com/yasinmiran/robin/internal/agent/yuv"
)

// browserCamera encodes n frames of a w x h picture that changes colour
// halfway, as the browser's camera sender would, and packetizes them.
func browserCamera(t *testing.T, w, h, n int) [][]*rtp.Packet {
	t.Helper()
	enc, err := h264.New(w, h, 30, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	pk := rtp.NewPacketizer(1200, 102, 0x1234, &codecs.H264Payloader{}, rtp.NewFixedSequencer(100), 90000)
	var frames [][]*rtp.Packet
	img := yuv.NewI420(w, h)
	for i := range n {
		y, u, v := byte(81), byte(90), byte(240)
		if i >= n/2 {
			y, u, v = 145, 54, 34
		}
		fillBytes(img.Y, y)
		fillBytes(img.U, u)
		fillBytes(img.V, v)
		for x := range 16 {
			img.Y[(i%h)*w+x] = 235 // a line that moves, so no frame is skipped
		}
		au, err := enc.Encode(img, time.Duration(i)*time.Second/30, i == 0)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, pk.Packetize(append([]byte(nil), au...), 3000))
	}
	return frames
}

type packetReader struct {
	packets []*rtp.Packet
	err     error
}

func (r *packetReader) ReadRTP() (*rtp.Packet, interceptor.Attributes, error) {
	if len(r.packets) == 0 {
		return nil, nil, r.err
	}
	p := r.packets[0]
	r.packets = r.packets[1:]
	return p, nil, nil
}

func flatten(frames [][]*rtp.Packet) []*rtp.Packet {
	var ps []*rtp.Packet
	for _, f := range frames {
		ps = append(ps, f...)
	}
	return ps
}

func near(a, b byte) bool { return a+6 >= b && b+6 >= a }

// TestPlayShowsTheBrowsersPictures: what the browser sends reaches the
// device fitted to its size, the second colour after the first.
func TestPlayShowsTheBrowsersPictures(t *testing.T) {
	shortIdle(t, time.Hour)
	dev := &fakeDevice{}
	c := newCamera(dev, "test", quiet())
	defer c.Close()
	frames := browserCamera(t, 640, 360, 20)
	var keys atomic.Int32
	r := &pacedReader{packets: flatten(frames), every: 5 * time.Millisecond}
	if err := c.Play(r, func() { keys.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if keys.Load() != 0 {
		t.Errorf("%d keyframes asked for on a clean stream", keys.Load())
	}
	waitFor(t, "both colours on the device", func() bool {
		var first, second bool
		for _, f := range dev.written() {
			y, u, v := at(f, 640, 400)
			first = first || near(y, 81) && near(u, 90) && near(v, 240)
			second = second || first && near(y, 145) && near(u, 54) && near(v, 34)
		}
		return second
	})
}

// pacedReader hands its packets out every so often, as a network would.
type pacedReader struct {
	packets []*rtp.Packet
	every   time.Duration
}

func (r *pacedReader) ReadRTP() (*rtp.Packet, interceptor.Attributes, error) {
	if len(r.packets) == 0 {
		return nil, nil, io.EOF
	}
	time.Sleep(r.every)
	p := r.packets[0]
	r.packets = r.packets[1:]
	return p, nil, nil
}

// TestPlayAsksForAKeyframeAfterLoss: a frame that lost a packet cannot be
// shown, nor can what follows it until the browser sends an IDR.
func TestPlayAsksForAKeyframeAfterLoss(t *testing.T) {
	shortIdle(t, time.Hour)
	frames := browserCamera(t, 640, 360, 12)
	if len(frames[5]) < 2 {
		t.Skip("frame 5 fits one packet; nothing to lose")
	}
	frames[5] = frames[5][1:]
	c := newCamera(&fakeDevice{}, "test", quiet())
	defer c.Close()
	var keys atomic.Int32
	if err := c.Play(&packetReader{packets: flatten(frames), err: io.EOF}, func() { keys.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if keys.Load() == 0 {
		t.Error("no keyframe asked for after a lost packet")
	}
}

func TestPlayEndsWithTheTrack(t *testing.T) {
	c := newCamera(&fakeDevice{}, "test", quiet())
	defer c.Close()
	gone := errors.New("receiver closed")
	if err := c.Play(&packetReader{err: gone}, func() {}); !errors.Is(err, gone) {
		t.Errorf("Play = %v, want the track's error", err)
	}
}

// TestStartRunsThePipeline starts the camera against a stand-in for
// gst-launch-1.0 that keeps what it is given, and finds black frames of
// the right size in it.
func TestStartRunsThePipeline(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"$@\" >" + dir + "/args\nexec cat >" + dir + "/frames\n"
	if err := os.WriteFile(filepath.Join(dir, "gst-launch-1.0"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	c, err := Start(Options{Device: "/dev/video42", Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	if c.Where != "/dev/video42" {
		t.Errorf("Where = %q", c.Where)
	}
	waitFor(t, "a black frame", func() bool {
		fi, err := os.Stat(filepath.Join(dir, "frames"))
		return err == nil && fi.Size() >= Width*Height*3/2
	})
	c.Close()
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if !strings.Contains(string(args), "v4l2sink device=/dev/video42") {
		t.Errorf("args %s", args)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "frames"))
	if len(b)%(Width*Height*3/2) != 0 || b[0] != 16 || b[Width*Height] != 128 {
		t.Errorf("%d bytes, starting %d, chroma %d: want whole black frames", len(b), b[0], b[Width*Height])
	}

	c, err = Start(Options{Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Where != "PipeWire" {
		t.Errorf("Where = %q", c.Where)
	}
}
