//go:build linux

package camera

import (
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agent/h264"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// solid is a w x h picture of one colour, its rows padded as a decoder's
// are.
func solid(w, h int, y, u, v byte) *h264.Picture {
	ys, cs := w+32, (w+1)/2+16
	p := &h264.Picture{Width: w, Height: h, YStride: ys, CStride: cs,
		Y: make([]byte, ys*h), U: make([]byte, cs*((h+1)/2)), V: make([]byte, cs*((h+1)/2))}
	fillBytes(p.Y, y)
	fillBytes(p.U, u)
	fillBytes(p.V, v)
	return p
}

// at is the Y, U and V of frame f at luma pixel x, y.
func at(f frame, x, y int) (byte, byte, byte) {
	py, pu, pv := f.planes()
	c := y/2*(Width/2) + x/2
	return py[y*Width+x], pu[c], pv[c]
}

func TestFitSameSizeCopies(t *testing.T) {
	pic := solid(Width, Height, 81, 90, 240)
	pic.Y[5*pic.YStride+7] = 200
	f := newFrame()
	newFitter(Width, Height).fit(f, pic)
	if y, u, v := at(f, 0, 0); y != 81 || u != 90 || v != 240 {
		t.Errorf("corner %d/%d/%d", y, u, v)
	}
	if y, _, _ := at(f, 7, 5); y != 200 {
		t.Errorf("the marked pixel is %d", y)
	}
	if y, u, v := at(f, Width-1, Height-1); y != 81 || u != 90 || v != 240 {
		t.Errorf("far corner %d/%d/%d", y, u, v)
	}
}

func TestFitScalesUpAWidePicture(t *testing.T) {
	pic := solid(640, 360, 120, 100, 160)
	f := newFrame()
	fit := newFitter(640, 360)
	if fit.x0 != 0 || fit.y0 != 0 || fit.w != Width || fit.h != Height {
		t.Errorf("fitted at %d,%d %dx%d, want the whole frame", fit.x0, fit.y0, fit.w, fit.h)
	}
	fit.fit(f, pic)
	for _, p := range [][2]int{{0, 0}, {640, 360}, {Width - 1, Height - 1}} {
		if y, u, v := at(f, p[0], p[1]); y != 120 || u != 100 || v != 160 {
			t.Errorf("at %v: %d/%d/%d", p, y, u, v)
		}
	}
}

// TestFitPillarboxesANarrowPicture: a 4:3 camera fills the frame's height
// and sits in the middle, black either side.
func TestFitPillarboxesANarrowPicture(t *testing.T) {
	pic := solid(640, 480, 120, 100, 160)
	f := newFrame()
	fit := newFitter(640, 480)
	if fit.h != Height || fit.w != 960 || fit.x0 != 160 || fit.y0 != 0 {
		t.Fatalf("fitted at %d,%d %dx%d, want 960x720 at 160,0", fit.x0, fit.y0, fit.w, fit.h)
	}
	fit.fit(f, pic)
	if y, u, v := at(f, 100, 300); y != 16 || u != 128 || v != 128 {
		t.Errorf("the bar is %d/%d/%d, want black", y, u, v)
	}
	if y, u, v := at(f, 640, 300); y != 120 || u != 100 || v != 160 {
		t.Errorf("the middle is %d/%d/%d", y, u, v)
	}
	if y, _, _ := at(f, 1200, 300); y != 16 {
		t.Errorf("the right bar is %d, want black", y)
	}
}

func TestFitLetterboxesATallPicture(t *testing.T) {
	fit := newFitter(1920, 1440)
	if fit.w != 960 || fit.h != Height {
		t.Errorf("4:3 fits as %dx%d", fit.w, fit.h)
	}
	fit = newFitter(2560, 1080)
	if fit.w != Width || fit.h != 540 || fit.y0 != 90 {
		t.Errorf("21:9 fits as %dx%d at y %d, want 1280x540 at 90", fit.w, fit.h, fit.y0)
	}
}

// TestFitOddSizes: a picture of odd dimensions samples inside itself.
func TestFitOddSizes(t *testing.T) {
	pic := solid(333, 187, 50, 60, 70)
	f := newFrame()
	newFitter(333, 187).fit(f, pic)
	if y, u, v := at(f, Width/2, Height/2); y != 50 || u != 60 || v != 70 {
		t.Errorf("middle %d/%d/%d", y, u, v)
	}
}

func TestSteps(t *testing.T) {
	if got, want := steps(4, 8), []int{0, 0, 1, 1, 2, 2, 3, 3}; !slices.Equal(got, want) {
		t.Errorf("steps(4, 8) = %v, want %v", got, want)
	}
	for i, s := range steps(1000, 7) {
		if s < 0 || s >= 1000 {
			t.Errorf("step %d samples %d, outside the source", i, s)
		}
	}
}

func TestPipelines(t *testing.T) {
	head := "fdsrc fd=0 blocksize=1382400 ! rawvideoparse use-sink-caps=false format=i420 width=1280 height=720 framerate=30/1 ! queue leaky=downstream"
	pw := strings.Join(pipeline("", "robin-camera-7"), " ")
	for _, want := range []string{head, "! videoconvert ! pipewiresink mode=provide sync=false ",
		`stream-properties="props,media.class=Video/Source,media.role=Camera,node.name=robin-camera-7,node.description=\"Robin camera\""`} {
		if !strings.Contains(pw, want) {
			t.Errorf("PipeWire pipeline lacks %q:\n%s", want, pw)
		}
	}
	v4l := strings.Join(pipeline("/dev/video9", "x"), " ")
	if !strings.Contains(v4l, head) || !strings.HasSuffix(v4l, "! videoconvert ! v4l2sink device=/dev/video9 sync=false") {
		t.Errorf("v4l2loopback pipeline:\n%s", v4l)
	}
}

// fakeDevice records the frames written to it.
type fakeDevice struct {
	mu     sync.Mutex
	frames []frame
	fail   error
	closed bool
}

func (d *fakeDevice) Write(b []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.frames = append(d.frames, append(frame(nil), b...))
	return d.fail
}

func (d *fakeDevice) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	return nil
}

