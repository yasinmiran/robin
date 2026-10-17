//go:build linux

package x11

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"

	"github.com/yasinmiran/remotejump/internal/protocol"
)

// observer is a full-screen window on the peer connection that records the
// key and button events it receives.
type observer struct {
	tx     *testX
	win    xproto.Window
	events <-chan xgb.Event
}

func newObserver(t *testing.T, tx *testX) *observer {
	t.Helper()
	x := tx.peer
	win, _ := xproto.NewWindowId(x)
	mask := uint32(xproto.EventMaskKeyPress | xproto.EventMaskKeyRelease |
		xproto.EventMaskButtonPress | xproto.EventMaskButtonRelease | xproto.EventMaskStructureNotify)
	err := xproto.CreateWindowChecked(x, 0, win, tx.root, 0, 0, maxW, maxH, 0,
		xproto.WindowClassInputOutput, 0, xproto.CwEventMask, []uint32{mask}).Check()
	if err != nil {
		t.Fatal(err)
	}
	o := &observer{tx: tx, win: win, events: tx.events}
	xproto.MapWindow(x, win)
	o.waitFor(t, func(ev xgb.Event) bool { _, ok := ev.(xproto.MapNotifyEvent); return ok })
	if err := xproto.SetInputFocusChecked(x, xproto.InputFocusPointerRoot, win, xproto.TimeCurrentTime).Check(); err != nil {
		t.Fatal(err)
	}
	return o
}

func (o *observer) waitFor(t *testing.T, match func(xgb.Event) bool) {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case ev := <-o.events:
			if match(ev) {
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for an event")
		}
	}
}

// drain returns the events that arrive until the stream has been quiet for
// a moment, after a round trip so everything we sent has been processed.
func (o *observer) drain() []xgb.Event {
	xproto.GetInputFocus(o.tx.c.x).Reply()
	xproto.GetInputFocus(o.tx.peer).Reply()
	var evs []xgb.Event
	for {
		select {
		case ev := <-o.events:
			evs = append(evs, ev)
		case <-time.After(200 * time.Millisecond):
			return evs
		}
	}
}

// keyEvent is a key event decoded against the keymap the server has now.
type keyEvent struct {
	press bool
	sym   xproto.Keysym
	state uint16
}

func (k keyEvent) String() string {
	p := "up"
	if k.press {
		p = "down"
	}
	return fmt.Sprintf("%s %#x state %#x", p, k.sym, k.state)
}

func (o *observer) keys(t *testing.T) []keyEvent {
	t.Helper()
	var out []keyEvent
	for _, ev := range o.drain() {
		var code xproto.Keycode
		var state uint16
		var press bool
		switch e := ev.(type) {
		case xproto.KeyPressEvent:
			code, state, press = e.Detail, e.State, true
		case xproto.KeyReleaseEvent:
			code, state = e.Detail, e.State
		default:
			continue
		}
		out = append(out, keyEvent{press, o.keysym(t, code, state), state})
	}
	return out
}

// keysym resolves a keycode the way a core client does: column 1 with Shift,
// column 0 otherwise, an empty column 1 meaning the same as column 0.
func (o *observer) keysym(t *testing.T, code xproto.Keycode, state uint16) xproto.Keysym {
	r, err := xproto.GetKeyboardMapping(o.tx.peer, code, 1).Reply()
	if err != nil {
		t.Fatal(err)
	}
	col := 0
	if state&xproto.ModMaskShift != 0 && len(r.Keysyms) > 1 && r.Keysyms[1] != 0 {
		col = 1
	}
	return r.Keysyms[col]
}

// typedSyms returns the keysyms of the presses, ignoring modifiers.
func typedSyms(evs []keyEvent) []xproto.Keysym {
	var out []xproto.Keysym
	for _, e := range evs {
		if e.press && !isModifierSym(e.sym) {
			out = append(out, e.sym)
		}
	}
	return out
}

func isModifierSym(s xproto.Keysym) bool { return s >= 0xffe1 && s <= 0xffee || s == xkISOLevel3Shift }

