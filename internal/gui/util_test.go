package gui

import (
	"strings"
	"testing"
)

func TestPrefixWriterPrefixesWholeLines(t *testing.T) {
	var b strings.Builder
	w := prefixWriter(&b, "agent: ")
	w.Write([]byte("one\ntw"))
	w.Write([]byte("o\nthree"))
	if got := b.String(); got != "agent: one\nagent: two\n" {
		t.Fatalf("got %q", got)
	}
}
