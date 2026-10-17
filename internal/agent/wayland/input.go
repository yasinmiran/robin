//go:build linux

package wayland

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"unicode/utf8"

	"github.com/yasinmiran/remotejump/internal/agent/keysym"
	"github.com/yasinmiran/remotejump/internal/protocol"
)

// xkVoidSymbol is on no layout, so a compositor that looks it up drops it
// without delivering anything.
const xkVoidSymbol = 0xffffff

const (
	wheelStep     = 100.0 // browser pixels per wheel step
	maxWheelSteps = 20    // per message, so a bogus delta cannot flood the desktop
	// maxHeldKeys bounds the keys tracked as held at once, so a page that
	// sends key-downs with unique codes and no key-ups cannot grow the map.
	maxHeldKeys = 64
)

// Pointer buttons as evdev codes, from <linux/input-event-codes.h>.
const (
	btnLeft   = 0x110
	btnRight  = 0x111
	btnMiddle = 0x112
	btnSide   = 0x113
	btnExtra  = 0x114
)

const (
	axisVertical   = uint32(0)
	axisHorizontal = uint32(1)
)

var errViewOnly = errors.New("view only: the host did not allow remote interaction")

// input turns browser events into the portal's Notify calls.
type input struct {
	s        *portalSession
	viewOnly bool
	// scaleX and scaleY take frame pixels to stream coordinates; maxX and
	// maxY are the stream's last pixel.
	scaleX, scaleY float64
	maxX, maxY     float64

	mu      sync.Mutex
	held    map[string]uint32 // keysym by KeyboardEvent.code
	buttons map[int32]bool
	wheelX  float64
	wheelY  float64
}

func newInput(s *portalSession, frameW, frameH int) *input {
	in := &input{
		s: s, viewOnly: s.viewOnly(), scaleX: 1, scaleY: 1,
		maxX: float64(frameW - 1), maxY: float64(frameH - 1),
		held: map[string]uint32{}, buttons: map[int32]bool{},
	}
	if s.width > 0 && s.height > 0 {
		in.scaleX = float64(s.width) / float64(frameW)
		in.scaleY = float64(s.height) / float64(frameH)
		in.maxX, in.maxY = float64(s.width-1), float64(s.height-1)
	}
	return in
}

// warmUp sends a key no layout has and a pointer motion of nothing. GNOME
// drops the first key event of a remote desktop session, and at times the
// first pointer event, and this way the ones it drops are not the user's.
func (in *input) warmUp() error {
	if in.viewOnly {
		return nil
	}
	if err := in.keysym(xkVoidSymbol, true); err != nil {
		return err
	}
	if err := in.keysym(xkVoidSymbol, false); err != nil {
		return err
	}
	return in.s.call("NotifyPointerMotion", 0.0, 0.0)
}

func (in *input) apply(m protocol.Control) error {
	if in.viewOnly {
		if m.T == protocol.ControlRelease {
			return nil
		}
		return errViewOnly
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	switch m.T {
	case protocol.ControlKey:
		return in.key(m)
	case protocol.ControlMove:
		x := min(max(float64(m.X)*in.scaleX, 0), in.maxX)
		y := min(max(float64(m.Y)*in.scaleY, 0), in.maxY)
		return in.s.call("NotifyPointerMotionAbsolute", in.s.node, x, y)
	case protocol.ControlButton:
		return in.button(m.Button, m.Down)
	case protocol.ControlWheel:
		return in.wheel(m.DX, m.DY)
	case protocol.ControlRelease:
		return in.releaseAll()
	}
	return fmt.Errorf("wayland: not an input event: %q", m.T)
}

// key types printable characters as the keysym they produce, whatever the
// layout, and holds named keys from down to up.
func (in *input) key(m protocol.Control) error {
	if r, n := utf8.DecodeRuneInString(m.Key); n == len(m.Key) && r != utf8.RuneError {
		if !m.Down || r < 0x20 || r == 0x7f {
			return nil // typed on key-down; nothing is held
		}
		sym := keysym.Rune(r)
		if err := in.keysym(sym, true); err != nil {
			return err
		}
		return in.keysym(sym, false)
	}
	sym, ok := keysym.Named(m.Key, m.Code)
	if !ok {
		return nil // Dead, Unidentified, locks, keys we do not map
	}
	id := m.Code
	if id == "" {
		id = m.Key
	}
	if !m.Down {
		if s, ok := in.held[id]; ok {
			delete(in.held, id)
			return in.keysym(s, false)
		}
		return nil
	}
	if _, ok := in.held[id]; ok {
		// Wayland clients repeat a held key themselves; the browser's
		// repeats on top would double it.
		return nil
	}
	if len(in.held) >= maxHeldKeys {
		return nil
	}
	in.held[id] = sym
	return in.keysym(sym, true)
}

func (in *input) keysym(sym uint32, down bool) error {
	return in.s.call("NotifyKeyboardKeysym", int32(sym), state(down))
}

func (in *input) button(browser int, down bool) error {
	b, ok := evdevButton(browser)
	if !ok {
		return fmt.Errorf("wayland: unsupported mouse button %d", browser)
	}
	if !down && !in.buttons[b] {
		return nil
	}
	if down {
		in.buttons[b] = true
	} else {
		delete(in.buttons, b)
	}
	return in.s.call("NotifyPointerButton", b, state(down))
}

// evdevButton maps MouseEvent.button: main, middle, secondary, back, forward.
func evdevButton(browser int) (int32, bool) {
	switch browser {
	case 0:
		return btnLeft, true
	case 1:
		return btnMiddle, true
	case 2:
		return btnRight, true
	case 3:
		return btnSide, true
	case 4:
		return btnExtra, true
	}
	return 0, false
}

// wheel turns pixel deltas into discrete steps, one per wheelStep, carrying
// the remainder so slow trackpad scrolls still add up. Discrete steps mean
// the same thing on every compositor; smooth scroll's sign does not.
func (in *input) wheel(dx, dy float64) error {
	var steps int32
	steps, in.wheelY = wheelSteps(in.wheelY + dy)
	if steps != 0 {
		if err := in.s.call("NotifyPointerAxisDiscrete", axisVertical, steps); err != nil {
			return err
		}
	}
	steps, in.wheelX = wheelSteps(in.wheelX + dx)
	if steps != 0 {
		return in.s.call("NotifyPointerAxisDiscrete", axisHorizontal, steps)
	}
	return nil
}

func wheelSteps(acc float64) (int32, float64) {
	n := math.Trunc(acc / wheelStep)
	rest := acc - n*wheelStep
	return int32(max(min(n, maxWheelSteps), -maxWheelSteps)), rest
}

// releaseAll lets go of every key and button we hold.
func (in *input) releaseAll() error {
	var first error
	for id, sym := range in.held {
		delete(in.held, id)
		if err := in.keysym(sym, false); err != nil && first == nil {
			first = err
		}
	}
	for b := range in.buttons {
		delete(in.buttons, b)
		if err := in.s.call("NotifyPointerButton", b, state(false)); err != nil && first == nil {
			first = err
		}
	}
	in.wheelX, in.wheelY = 0, 0
	return first
}

func (in *input) close() {
	if in.viewOnly {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	in.releaseAll()
}

func state(down bool) uint32 {
	if down {
		return 1
	}
	return 0
}