func (tx *testX) input(t *testing.T, ms ...protocol.Control) {
	t.Helper()
	for _, m := range ms {
		if err := tx.c.Input(m); err != nil {
			t.Fatalf("Input(%+v): %v", m, err)
		}
	}
}

func key(k, code string, down bool) protocol.Control {
	return protocol.Control{T: protocol.ControlKey, Key: k, Code: code, Down: down}
}

func tap(k, code string) []protocol.Control {
	return []protocol.Control{key(k, code, true), key(k, code, false)}
}

// typeText sends each character as the browser would: down then up.
func typeText(tx *testX, t *testing.T, s string) {
	t.Helper()
	for _, r := range s {
		tx.input(t, tap(string(r), "")...)
	}
}

// assertNothingHeld checks no key, button or modifier is down on the server.
func (tx *testX) assertNothingHeld(t *testing.T) {
	t.Helper()
	km, err := xproto.QueryKeymap(tx.peer).Reply()
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range km.Keys {
		if b != 0 {
			t.Errorf("keys still down: byte %d = %08b", i, b)
		}
	}
	p, err := xproto.QueryPointer(tx.peer, tx.root).Reply()
	if err != nil {
		t.Fatal(err)
	}
	if p.Mask != 0 {
		t.Errorf("modifier/button mask %#x still set", p.Mask)
	}
}

func TestPointerMove(t *testing.T) {
	tx := startX(t, "sleep 1000")
	for _, tc := range []struct{ x, y, wantX, wantY int }{
		{100, 200, 100, 200},
		{1919, 1079, 1919, 1079},
		{-5, 99999, 0, maxH - 1},
	} {
		tx.input(t, protocol.Control{T: protocol.ControlMove, X: tc.x, Y: tc.y})
		p, err := xproto.QueryPointer(tx.peer, tx.root).Reply()
		if err != nil {
			t.Fatal(err)
		}
		if int(p.RootX) != tc.wantX || int(p.RootY) != tc.wantY {
			t.Errorf("move %d,%d: pointer at %d,%d, want %d,%d", tc.x, tc.y, p.RootX, p.RootY, tc.wantX, tc.wantY)
		}
	}
}

func TestButtonsAndWheel(t *testing.T) {
	tx := startX(t, "sleep 1000")
	o := newObserver(t, tx)
	tx.input(t, protocol.Control{T: protocol.ControlMove, X: 50, Y: 50})
	for b := range 5 {
		tx.input(t,
			protocol.Control{T: protocol.ControlButton, Button: b, Down: true},
			protocol.Control{T: protocol.ControlButton, Button: b, Down: false})
	}
	wheel := func(dx, dy float64) protocol.Control {
		return protocol.Control{T: protocol.ControlWheel, DX: dx, DY: dy}
	}
	// 250 -> two clicks down, 50 left; -60 -> -10, none; -100 -> -110, one up;
	// dx 100 -> one right; a huge delta is capped.
	tx.input(t, wheel(0, 250), wheel(0, -60), wheel(0, -100), wheel(100, 0), wheel(0, 1e12))
	var got []string
	for _, ev := range o.drain() {
		switch e := ev.(type) {
		case xproto.ButtonPressEvent:
			got = append(got, fmt.Sprint("+", e.Detail))
		case xproto.ButtonReleaseEvent:
			got = append(got, fmt.Sprint("-", e.Detail))
		}
	}
	want := "+1 -1 +2 -2 +3 -3 +8 -8 +9 -9 +5 -5 +5 -5 +4 -4 +7 -7" + strings.Repeat(" +5 -5", maxWheelClicks)
	if strings.Join(got, " ") != want {
		t.Errorf("buttons\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	if err := tx.c.Input(protocol.Control{T: protocol.ControlButton, Button: 7, Down: true}); err == nil {
		t.Error("button 7: want an error")
	}
	tx.assertNothingHeld(t)
}

func TestTyping(t *testing.T) {
	tx := startX(t, "sleep 1000")
	o := newObserver(t, tx)

	// Shifted ASCII, Latin-1 missing from the US map, and characters with no
	// legacy keysym at all.
	text := `Hello, World! ~{}|"<>?_+ æøå ÆØÅ € 😀 ✓`
	typeText(tx, t, text)
	evs := o.keys(t)
	var want []xproto.Keysym
	for _, r := range text {
		want = append(want, runeKeysym(r))
	}
	if got := typedSyms(evs); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("typed\n got %x\nwant %x", got, want)
	}
	for _, e := range evs {
		if e.state&^xproto.ModMaskShift != 0 {
			t.Errorf("unexpected modifiers on %v", e)
		}
	}
	tx.assertNothingHeld(t)
}

