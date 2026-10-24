package gui

import (
	"bytes"
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

// TestPrefixWriterBoundsAPartialLine: an agent that logs without newlines
// is shown in lines of maxLogLine, not held until memory runs out.
func TestPrefixWriterBoundsAPartialLine(t *testing.T) {
	var b strings.Builder
	w := prefixWriter(&b, "agent: ").(*linePrefixer)
	chunk := bytes.Repeat([]byte("x"), 1000)
	for range 1 << 10 {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
		if len(w.partial) > maxLogLine {
			t.Fatalf("holds %d bytes of a line, the bound is %d", len(w.partial), maxLogLine)
		}
	}
	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	if len(lines) < (1000<<10)/maxLogLine-1 {
		t.Fatalf("got %d lines out of 1000 KiB", len(lines))
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "agent: x") || len(l) > len("agent: ")+maxLogLine {
			t.Fatalf("line %.40q... of %d bytes", l, len(l))
		}
	}
	w.Write([]byte("\n"))
	if got := strings.Count(b.String(), "x"); got != 1000<<10 {
		t.Fatalf("%d of %d bytes shown", got, 1000<<10)
	}
}
