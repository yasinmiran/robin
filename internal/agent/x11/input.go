//go:build linux

package x11

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"

	"github.com/yasinmiran/remotejump/internal/protocol"
)

const (
	wheelStep      = 100.0 // browser pixels per wheel click
	maxWheelClicks = 20    // per message, so a bogus delta cannot stall input
	maxSpareKeys   = 16
	// maxHeldKeys bounds the keys tracked as held at once. No real keyboard
	// holds this many; the cap stops a peer that sends key-down events with
	// unique codes and never the matching key-up from growing the map without
	// bound.
	maxHeldKeys = 64
)

// keyboard is the input state: the server's keymap as last read, the keys
// and buttons we hold, and the spare keycodes we bind keysyms to.
type keyboard struct {
	mu    sync.Mutex
	x     *xgb.Conn
	root  xproto.Window
	stale atomic.Bool // set on MappingNotify; reload before the next lookup

	min, max xproto.Keycode
	perCode  int
	syms     []xproto.Keysym // perCode entries per keycode from min

	spares  []spare
	clock   uint64
	repeats [32]byte           // per-key autorepeat as we found it, restored on close
	held    map[string]heldKey // by KeyboardEvent.code
	buttons map[byte]bool
	wheelX  float64
	wheelY  float64
}

type heldKey struct {
	code xproto.Keycode
	sym  xproto.Keysym
}

type spare struct {
	code     xproto.Keycode
	sym      xproto.Keysym // currently bound, 0 when free
	lastUsed uint64
}

func (k *keyboard) init(x *xgb.Conn, setup *xproto.SetupInfo) error {
	k.x = x
	k.root = setup.DefaultScreen(x).Root
	k.min, k.max = setup.MinKeycode, setup.MaxKeycode
	k.held = map[string]heldKey{}
	k.buttons = map[byte]bool{}
	k.stale.Store(true)
	return k.disableAutorepeat()
}

// disableAutorepeat stops the server repeating held keys: the browser sends
// its own repeats, and a server repeating through a network stall turns one
// Backspace into many. It is switched off per key, which survives a desktop
// turning the global switch back on.
func (k *keyboard) disableAutorepeat() error {
	r, err := xproto.GetKeyboardControl(k.x).Reply()
	if err != nil {
		return fmt.Errorf("read keyboard control: %w", err)
	}
	copy(k.repeats[:], r.AutoRepeats)
	k.setAutorepeat(func(xproto.Keycode) bool { return true }, xproto.AutoRepeatModeOff)
	return nil
}

func (k *keyboard) setAutorepeat(keys func(xproto.Keycode) bool, mode uint32) {
	for code := int(k.min); code <= int(k.max); code++ {
		if keys(xproto.Keycode(code)) {
			xproto.ChangeKeyboardControl(k.x, xproto.KbKey|xproto.KbAutoRepeatMode, []uint32{uint32(code), mode})
		}
	}
}

// Input injects one browser input event. Key events follow the browser's
// KeyboardEvent: printable keys are typed as the character they produce,
// whatever the keymap, and named keys are held from down to up.
func (c *Conn) Input(m protocol.Control) error {
	k := &c.kb
	k.mu.Lock()
	defer k.mu.Unlock()
	switch m.T {
	case protocol.ControlKey:
		return k.key(m)
	case protocol.ControlMove:
		w, h := c.Size()
		x, y := min(max(m.X, 0), w-1), min(max(m.Y, 0), h-1)
		xtest.FakeInput(k.x, xproto.MotionNotify, 0, 0, k.root, int16(x), int16(y), 0)
		return nil
	case protocol.ControlButton:
		return k.button(m.Button, m.Down)
	case protocol.ControlWheel:
		k.wheel(m.DX, m.DY)
		return nil
	case protocol.ControlRelease:
		k.releaseAll()
		return nil
	}
	return fmt.Errorf("x11: not an input event: %q", m.T)
}

