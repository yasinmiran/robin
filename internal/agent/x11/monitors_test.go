//go:build linux

package x11

import (
	"fmt"
	"testing"
	"time"

	"github.com/jezek/xgb/xproto"

	"github.com/yasinmiran/robin/internal/agent/display"
	"github.com/yasinmiran/robin/internal/protocol"
)

// splitMonitors divides the test display into LEFT and RIGHT monitors of
// different widths, RIGHT primary and holding the screen's output, the way
// xrandr --setmonitor defines virtual monitors.
func splitMonitors(t *testing.T, tx *testX) (left, right Rect) {
	t.Helper()
	w, h := tx.c.Size()
	cut := w * 5 / 12
	left, right = Rect{W: cut, H: h}, Rect{X: cut, W: w - cut, H: h}
	onDisplay(t, tx, "xrandr", "--setmonitor", "*RIGHT", geometry(right), "screen")
	onDisplay(t, tx, "xrandr", "--setmonitor", "LEFT", geometry(left), "none")
	return left, right
}

// geometry writes r as xrandr --setmonitor takes it, at about 96 DPI.
func geometry(r Rect) string {
	return fmt.Sprintf("%d/%dx%d/%d+%d+%d", r.W, r.W*254/960, r.H, r.H*254/960, r.X, r.Y)
}

// paint maps an override-redirect window of one colour on the test display.
func paint(t *testing.T, tx *testX, r Rect, rgb uint32) {
	t.Helper()
	x := tx.peer
	win, err := xproto.NewWindowId(x)
	if err != nil {
		t.Fatal(err)
	}
	xproto.CreateWindow(x, 0, win, tx.root, int16(r.X), int16(r.Y), uint16(r.W), uint16(r.H), 0,
		xproto.WindowClassInputOutput, 0, xproto.CwBackPixel|xproto.CwOverrideRedirect, []uint32{rgb, 1})
	xproto.MapWindow(x, win)
	xproto.GetInputFocus(x).Reply()
}

func TestMonitors(t *testing.T) {
	tx := startX(t, "sleep 1000")
	if !tx.c.randr15 {
		t.Skip("the X server predates RandR 1.5")
	}
	left, right := splitMonitors(t, tx)
	got, err := tx.c.Monitors()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Monitor{
		"LEFT":  {Name: "LEFT", Rect: left},
		"RIGHT": {Name: "RIGHT", Rect: right, Primary: true},
	}
	if len(got) != len(want) {
		t.Fatalf("monitors %+v, want %+v", got, want)
	}
	for _, m := range got {
		if m != want[m.Name] {
			t.Errorf("monitor %+v, want %+v", m, want[m.Name])
		}
	}
}

func TestViewportCapture(t *testing.T) {
	tx := startX(t, "sleep 1000")
	c := tx.c
	w, h := c.Size()
	left, right := Rect{W: w / 2, H: h}, Rect{X: w / 2, W: w - w/2, H: h}
	paint(t, tx, Rect{X: left.X + 100, Y: 100, W: 200, H: 100}, 0xff0000)
	paint(t, tx, Rect{X: right.X + 100, Y: 100, W: 200, H: 100}, 0x0000ff)

	for _, tc := range []struct {
		name string
		view Rect
		w, h int
		bgr  [3]byte // at 150,150 of the frame
	}{
		{"left", left, left.W, left.H, [3]byte{0, 0, 0xff}},
		{"right", right, right.W, right.H, [3]byte{0xff, 0, 0}},
		{"whole", Rect{}, w, h, [3]byte{0, 0, 0xff}},
		{"off the screen", Rect{X: w + 10, W: 100, H: 100}, w, h, [3]byte{0, 0, 0xff}},
	} {
		c.SetViewport(tc.view)
		if vw, vh := c.ViewSize(); vw != tc.w || vh != tc.h {
			t.Errorf("%s: ViewSize %dx%d, want %dx%d", tc.name, vw, vh, tc.w, tc.h)
		}
		var f *Frame
		for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			f = mustCapture(t, c)
			px := f.Pix[150*f.Stride+150*4:]
			if [3]byte{px[0], px[1], px[2]} == tc.bgr || time.Now().After(deadline) {
				break
			}
		}
		if f.Width != tc.w || f.Height != tc.h {
			t.Errorf("%s: frame %dx%d, want %dx%d", tc.name, f.Width, f.Height, tc.w, tc.h)
		}
		if px := f.Pix[150*f.Stride+150*4:]; [3]byte{px[0], px[1], px[2]} != tc.bgr {
			t.Errorf("%s: pixel at 150,150 is BGR %v, want %v", tc.name, px[:3], tc.bgr)
		}
	}
}