func TestShortcutsAndNamedKeys(t *testing.T) {
	tx := startX(t, "sleep 1000")
	o := newObserver(t, tx)
	ctrl := uint16(xproto.ModMaskControl)
	shift := uint16(xproto.ModMaskShift)

	for _, tc := range []struct {
		name  string
		input []protocol.Control
		sym   xproto.Keysym // the one non-modifier press expected
		state uint16        // its modifier state
	}{
		{"ctrl+c", seq(key("Control", "ControlLeft", true), tap("c", "KeyC"), key("Control", "ControlLeft", false)), 'c', ctrl},
		{"ctrl+shift+C", seq(key("Shift", "ShiftLeft", true), key("Control", "ControlRight", true), tap("C", "KeyC"),
			key("Control", "ControlRight", false), key("Shift", "ShiftLeft", false)), 'C', ctrl | shift},
		// German layout: Shift+7 is '/', which the US map has unshifted.
		{"shift held, unshifted char", seq(key("Shift", "ShiftLeft", true), tap("/", "Digit7"), key("Shift", "ShiftLeft", false)), '/', 0},
		// AltGr+e on a Norwegian layout: AltGr must not leak into the tap.
		{"altgr+e", seq(key("AltGraph", "AltRight", true), tap("€", "KeyE"), key("AltGraph", "AltRight", false)), runeKeysym('€'), 0},
		{"enter", tap("Enter", "Enter"), 0xff0d, 0},
		{"F13", tap("F13", "F13"), 0xffca, 0},
		{"alt+tab", seq(key("Alt", "AltLeft", true), tap("Tab", "Tab"), key("Alt", "AltLeft", false)), 0xff09, xproto.ModMask1},
	} {
		tx.input(t, tc.input...)
		evs := o.keys(t)
		var presses []keyEvent
		for _, e := range evs {
			if e.press && !isModifierSym(e.sym) {
				presses = append(presses, e)
			}
		}
		if len(presses) != 1 || presses[0].sym != tc.sym || presses[0].state != tc.state {
			t.Errorf("%s: got %v, want one press of %#x with state %#x", tc.name, evs, tc.sym, tc.state)
		}
		tx.assertNothingHeld(t)
	}

	// Browser autorepeat: a named key presses again, a character taps again.
	tx.input(t, key("Backspace", "Backspace", true))
	rep := key("Backspace", "Backspace", true)
	rep.Repeat = true
	tx.input(t, rep, rep, key("Backspace", "Backspace", false))
	rc := key("x", "KeyX", true)
	rc.Repeat = true
	tx.input(t, key("x", "KeyX", true), rc, key("x", "KeyX", false))
	var got []string
	for _, e := range o.keys(t) {
		got = append(got, e.String())
	}
	want := "down 0xff08 state 0x0,up 0xff08 state 0x0,down 0xff08 state 0x0,up 0xff08 state 0x0," +
		"down 0xff08 state 0x0,up 0xff08 state 0x0," +
		"down 0x78 state 0x0,up 0x78 state 0x0,down 0x78 state 0x0,up 0x78 state 0x0"
	if strings.Join(got, ",") != want {
		t.Errorf("repeat\n got %s\nwant %s", strings.Join(got, ","), want)
	}

	// Keys the browser cannot name, and the locks it already applied, do nothing.
	tx.input(t, seq(tap("Dead", "BracketLeft"), tap("Unidentified", ""), tap("CapsLock", "CapsLock"), tap("NumLock", "NumLock"))...)
	if evs := o.keys(t); len(evs) != 0 {
		t.Errorf("ignored keys produced %v", evs)
	}

	// Release lets go of everything a vanished browser was holding.
	tx.input(t, key("Shift", "ShiftLeft", true), key("Control", "ControlLeft", true), key("a", "KeyA", true),
		key("ArrowDown", "ArrowDown", true), protocol.Control{T: protocol.ControlButton, Button: 0, Down: true},
		protocol.Control{T: protocol.ControlRelease})
	tx.assertNothingHeld(t)
}