func (k *keyboard) key(m protocol.Control) error {
	if r, n := utf8.DecodeRuneInString(m.Key); n == len(m.Key) && r != utf8.RuneError {
		if !m.Down || r < 0x20 || r == 0x7f {
			return nil // typed on key-down; nothing is held
		}
		return k.typeRune(r)
	}
	sym, ok := namedKeysym(m.Key, m.Code)
	if !ok {
		return nil // Dead, Unidentified, locks, keys we do not map
	}
	id := m.Code
	if id == "" {
		id = m.Key
	}
	if !m.Down {
		if h, ok := k.held[id]; ok {
			delete(k.held, id)
			k.fake(xproto.KeyRelease, byte(h.code))
		}
		return nil
	}
	if h, ok := k.held[id]; ok {
		// Browser autorepeat, or a key-up we never got. The server drops a
		// press of a key that is down, so repeat the way X autorepeat does.
		k.fake(xproto.KeyRelease, byte(h.code))
		k.fake(xproto.KeyPress, byte(h.code))
		return nil
	}
	if len(k.held) >= maxHeldKeys {
		return nil // a well-behaved browser never holds this many; ignore the flood
	}
	code, _, err := k.keycodeFor(sym)
	if err != nil {
		return err
	}
	k.held[id] = heldKey{code, sym}
	k.fake(xproto.KeyPress, byte(code))
	return nil
}

// typeRune taps the key that produces r. Shift and AltGr are set to what that
// key's level needs for the tap only; every other held modifier stays down,
// so Ctrl+C arrives as Ctrl+C.
func (k *keyboard) typeRune(r rune) error {
	code, level, err := k.keycodeFor(runeKeysym(r))
	if err != nil {
		return err
	}
	var lift []xproto.Keycode // held keys to let go of around the tap
	shiftHeld := false
	for _, h := range k.held {
		switch {
		case isShift(h.sym):
			shiftHeld = true
			if level == 0 {
				lift = append(lift, h.code)
			}
		case isLevel3(h.sym):
			lift = append(lift, h.code)
		}
	}
	var shift xproto.Keycode
	if level == 1 && !shiftHeld {
		if shift, _, err = k.keycodeFor(xkShiftL); err != nil {
			return err
		}
	}

	for _, kc := range lift {
		k.fake(xproto.KeyRelease, byte(kc))
	}
	if shift != 0 {
		k.fake(xproto.KeyPress, byte(shift))
	}
	k.fake(xproto.KeyPress, byte(code))
	k.fake(xproto.KeyRelease, byte(code))
	if shift != 0 {
		k.fake(xproto.KeyRelease, byte(shift))
	}
	for _, kc := range lift {
		k.fake(xproto.KeyPress, byte(kc))
	}
	return nil
}

// keycodeFor returns a keycode and shift level (0 or 1) producing sym,
// binding sym to a spare keycode when the keymap has no such key.
func (k *keyboard) keycodeFor(sym xproto.Keysym) (xproto.Keycode, int, error) {
	if err := k.refresh(); err != nil {
		return 0, 0, err
	}
	// Columns 0 and 1 are group 1 without and with Shift. Deeper levels need
	// modifiers whose meaning depends on the key's XKB type, which the core
	// protocol does not show, so those keysyms take a spare keycode instead.
	for level := range min(2, k.perCode) {
		for i := level; i < len(k.syms); i += k.perCode {
			if k.syms[i] == sym {
				code := k.min + xproto.Keycode(i/k.perCode)
				k.touch(code)
				return code, level, nil
			}
		}
	}
	code, err := k.bind(sym)
	return code, 0, err
}

// touch marks a spare keycode as just used, so the pool keeps it bound.
func (k *keyboard) touch(code xproto.Keycode) {
	for i := range k.spares {
		if k.spares[i].code == code {
			k.clock++
			k.spares[i].lastUsed = k.clock
		}
	}
}

// bind maps sym onto the least recently used spare keycode that is not held.
// Reusing a keycode soon after a tap would race clients that read the new
// mapping before processing the old key event, hence the pool.
func (k *keyboard) bind(sym xproto.Keysym) (xproto.Keycode, error) {
	best := -1
	for i, s := range k.spares {
		if k.isHeld(s.code) {
			continue
		}
		if best < 0 || s.lastUsed < k.spares[best].lastUsed {
			best = i
		}
	}
	if best < 0 {
		return 0, fmt.Errorf("x11: no spare keycode for keysym %#x", sym)
	}
	s := &k.spares[best]
	// The same keysym on both levels keeps Shift from changing it.
	err := xproto.ChangeKeyboardMappingChecked(k.x, 1, s.code, 2, []xproto.Keysym{sym, sym}).Check()
	if err != nil {
		return 0, fmt.Errorf("x11: bind keysym %#x: %w", sym, err)
	}
	k.clock++
	s.sym, s.lastUsed = sym, k.clock
	k.setSyms(s.code, sym, sym)
	return s.code, nil
}