func TestViewportInput(t *testing.T) {
	tx := startX(t, "sleep 1000")
	c := tx.c
	w, h := c.Size()
	right := Rect{X: w / 2, Y: 40, W: w - w/2, H: h - 40}
	pointer := func() (int, int) {
		p, err := xproto.QueryPointer(tx.peer, tx.root).Reply()
		if err != nil {
			t.Fatal(err)
		}
		return int(p.RootX), int(p.RootY)
	}
	for _, tc := range []struct {
		name         string
		view         Rect
		x, y         int
		wantX, wantY int
	}{
		{"inside", right, 10, 20, right.X + 10, right.Y + 20},
		{"past the edge", right, 99999, 99999, right.X + right.W - 1, right.Y + right.H - 1},
		{"whole desktop", Rect{}, 10, 20, 10, 20},
	} {
		c.SetViewport(tc.view)
		if err := c.Input(protocol.Control{T: protocol.ControlMove, X: tc.x, Y: tc.y}); err != nil {
			t.Fatal(err)
		}
		if x, y := pointer(); x != tc.wantX || y != tc.wantY {
			t.Errorf("%s: move to %d,%d put the pointer at %d,%d, want %d,%d", tc.name, tc.x, tc.y, x, y, tc.wantX, tc.wantY)
		}
	}
}

// BenchmarkCaptureThreeMonitors grabs and hashes, as Capture does each tick
// once DAMAGE proved unreliable, the 5120x2640 root of a desktop with two
// 2560x1440 screens and a laptop panel, and then just one of the screens.
func BenchmarkCaptureThreeMonitors(b *testing.B) {
	for _, tc := range []struct {
		name string
		view Rect
	}{
		{"root 5120x2640", Rect{}},
		{"monitor 2560x1440", Rect{W: 2560, H: 1440}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			tx := startXWith(b, display.Options{Session: "sleep 1000", MaxWidth: 5120, MaxHeight: 2640})
			if w, h := tx.c.Size(); w != 5120 || h != 2640 {
				b.Fatalf("display is %dx%d", w, h)
			}
			tx.c.SetViewport(tc.view)
			w, h := tx.c.ViewSize()
			b.SetBytes(int64(w * h * 4))
			for b.Loop() {
				f, _, err := tx.c.Capture(true)
				if err != nil {
					b.Fatal(err)
				}
				pixelHash(f.Pix[:f.Stride*f.Height])
			}
		})
	}
}

// TestDamageOutsideViewport: drawing on a monitor that is not shown costs no
// capture, and drawing on the one shown still does.
func TestDamageOutsideViewport(t *testing.T) {
	tx := startX(t, "sleep 1000")
	c := tx.c
	w, h := c.Size()
	c.SetViewport(Rect{W: w / 2, H: h})
	waitIdle(t, c)

	paint(t, tx, Rect{X: w/2 + 100, Y: 100, W: 200, H: 100}, 0x00ff00)
	for end := time.Now().Add(1500 * time.Millisecond); time.Now().Before(end); time.Sleep(30 * time.Millisecond) {
		if _, changed, err := c.Capture(false); err != nil {
			t.Fatal(err)
		} else if changed {
			t.Fatal("drawing outside the viewport counted as a change")
		}
	}

	paint(t, tx, Rect{X: 100, Y: 100, W: 200, H: 100}, 0x00ff00)
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(30 * time.Millisecond) {
		if _, changed, err := c.Capture(false); err != nil {
			t.Fatal(err)
		} else if changed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("drawing inside the viewport went unnoticed")
		}
	}
	c.cap.mu.Lock()
	polling := c.cap.polling
	c.cap.mu.Unlock()
	if polling {
		t.Error("Capture stopped trusting DAMAGE")
	}
}
