//go:build linux

package x11

import (
	"strconv"
	"strings"

	"github.com/jezek/xgb/xproto"
)

// Keysyms from <X11/keysymdef.h> that named keys map to.
const (
	xkShiftL         = 0xffe1
	xkShiftR         = 0xffe2
	xkControlL       = 0xffe3
	xkControlR       = 0xffe4
	xkAltL           = 0xffe9
	xkAltR           = 0xffea
	xkSuperL         = 0xffeb
	xkSuperR         = 0xffec
	xkModeSwitch     = 0xff7e
	xkISOLevel3Shift = 0xfe03
	xkF1             = 0xffbe
)

// namedKeys maps KeyboardEvent.key values that are not characters.
var namedKeys = map[string]xproto.Keysym{
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

// namedKeysym maps a non-character key. Modifiers take their kind from key
// and their side from code, so a remapped key keeps its meaning.
//
// CapsLock and NumLock are deliberately absent: the browser already applies
// them to key, and a lock on the remote side would apply them again.
func namedKeysym(key, code string) (xproto.Keysym, bool) {
	right := strings.HasSuffix(code, "Right")
	side := func(l, r xproto.Keysym) (xproto.Keysym, bool) {
		if right {
			return r, true
		}
		return l, true
	}
	switch key {
	case "AltGraph":
		return xkISOLevel3Shift, true
	case "Shift":
		return side(xkShiftL, xkShiftR)
	case "Control":
		return side(xkControlL, xkControlR)
	case "Alt":
		return side(xkAltL, xkAltR)
	case "Meta", "OS":
		return side(xkSuperL, xkSuperR)
	}
	if ks, ok := namedKeys[key]; ok {
		return ks, true
	}
	if n, ok := strings.CutPrefix(key, "F"); ok {
		if i, err := strconv.Atoi(n); err == nil && i >= 1 && i <= 24 {
			return xkF1 + xproto.Keysym(i-1), true
		}
	}
	return 0, false
}

// runeKeysym returns the keysym for a character: Latin-1 keysyms equal the
// code point, everything else uses the Unicode keysym range.
func runeKeysym(r rune) xproto.Keysym {
	if r >= 0x20 && r <= 0x7e || r >= 0xa0 && r <= 0xff {
		return xproto.Keysym(r)
	}
	return 0x01000000 + xproto.Keysym(r)
}

func isShift(s xproto.Keysym) bool { return s == xkShiftL || s == xkShiftR }

func isLevel3(s xproto.Keysym) bool { return s == xkISOLevel3Shift || s == xkModeSwitch }
