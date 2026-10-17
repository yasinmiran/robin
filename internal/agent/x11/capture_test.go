//go:build linux

package x11

import (
	"bytes"
	"testing"
	"time"

	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/xproto"
)

func nonZero(f *Frame) bool {
	for y := range f.Height {
		row := f.Pix[y*f.Stride : y*f.Stride+f.Width*4]
		for i := 0; i < len(row); i += 4 {
			if row[i]|row[i+1]|row[i+2] != 0 {
				return true
			}
		}
	}
	return false
}

// waitIdle captures until DAMAGE reports a quiet screen.
func waitIdle(t *testing.T, c *Conn) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if _, changed, err := c.Capture(false); err != nil {
			t.Fatal(err)
		} else if !changed {
			return
		}
	}
	t.Fatal("screen never went idle")
}

func TestCapture(t *testing.T) {
	tx := startX(t, "xterm -geometry 80x24+0+0")
	c := tx.c
	if c.cap.noSHM {
		t.Fatal("MIT-SHM unavailable")
	}

	var f *Frame
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		var err error
		if f, _, err = c.Capture(true); err != nil {
			t.Fatal(err)
		}
		if nonZero(f) || time.Now().After(deadline) {
			break
		}
	}
	if f.Width != maxW || f.Height != maxH || f.Stride != maxW*4 || len(f.Pix) < f.Stride*f.Height {
		t.Fatalf("frame %dx%d stride %d len %d", f.Width, f.Height, f.Stride, len(f.Pix))
	}
	if !nonZero(f) {
		t.Fatal("xterm never showed up in the capture")
	}

	waitIdle(t, c)
	if _, changed, _ := c.Capture(false); changed {
		t.Error("idle screen reported a change")
	}
	if _, changed, _ := c.Capture(true); !changed {
		t.Error("force did not capture")
	}

	// Draw from another client: DAMAGE must notice.
	x := tx.peer
	win, _ := xproto.NewWindowId(x)
	xproto.CreateWindow(x, 0, win, tx.root, 500, 500, 200, 100, 0, xproto.WindowClassInputOutput, 0,
		xproto.CwBackPixel|xproto.CwOverrideRedirect, []uint32{0xff0000, 1})
	xproto.MapWindow(x, win)
	xproto.GetInputFocus(x).Reply()
	changed := false
	for deadline := time.Now().Add(2 * time.Second); !changed && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if f, changed, _ = c.Capture(false); changed {
			px := f.Pix[550*f.Stride+600*4:]
			if px[0] != 0 || px[1] != 0 || px[2] != 0xff {
				t.Errorf("pixel at 600,550 = %v, want red BGR 0,0,255", px[:3])
			}
		}
	}
	if !changed {
		t.Error("a new window did not register as damage")
	}

	// The GetImage fallback sees the same pixels.
	waitIdle(t, c)
	shmPix := bytes.Clone(mustCapture(t, c).Pix)
	c.cap.mu.Lock()
	c.cap.noSHM = true
	c.cap.mu.Unlock()
	plain := mustCapture(t, c)
	if !bytes.Equal(rgb(shmPix), rgb(plain.Pix)) {
		t.Error("GetImage and MIT-SHM captures differ")
	}
}

func mustCapture(t *testing.T, c *Conn) *Frame {
	t.Helper()
	f, _, err := c.Capture(true)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// rgb drops the undefined fourth byte of each pixel.
func rgb(pix []byte) []byte {
	out := make([]byte, 0, len(pix)/4*3)
	for i := 0; i+3 < len(pix); i += 4 {
		out = append(out, pix[i:i+3]...)
	}
	return out
}

func TestResize(t *testing.T) {
	tx := startX(t, "sleep 1000")
	c := tx.c
	modes := func() int {
		r, err := randr.GetScreenResourcesCurrent(tx.peer, tx.root).Reply()
		if err != nil {
			t.Fatal(err)
		}
		return len(r.Modes)
	}
	baseModes := modes()
	for _, tc := range []struct{ w, h, wantW, wantH int }{
		{1280, 720, 1280, 720},
		{1001, 777, 1000, 776},
		{640, 480, 640, 480},
		{1600, 300, 1600, 300}, // wider but shorter
		{300, 900, 300, 900},   // narrower but taller
		{1919, 1081, 1918, 1080},
		{4000, 4000, maxW, maxH},
		{1, 1, 2, 2},
		{maxW, maxH, maxW, maxH},
		{1366, 768, 1366, 768},
	} {
		gw, gh, err := c.Resize(tc.w, tc.h)
		if err != nil {
			t.Fatalf("Resize(%d, %d): %v", tc.w, tc.h, err)
		}
		if gw != tc.wantW || gh != tc.wantH {
			t.Errorf("Resize(%d, %d) = %dx%d, want %dx%d", tc.w, tc.h, gw, gh, tc.wantW, tc.wantH)
		}
		geo, err := xproto.GetGeometry(tx.peer, xproto.Drawable(tx.root)).Reply()
		if err != nil {
			t.Fatal(err)
		}
		if int(geo.Width) != gw || int(geo.Height) != gh {
			t.Errorf("after Resize(%d, %d) the root is %dx%d", tc.w, tc.h, geo.Width, geo.Height)
		}
		if w, h := c.Size(); w != gw || h != gh {
			t.Errorf("Size = %dx%d after resize to %dx%d", w, h, gw, gh)
		}
		f := mustCapture(t, c)
		if f.Width != gw || f.Height != gh {
			t.Errorf("capture is %dx%d after resize to %dx%d", f.Width, f.Height, gw, gh)
		}
		if n := modes(); n > baseModes+1 {
			t.Errorf("modes pile up: %d, started with %d", n, baseModes)
		}
	}
}

// TestExternalResize: another client switching modes must not break capture
// for more than one frame.
func TestExternalResize(t *testing.T) {
	tx := startX(t, "sleep 1000")
	other, err := Open(tx.d.Name, tx.d.XAuthority)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	mustCapture(t, tx.c)
	if _, _, err := other.Resize(800, 600); err != nil {
		t.Fatal(err)
	}
	var f *Frame
	for range 3 {
		if f, _, err = tx.c.Capture(true); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || f.Width != 800 || f.Height != 600 {
		t.Fatalf("capture after external resize: %v, %+v", err, f)
	}
}

func BenchmarkCapture1080p(b *testing.B) {
	for _, mode := range []string{"shm", "getimage"} {
		b.Run(mode, func(b *testing.B) {
			tx := startX(b, "sleep 1000")
			tx.c.cap.noSHM = mode == "getimage"
			b.SetBytes(maxW * maxH * 4)
			for b.Loop() {
				if _, _, err := tx.c.Capture(true); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
