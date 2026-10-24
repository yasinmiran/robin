package main

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestIsCode(t *testing.T) {
	for line, want := range map[string]bool{
		"":                      false,
		"   \t":                 false,
		"// a comment":          false,
		"  /* opens a comment":  false,
		"   * inside a comment": false,
		"}":                     false,
		"  });":                 false,
		"],":                    false,
		"x = 1;":                true,
		"} else {":              true,
		"return;":               true,
		"});  // trailing note": true,
	} {
		if got := isCode(line); got != want {
			t.Errorf("isCode(%q) = %v, want %v", line, got, want)
		}
	}
}

// offset returns where needle starts in source, in UTF-16 units.
func offset(t *testing.T, p *page, needle string) int {
	t.Helper()
	i := strings.Index(p.source, needle)
	if i < 0 {
		t.Fatalf("%q not in source", needle)
	}
	return len(utf16.Encode([]rune(p.source[:i])))
}

const ifScript = `import { x } from './x.js';

// f runs only for a truthy a.
export function f(a) {
  if (a) {
    return 1;
  }
  return 2;
}
`

func covered(p *page) []int {
	var lines []int
	for i, c := range p.covered {
		if c && p.code[i] {
			lines = append(lines, i+1)
		}
	}
	return lines
}

func TestRanInnermostCountWins(t *testing.T) {
	p := newPage("s.js", ifScript)
	fn := offset(t, p, "export function")
	block := offset(t, p, "{\n    return 1")
	// The script ran, f ran once with a falsy a: its if block did not.
	// Ranges arrive out of order to show order does not matter.
	p.ran([]v8Range{
		{Start: block, End: block + len("{\n    return 1;\n  }"), Count: 0},
		{Start: fn, End: len(p.units) - 1, Count: 1},
		{Start: 0, End: len(p.units), Count: 1},
	})
	if got, want := covered(p), []int{1, 4, 5, 8}; !slices.Equal(got, want) {
		t.Fatalf("covered lines %v, want %v", got, want)
	}
	if c, n := p.lines(); c != 4 || n != 5 {
		t.Fatalf("lines() = %d of %d, want 4 of 5", c, n)
	}
}

func TestRanFunctionNeverCalled(t *testing.T) {
	p := newPage("s.js", ifScript)
	fn := offset(t, p, "export function")
	p.ran([]v8Range{{Start: 0, End: len(p.units), Count: 1}, {Start: fn, End: len(p.units) - 1, Count: 0}})
	if got, want := covered(p), []int{1}; !slices.Equal(got, want) {
		t.Fatalf("covered lines %v, want %v", got, want)
	}
}

func TestRanMergesAcrossRuns(t *testing.T) {
	p := newPage("s.js", ifScript)
	fn := offset(t, p, "export function")
	block := offset(t, p, "{\n    return 1")
	end := block + len("{\n    return 1;\n  }")
	p.ran([]v8Range{{Start: 0, End: len(p.units), Count: 1}, {Start: block, End: end, Count: 0}})
	// A later run took the branch but, say, never reached the last line.
	p.ran([]v8Range{{Start: 0, End: fn, Count: 1}, {Start: block, End: end, Count: 1}})
	if c, n := p.lines(); c != n {
		t.Fatalf("lines() = %d of %d, want all of them across both runs", c, n)
	}
}

func TestRanClampsRanges(t *testing.T) {
	p := newPage("s.js", "a();\nb();\n")
	p.ran([]v8Range{{Start: -5, End: 3, Count: 1}, {Start: 5, End: 1000, Count: 1}})
	if c, n := p.lines(); c != 2 || n != 2 {
		t.Fatalf("lines() = %d of %d", c, n)
	}
}

func TestRanCountsUTF16Units(t *testing.T) {
	// V8 offsets count UTF-16 units: the emoji is two of them and × is one.
	p := newPage("s.js", "const s = '😀 × 😀';\nnext();\nlast();\n")
	next := offset(t, p, "next")
	if next != 21 {
		t.Fatalf("next() at unit %d, want 21", next)
	}
	p.ran([]v8Range{{Start: next, End: next + len("next();"), Count: 1}})
	if got, want := covered(p), []int{2}; !slices.Equal(got, want) {
		t.Fatalf("covered lines %v, want %v", got, want)
	}
}

func TestRanIgnoresWhitespace(t *testing.T) {
	// A range that covers only the indentation of a line does not cover it.
	p := newPage("s.js", "a();\n    b();\n")
	p.ran([]v8Range{{Start: 0, End: 9, Count: 1}})
	if got, want := covered(p), []int{1}; !slices.Equal(got, want) {
		t.Fatalf("covered lines %v, want %v", got, want)
	}
}