func (d *fakeDevice) written() []frame {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]frame(nil), d.frames...)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func shortIdle(t *testing.T, d time.Duration) {
	t.Helper()
	old := idleEvery
	idleEvery = d
	t.Cleanup(func() { idleEvery = old })
}

// TestBlackUntilPictures: the device shows black at once and while nothing
// arrives, a picture as soon as one does, and black again once they stop.
func TestBlackUntilPictures(t *testing.T) {
	shortIdle(t, 50*time.Millisecond)
	dev := &fakeDevice{}
	c := newCamera(dev, "test", quiet())
	waitFor(t, "a black frame", func() bool { return len(dev.written()) >= 1 })
	if y, u, v := at(dev.written()[0], 10, 10); y != 16 || u != 128 || v != 128 {
		t.Errorf("first frame %d/%d/%d, want black", y, u, v)
	}
	if !c.show(newFitter(Width, Height), solid(Width, Height, 200, 50, 60)) {
		t.Fatal("the first picture was dropped")
	}
	waitFor(t, "the picture", func() bool {
		for _, f := range dev.written() {
			if y, _, _ := at(f, 10, 10); y == 200 {
				return true
			}
		}
		return false
	})
	waitFor(t, "black after the pictures stop", func() bool {
		w := dev.written()
		y, _, _ := at(w[len(w)-1], 10, 10)
		return y == 16
	})
	c.Close()
	c.Close()
	if !dev.closed {
		t.Error("Close left the device open")
	}
	select {
	case <-c.done:
	default:
		t.Fatal("the writer outlived Close")
	}
	n := len(dev.written())
	c.show(newFitter(Width, Height), solid(Width, Height, 1, 2, 3))
	if len(dev.written()) != n {
		t.Error("a closed camera showed a picture")
	}
}

// TestShowDropsWhenTheWriterIsBehind: one picture waits for the writer;
// the next is dropped rather than queued behind it.
func TestShowDropsWhenTheWriterIsBehind(t *testing.T) {
	shortIdle(t, time.Hour)
	c := &Camera{log: quiet(), frames: make(chan frame, 1), free: make(chan frame, 3)}
	for range 3 {
		c.free <- newFrame()
	}
	fit, pic := newFitter(Width, Height), solid(Width, Height, 1, 2, 3)
	if !c.show(fit, pic) || c.show(fit, pic) {
		t.Error("want the first picture queued and the second dropped")
	}
	if len(c.free) != 2 {
		t.Errorf("%d frames free, want the dropped one back", len(c.free))
	}
}

// logBuffer keeps what a logger writes, for a test to read while the
// camera's writer logs.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestADeviceThatRefusesIsLoggedNotFatal: the writer carries on past a
// device that takes no picture, and says so once rather than every time.
func TestADeviceThatRefusesIsLoggedNotFatal(t *testing.T) {
	shortIdle(t, 10*time.Millisecond)
	dev := &fakeDevice{fail: errors.New("restarting")}
	var log logBuffer
	c := newCamera(dev, "test", slog.New(slog.NewTextHandler(&log, nil)))
	waitFor(t, "a few attempts", func() bool { return len(dev.written()) >= 3 })
	c.Close()
	warning := `level=WARN msg="camera: the device took no picture" err=restarting times=1`
	if got := log.String(); strings.Count(got, "level=WARN") != 1 || !strings.Contains(got, warning) {
		t.Errorf("logged\n%s\nwant the one warning %s", got, warning)
	}
}

func TestStartWithoutPipeWireSaysSo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("PIPEWIRE_REMOTE", "")
	_, err := Start(Options{Log: quiet()})
	if err == nil || !strings.Contains(err.Error(), "no PipeWire for this user at "+dir+"/pipewire-0") {
		t.Fatalf("Start = %v, want no PipeWire", err)
	}
}
