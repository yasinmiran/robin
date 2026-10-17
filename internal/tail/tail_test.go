package tail

import (
	"strings"
	"testing"
)

func TestBufferKeepsLastBytes(t *testing.T) {
	b := &Buffer{Max: 10}
	b.Write([]byte("0123456789"))
	b.Write([]byte("abc"))
	if got := b.String(); got != "3456789abc" {
		t.Fatalf("got %q", got)
	}
}

func TestZeroBufferKeepsDefault(t *testing.T) {
	var b Buffer
	b.Write([]byte(strings.Repeat("x", DefaultMax+100)))
	if got := len(b.String()); got != DefaultMax {
		t.Fatalf("kept %d bytes, want %d", got, DefaultMax)
	}
}
