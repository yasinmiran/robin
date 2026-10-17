package keysym

import "testing"

func TestNamed(t *testing.T) {
	for _, tc := range []struct {
		key, code string
		want      uint32
	}{
		{"Enter", "Enter", 0xff0d},
		{"Shift", "ShiftRight", ShiftR},
		{"Control", "ControlLeft", 0xffe3},
		{"Meta", "MetaRight", 0xffec},
		{"F12", "F12", 0xffc9},
		{"AltGraph", "AltRight", ISOLevel3Shift},
	} {
		if got, ok := Named(tc.key, tc.code); !ok || got != tc.want {
			t.Errorf("Named(%q, %q) = %#x, %v; want %#x", tc.key, tc.code, got, ok, tc.want)
		}
	}
	for _, key := range []string{"CapsLock", "NumLock", "Dead", "F0", "F25"} {
		if got, ok := Named(key, key); ok {
			t.Errorf("Named(%q) = %#x; want no keysym", key, got)
		}
	}
}

func TestRune(t *testing.T) {
	for r, want := range map[rune]uint32{'a': 0x61, 'é': 0xe9, '€': 0x10020ac, 'ж': 0x1000436} {
		if got := Rune(r); got != want {
			t.Errorf("Rune(%q) = %#x, want %#x", r, got, want)
		}
	}
}
