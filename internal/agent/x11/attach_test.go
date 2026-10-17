//go:build linux

package x11

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"

	"github.com/yasinmiran/remotejump/internal/protocol"
)

// onDisplay runs a command against the test display.
func onDisplay(t *testing.T, tx *testX, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "DISPLAY="+tx.d.Name, "XAUTHORITY="+tx.d.XAuthority)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return string(out)
}

func TestAttach(t *testing.T) {
	tx := startX(t, "sleep 1000")
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// A file without a cookie for the display is passed over.
	c, used, err := Attach(tx.d.Name, []string{empty, tx.d.XAuthority})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if used != tx.d.XAuthority {
		t.Errorf("attached with %q, want %q", used, tx.d.XAuthority)
	}

	// No cookie at all is refused, until the user gets host-based access the
	// way xhost +si:localuser grants it.
	if _, _, err := Attach(tx.d.Name, []string{empty}); err == nil || !strings.Contains(err.Error(), "without a cookie") {
		t.Fatalf("attach with no cookie: err %v", err)
	}
	if _, err := exec.LookPath("xhost"); err != nil {
		t.Skip("no xhost")
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	onDisplay(t, tx, "xhost", "+si:localuser:"+me.Username)
	c, used, err = Attach(tx.d.Name, []string{empty})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if used != "" {
		t.Errorf("attached with %q, want no cookie", used)
	}

	if _, _, err := Attach(":4242", nil); err == nil || !strings.Contains(err.Error(), "no X server") {
		t.Errorf("attach to a display nobody runs: err %v", err)
	}
}

// keyboardState is everything about the keyboard an attached session may
// touch, as another client sees it.
func keyboardState(t *testing.T, tx *testX) string {
	t.Helper()
	x := tx.peer
	setup := xproto.Setup(x)
	kc, err := xproto.GetKeyboardControl(x).Reply()
	if err != nil {
		t.Fatal(err)
	}
	km, err := xproto.GetKeyboardMapping(x, setup.MinKeycode, byte(int(setup.MaxKeycode)-int(setup.MinKeycode)+1)).Reply()
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("repeat %d %x\nkeymap %d %v\n%s", kc.GlobalAutoRepeat, kc.AutoRepeats,
		km.KeysymsPerKeycode, km.Keysyms, onDisplay(t, tx, "xkbcomp", "-xkb", tx.d.Name, "-"))
}

func TestAttachLeavesTheDesktopAsFound(t *testing.T) {
	tx := startX(t, "sleep 1000")
	before := keyboardState(t, tx)

	// Watching changes nothing at all.
	c, _, err := Attach(tx.d.Name, []string{tx.d.XAuthority})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Capture(true); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if after := keyboardState(t, tx); after != before {
		t.Fatalf("a view-only session changed the keyboard:\n%s", lineDiff(before, after))
	}

	// Typing switches autorepeat off and binds a spare keycode for a
	// character the keymap lacks; Close puts both back.
	c, _, err = Attach(tx.d.Name, []string{tx.d.XAuthority})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []protocol.Control{key("ж", "KeyX", true), key("ж", "KeyX", false), key("Shift", "ShiftLeft", true)} {
		if err := c.Input(m); err != nil {
			t.Fatal(err)
		}
	}
	xproto.GetInputFocus(c.x).Reply()
	if during := keyboardState(t, tx); during == before {
		t.Fatal("typing changed neither autorepeat nor the keymap; the restore below proves nothing")
	}
	c.Close()
	if after := keyboardState(t, tx); after != before {
		t.Fatalf("keyboard not restored after typing:\n%s", lineDiff(before, after))
	}
}

func lineDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	var out []string
	for i := range max(len(al), len(bl)) {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			out = append(out, fmt.Sprintf("line %d:\n- %.200s\n+ %.200s", i+1, x, y))
		}
	}
	return strings.Join(out, "\n")
}