func (k *keyboard) isHeld(code xproto.Keycode) bool {
	for _, h := range k.held {
		if h.code == code {
			return true
		}
	}
	return false
}

// refresh reloads the keymap after a MappingNotify, and on first use picks
// the spare keycodes: those with no keysym at all.
func (k *keyboard) refresh() error {
	if !k.stale.Swap(false) {
		return nil
	}
	r, err := xproto.GetKeyboardMapping(k.x, k.min, byte(int(k.max)-int(k.min)+1)).Reply()
	if err != nil {
		k.stale.Store(true)
		return fmt.Errorf("x11: read keymap: %w", err)
	}
	k.perCode, k.syms = int(r.KeysymsPerKeycode), r.Keysyms
	if k.spares != nil || k.perCode == 0 {
		return nil
	}
	for code := int(k.max); code >= int(k.min) && len(k.spares) < maxSpareKeys; code-- {
		i := (code - int(k.min)) * k.perCode
		empty := true
		for _, s := range k.syms[i : i+k.perCode] {
			empty = empty && s == 0
		}
		if empty {
			k.spares = append(k.spares, spare{code: xproto.Keycode(code)})
		}
	}
	return nil
}

// setSyms records a binding locally so lookups see it before MappingNotify.
func (k *keyboard) setSyms(code xproto.Keycode, syms ...xproto.Keysym) {
	i := int(code-k.min) * k.perCode
	for j := range k.perCode {
		var s xproto.Keysym
		if j < len(syms) {
			s = syms[j]
		}
		k.syms[i+j] = s
	}
}

func (k *keyboard) button(browser int, down bool) error {
	b, ok := xButton(browser)
	if !ok {
		return fmt.Errorf("x11: unsupported mouse button %d", browser)
	}
	if down {
		k.buttons[b] = true
		k.fake(xproto.ButtonPress, b)
	} else if k.buttons[b] {
		delete(k.buttons, b)
		k.fake(xproto.ButtonRelease, b)
	}
	return nil
}

// xButton maps MouseEvent.button to an X button: main, middle, secondary,
// then back and forward.
func xButton(browser int) (byte, bool) {
	switch browser {
	case 0, 1, 2:
		return byte(browser + 1), true
	case 3, 4:
		return byte(browser + 5), true
	}
	return 0, false
}

// wheel turns pixel deltas into clicks, one per wheelStep, carrying the
// remainder so slow trackpad scrolls still add up.
func (k *keyboard) wheel(dx, dy float64) {
	k.wheelX += dx
	k.wheelY += dy
	k.wheelY = k.clicks(k.wheelY, 4, 5)
	k.wheelX = k.clicks(k.wheelX, 6, 7)
}

func (k *keyboard) clicks(acc float64, neg, pos byte) float64 {
	n := math.Trunc(acc / wheelStep)
	rest := acc - n*wheelStep
	b := pos
	if n < 0 {
		b, n = neg, -n
	}
	for range int(min(n, maxWheelClicks)) {
		k.fake(xproto.ButtonPress, b)
		k.fake(xproto.ButtonRelease, b)
	}
	return rest
}

// releaseAll lets go of every key and button we hold.
func (k *keyboard) releaseAll() {
	for id, h := range k.held {
		delete(k.held, id)
		k.fake(xproto.KeyRelease, byte(h.code))
	}
	for b := range k.buttons {
		delete(k.buttons, b)
		k.fake(xproto.ButtonRelease, b)
	}
	k.wheelX, k.wheelY = 0, 0
}

// close releases everything and puts back the keymap and autorepeat we
// changed.
func (k *keyboard) close() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.releaseAll()
	for _, s := range k.spares {
		if s.sym != 0 {
			xproto.ChangeKeyboardMapping(k.x, 1, s.code, 1, []xproto.Keysym{0})
		}
	}
	k.setAutorepeat(func(c xproto.Keycode) bool { return k.repeats[c/8]&(1<<(c%8)) != 0 }, xproto.AutoRepeatModeOn)
}

// fake sends one XTEST event. It is unchecked: a failure surfaces as an
// error event, and the server processes requests in order, so keymap
// changes made just before are in effect.
func (k *keyboard) fake(typ, detail byte) {
	xtest.FakeInput(k.x, typ, detail, 0, k.root, 0, 0, 0)
}
