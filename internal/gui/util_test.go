package gui

import (
	"strings"
	"testing"
)

func TestTailBufferKeepsLastBytes(t *testing.T) {
	tb := &tailBuffer{max: 10}
	tb.Write([]byte("0123456789"))
	tb.Write([]byte("abc"))
	if got := tb.String(); got != "3456789abc" {
		t.Fatalf("got %q", got)
	}
}

func TestPrefixWriterPrefixesWholeLines(t *testing.T) {
	var b strings.Builder
	w := prefixWriter(&b, "agent: ")
	w.Write([]byte("one\ntw"))
	w.Write([]byte("o\nthree"))
	if got := b.String(); got != "agent: one\nagent: two\n" {
		t.Fatalf("got %q", got)
	}
}