func TestCloseHandsClipboardToManager(t *testing.T) {
	tx := startX(t, "sleep 1000")
	x := tx.peer
	atom := func(name string) xproto.Atom {
		r, err := xproto.InternAtom(x, false, uint16(len(name)), name).Reply()
		if err != nil {
			t.Fatal(err)
		}
		return r.Atom
	}
	manager, saveTargets, clipboard, utf8, prop := atom("CLIPBOARD_MANAGER"), atom("SAVE_TARGETS"), atom("CLIPBOARD"), atom("UTF8_STRING"), atom("RJ_TEST_SAVED")

	// Without a clipboard manager Close does not wait for one.
	c, _, err := Attach(tx.d.Name, []string{tx.d.XAuthority})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetClipboard("gone"); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	c.Close()
	if d := time.Since(t0); d > handOffTimeout/2 {
		t.Errorf("Close took %v with no clipboard manager", d)
	}

	mgr, _ := xproto.NewWindowId(x)
	if err := xproto.CreateWindowChecked(x, 0, mgr, tx.root, 0, 0, 1, 1, 0, xproto.WindowClassInputOnly, 0, 0, nil).Check(); err != nil {
		t.Fatal(err)
	}
	xproto.SetSelectionOwner(x, mgr, manager, xproto.TimeCurrentTime)
	if r, err := xproto.GetSelectionOwner(x, manager).Reply(); err != nil || r.Owner != mgr {
		t.Fatalf("could not play the clipboard manager: %v", err)
	}
	if err := tx.c.SetClipboard("kept after close ✓"); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() { tx.c.Close(); close(closed) }()

	next := func(match func(xgb.Event) bool) xgb.Event {
		for timeout := time.After(3 * time.Second); ; {
			select {
			case ev := <-tx.events:
				if match(ev) {
					return ev
				}
			case <-timeout:
				t.Fatal("timed out waiting for the clipboard owner")
			}
		}
	}
	req := next(func(ev xgb.Event) bool {
		e, ok := ev.(xproto.SelectionRequestEvent)
		return ok && e.Selection == manager && e.Target == saveTargets
	}).(xproto.SelectionRequestEvent)
	xproto.ConvertSelection(x, mgr, clipboard, utf8, prop, req.Time)
	next(func(ev xgb.Event) bool { e, ok := ev.(xproto.SelectionNotifyEvent); return ok && e.Requestor == mgr })
	got, err := xproto.GetProperty(x, true, mgr, prop, xproto.GetPropertyTypeAny, 0, 1<<16).Reply()
	if err != nil {
		t.Fatal(err)
	}
	done := xproto.SelectionNotifyEvent{Time: req.Time, Requestor: req.Requestor, Selection: manager, Target: saveTargets, Property: req.Property}
	xproto.SendEvent(x, false, req.Requestor, xproto.EventMaskNoEvent, string(done.Bytes()))
	xproto.GetInputFocus(x).Reply()

	select {
	case <-closed:
	case <-time.After(handOffTimeout / 2):
		t.Error("Close kept waiting after the clipboard manager answered")
	}
	if string(got.Value) != "kept after close ✓" {
		t.Errorf("the clipboard manager saved %q", got.Value)
	}
}

// TestAttachedAutorepeat: an attached session stops server autorepeat with
// the global switch, and switches it off again if the desktop turns it on.
func TestAttachedAutorepeat(t *testing.T) {
	tx := startX(t, "sleep 1000")
	o := newObserver(t, tx)
	c, _, err := Attach(tx.d.Name, []string{tx.d.XAuthority})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	hold := func(what string) {
		t.Helper()
		if err := c.Input(key("ArrowDown", "ArrowDown", true)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1500 * time.Millisecond)
		if err := c.Input(key("ArrowDown", "ArrowDown", false)); err != nil {
			t.Fatal(err)
		}
		xproto.GetInputFocus(c.x).Reply()
		presses := 0
		for _, e := range o.keys(t) {
			if e.press {
				presses++
			}
		}
		if presses != 1 {
			t.Errorf("%s: holding a key for 1.5 s gave %d presses, want 1", what, presses)
		}
	}
	hold("attached")
	xproto.ChangeKeyboardControl(tx.peer, xproto.KbAutoRepeatMode, []uint32{xproto.AutoRepeatModeOn})
	xproto.GetInputFocus(tx.peer).Reply()
	hold("after the desktop turned autorepeat back on")
}
