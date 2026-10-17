//go:build linux

package x11

import (
	"github.com/jezek/xgb/xproto"

	"github.com/yasinmiran/remotejump/internal/agent/keysym"
)

const xkModeSwitch = 0xff7e

func namedKeysym(key, code string) (xproto.Keysym, bool) {
	s, ok := keysym.Named(key, code)
	return xproto.Keysym(s), ok
}

func runeKeysym(r rune) xproto.Keysym { return xproto.Keysym(keysym.Rune(r)) }

func isShift(s xproto.Keysym) bool { return s == keysym.ShiftL || s == keysym.ShiftR }

func isLevel3(s xproto.Keysym) bool { return s == keysym.ISOLevel3Shift || s == xkModeSwitch }
