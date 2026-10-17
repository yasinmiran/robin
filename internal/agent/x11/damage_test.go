//go:build linux

package x11

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/damage"
	"github.com/jezek/xgb/xproto"
)

// window maps a window of colour at x,y on the peer connection.
func window(t *testing.T, tx *testX, x, y int16, colour uint32) xproto.Window {
	t.Helper()
	win, _ := xproto.NewWindowId(tx.peer)
	err := xproto.CreateWindowChecked(tx.peer, 0, win, tx.root, x, y, 200, 100, 0, xproto.WindowClassInputOutput, 0,
		xproto.CwBackPixel|xproto.CwOverrideRedirect, []uint32{colour, 1}).Check()
	if err != nil {
		t.Fatal(err)
	}
	xproto.MapWindow(tx.peer, win)
	xproto.GetInputFocus(tx.peer).Reply()
	return win
}

// repaint gives a mapped window a new background colour.
func repaint(tx *testX, win xproto.Window, colour uint32) {
	xproto.ChangeWindowAttributes(tx.peer, win, xproto.CwBackPixel, []uint32{colour})
	xproto.ClearArea(tx.peer, false, win, 0, 0, 0, 0)
	xproto.GetInputFocus(tx.peer).Reply()
}

// awaitPixel captures until one reports a change and shows bgr at x,y.
func awaitPixel(t *testing.T, c *Conn, x, y int, bgr [3]byte, within time.Duration, what string) {
	t.Helper()
	var last []byte
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		f, changed, err := c.Capture(false)
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			last = f.Pix[y*f.Stride+4*x : y*f.Stride+4*x+3]
			if bytes.Equal(last, bgr[:]) {
				return
			}
		}
	}
	t.Fatalf("%s: no capture showed %v at %d,%d within %v (last change showed %v)", what, bgr, x, y, within, last)
}

// TestDamageMissedFallsBackToComparing stands in for a compositor whose
// drawing DAMAGE never reports, by destroying our DAMAGE object.
func TestDamageMissedFallsBackToComparing(t *testing.T) {
	tx := startX(t, "sleep 1000")
	c := tx.c
	c.cap.mu.Lock()
	c.cap.checkEvery = 300 * time.Millisecond
	c.cap.mu.Unlock()
	waitIdle(t, c)

	// While DAMAGE works, rechecking an idle screen finds nothing.
	time.Sleep(400 * time.Millisecond)
	if _, changed, _ := c.Capture(false); changed || c.cap.polling {
		t.Fatalf("recheck of an idle screen: changed %v, polling %v", changed, c.cap.polling)
	}

	damage.Destroy(c.x, c.damage)
	win := window(t, tx, 500, 500, 0xff0000)
	awaitPixel(t, c, 600, 550, [3]byte{0, 0, 0xff}, 2*time.Second, "red window, found by a recheck")
	if !c.cap.polling {
		t.Fatal("a change DAMAGE missed did not switch to comparing frames")
	}
	// From now on every capture compares, so a change shows up at once and
	// an unchanged screen still reports nothing.
	repaint(tx, win, 0x00ff00)
	awaitPixel(t, c, 600, 550, [3]byte{0, 0xff, 0}, 200*time.Millisecond, "green repaint while comparing")
	if _, changed, _ := c.Capture(false); changed {
		t.Error("an unchanged screen reported a change while comparing")
	}
}

// TestDamageUnderCompositor checks that DAMAGE on the root window hears
// about drawing in windows a compositing manager redirects, so idle
// screens stay free under a compositor.
func TestDamageUnderCompositor(t *testing.T) {
	for _, argv := range [][]string{
		{"xcompmgr"},
		{"picom", "--backend", "xrender", "--config", "/dev/null"},
	} {
		t.Run(argv[0], func(t *testing.T) {
			if _, err := exec.LookPath(argv[0]); err != nil {
				t.Skipf("no %s", argv[0])
			}
			tx := startX(t, "sleep 1000")
			cm := exec.Command(argv[0], argv[1:]...)
			cm.Env = append(os.Environ(), "DISPLAY="+tx.d.Name, "XAUTHORITY="+tx.d.XAuthority)
			var stderr bytes.Buffer
			cm.Stderr = &stderr
			if err := cm.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cm.Process.Kill(); cm.Wait() })
			awaitCompositor(t, tx.peer, &stderr)

			c := tx.c
			c.cap.mu.Lock()
			c.cap.checkEvery = time.Hour // only DAMAGE may report a change
			c.cap.mu.Unlock()
			waitIdle(t, c)
			win := window(t, tx, 500, 500, 0xff0000)
			awaitPixel(t, c, 600, 550, [3]byte{0, 0, 0xff}, 3*time.Second, "new redirected window")
			waitIdle(t, c)
			repaint(tx, win, 0x00ff00)
			awaitPixel(t, c, 600, 550, [3]byte{0, 0xff, 0}, 3*time.Second, "repaint inside a redirected window")
		})
	}
}

func awaitCompositor(t *testing.T, x *xgb.Conn, stderr *bytes.Buffer) {
	t.Helper()
	name := "_NET_WM_CM_S0"
	a, err := xproto.InternAtom(x, false, uint16(len(name)), name).Reply()
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if r, err := xproto.GetSelectionOwner(x, a.Atom).Reply(); err == nil && r.Owner != 0 {
			return
		}
	}
	t.Fatalf("no compositing manager took %s; its stderr:\n%s", name, stderr.String())
}
