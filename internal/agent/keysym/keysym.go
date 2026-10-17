// Package keysym maps browser keyboard events to the keysyms of
// <X11/keysymdef.h>, which both the X11 and the Wayland desktop take.
package keysym

import (
	"strconv"
	"strings"
)

// Keysyms that callers handle specially.
const (
	ShiftL         = 0xffe1
	ShiftR         = 0xffe2
	ISOLevel3Shift = 0xfe03
)

const (
	controlL = 0xffe3
	controlR = 0xffe4
	altL     = 0xffe9
	altR     = 0xffea
	superL   = 0xffeb
	superR   = 0xffec
	f1       = 0xffbe
)

// namedKeys maps KeyboardEvent.key values that are not characters.
var namedKeys = map[string]uint32{
	"Enter":       0xff0d,
	"Tab":         0xff09,
	"Backspace":   0xff08,
	"Escape":      0xff1b,
	"Delete":      0xffff,
	"Insert":      0xff63,
	"Home":        0xff50,
	"End":         0xff57,
	"PageUp":      0xff55,
	"PageDown":    0xff56,
	"ArrowLeft":   0xff51,
	"ArrowUp":     0xff52,
	"ArrowRight":  0xff53,
	"ArrowDown":   0xff54,
	"ContextMenu": 0xff67,
	"PrintScreen": 0xff61,
}

// Named maps a non-character key. Modifiers take their kind from key and
// their side from code, so a remapped key keeps its meaning.
//
// CapsLock and NumLock are deliberately absent: the browser already applies
// them to key, and a lock on the remote side would apply them again.
func Named(key, code string) (uint32, bool) {
	right := strings.HasSuffix(code, "Right")
	side := func(l, r uint32) (uint32, bool) {
		if right {
			return r, true
		}
		return l, true
	}
	switch key {
	case "AltGraph":
		return ISOLevel3Shift, true
	case "Shift":
		return side(ShiftL, ShiftR)
	case "Control":
		return side(controlL, controlR)
	case "Alt":
		return side(altL, altR)
	case "Meta", "OS":
		return side(superL, superR)
	}
	if ks, ok := namedKeys[key]; ok {
		return ks, true
	}
	if n, ok := strings.CutPrefix(key, "F"); ok {
		if i, err := strconv.Atoi(n); err == nil && i >= 1 && i <= 24 {
			return f1 + uint32(i-1), true
		}
	}
	return 0, false
}

// Rune returns the keysym for a character: Latin-1 keysyms equal the code
// point, everything else uses the Unicode keysym range.
func Rune(r rune) uint32 {
	if r >= 0x20 && r <= 0x7e || r >= 0xa0 && r <= 0xff {
		return uint32(r)
	}
	return 0x01000000 + uint32(r)
}
