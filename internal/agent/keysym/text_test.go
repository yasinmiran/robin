package keysym

import "testing"

func TestText(t *testing.T) {
	for r, want := range map[rune]uint32{'a': 0x61, ' ': 0x20, 'é': 0xe9, '日': 0x10065e5, '😀': 0x101f600, '\n': 0xff0d, '\t': 0xff09} {
		if got, ok := Text(r); !ok || got != want {
			t.Errorf("Text(%q) = %#x, %v; want %#x", r, got, ok, want)
		}
	}
	for _, r := range []rune{'\r', 0, 0x1b, 0x7f, 0x85, 0x9f} {
		if got, ok := Text(r); ok {
			t.Errorf("Text(%U) = %#x; want nothing typed", r, got)
		}
	}
}
