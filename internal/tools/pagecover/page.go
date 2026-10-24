package main

import (
	"cmp"
	"slices"
	"strings"
	"unicode/utf16"
)

// page is one script and which of its lines any input ran.
type page struct {
	name, source string
	units        []uint16 // the source as JavaScript indexes it
	lineOf       []int    // the line of each unit
	code         []bool   // whether each line holds code
	covered      []bool
}

func newPage(name, source string) *page {
	p := &page{name: name, source: source, units: utf16.Encode([]rune(source))}
	p.lineOf = make([]int, len(p.units))
	line := 0
	for i, u := range p.units {
		p.lineOf[i] = line
		if u == '\n' {
			line++
		}
	}
	for _, l := range strings.Split(source, "\n") {
		p.code = append(p.code, isCode(l))
	}
	p.covered = make([]bool, len(p.code))
	return p
}

// isCode reports whether a line is more than blank space, a comment or
// brackets closing what an earlier line opened.
func isCode(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "/*") || strings.HasPrefix(t, "*") {
		return false
	}
	return strings.Trim(t, "})];,") != ""
}

// ran marks the lines a script's ranges show running. V8 nests each block
// inside its function and each function inside the script, so painting
// outer ranges before inner ones leaves every unit with its innermost count.
func (p *page) ran(ranges []v8Range) {
	ranges = slices.Clone(ranges)
	slices.SortStableFunc(ranges, func(a, b v8Range) int {
		return cmp.Or(cmp.Compare(a.Start, b.Start), cmp.Compare(b.End, a.End))
	})
	counts := make([]int, len(p.units))
	for _, r := range ranges {
		for i := max(r.Start, 0); i < min(r.End, len(counts)); i++ {
			counts[i] = r.Count
		}
	}
	for i, c := range counts {
		if c > 0 && !isSpace(p.units[i]) {
			p.covered[p.lineOf[i]] = true
		}
	}
}

func isSpace(u uint16) bool {
	return u == ' ' || u == '\t' || u == '\n' || u == '\r'
}

// lines returns how many code lines ran, of how many there are.
func (p *page) lines() (covered, total int) {
	for i, code := range p.code {
		if code {
			total++
			if p.covered[i] {
				covered++
			}
		}
	}
	return covered, total
}