// TestHeldKeysBounded proves a peer cannot exhaust the agent's memory by
// sending key-down events with unique codes and never the matching key-up: the
// held-key map must stay bounded however many arrive.
func TestHeldKeysBounded(t *testing.T) {
	tx := startX(t, "sleep 1000")
	for i := 0; i < maxHeldKeys*8; i++ {
		tx.input(t, key("Shift", fmt.Sprintf("Flood%d", i), true))
	}
	tx.c.kb.mu.Lock()
	held := len(tx.c.kb.held)
	tx.c.kb.mu.Unlock()
	if held > maxHeldKeys {
		t.Fatalf("held keys = %d after a flood of unique codes, want <= %d", held, maxHeldKeys)
	}
	tx.input(t, protocol.Control{T: protocol.ControlRelease})
	tx.assertNothingHeld(t)
}

// TestServerAutorepeat documents whether the server repeats keys we hold:
// it must not, or browser repeats would come on top.
func TestServerAutorepeat(t *testing.T) {
	tx := startX(t, "sleep 1000")
	o := newObserver(t, tx)
	tx.input(t, key("ArrowDown", "ArrowDown", true))
	time.Sleep(1500 * time.Millisecond)
	tx.input(t, key("ArrowDown", "ArrowDown", false))
	presses := 0
	for _, e := range o.keys(t) {
		if e.press {
			presses++
		}
	}
	if presses != 1 {
		t.Errorf("holding a key for 1.5 s gave %d presses, want 1", presses)
	}

	// Close puts per-key autorepeat back the way Open found it.
	found := tx.c.kb.repeats
	tx.c.Close()
	r, err := xproto.GetKeyboardControl(tx.peer).Reply()
	if err != nil {
		t.Fatal(err)
	}
	if found == ([32]byte{}) || !bytes.Equal(r.AutoRepeats, found[:]) {
		t.Errorf("autorepeat after Close %x, want %x", r.AutoRepeats, found)
	}
}

// TestTypingIntoXterm checks the text a real Xlib client sees, which goes
// through XKB rather than our core decoding.
func TestTypingIntoXterm(t *testing.T) {
	if _, err := exec.LookPath("xterm"); err != nil {
		t.Skip("no xterm")
	}
	tx := startX(t, "sleep 1000")
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	xt := exec.Command("xterm", "-geometry", "80x10+0+0", "-e", "sh", "-c",
		`stty -echo; touch "$0.ready"; IFS= read -r line; printf %s "$line" > "$0"`, out)
	xt.Env = append(os.Environ(), "DISPLAY="+tx.d.Name, "XAUTHORITY="+tx.d.XAuthority, "LANG=C.UTF-8")
	if err := xt.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { xt.Process.Kill(); xt.Wait() })
	waitFile(t, out+".ready")
	time.Sleep(300 * time.Millisecond)
	tx.input(t, protocol.Control{T: protocol.ControlMove, X: 100, Y: 50}) // no WM: focus follows the pointer

	text := `echo "Ünïcödé" æøå ÆØÅ €½ 😀 {~}`
	typeText(tx, t, text)
	tx.input(t, tap("Enter", "Enter")...)
	waitFile(t, out)
	b, _ := os.ReadFile(out)
	if string(b) != text {
		t.Errorf("xterm read %q, want %q", b, text)
	}
	tx.assertNothingHeld(t)
}

func waitFile(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if fi, err := os.Stat(path); err == nil && (fi.Size() > 0 || strings.HasSuffix(path, ".ready")) {
			return
		}
	}
	t.Fatalf("%s did not appear", path)
}

func seq(parts ...any) []protocol.Control {
	var out []protocol.Control
	for _, p := range parts {
		switch v := p.(type) {
		case protocol.Control:
			out = append(out, v)
		case []protocol.Control:
			out = append(out, v...)
		}
	}
	return out
}
