package runes

import "testing"

func TestReordersText(t *testing.T) {
	for r, want := range map[rune]bool{
		'‮':  true, // right-to-left override
		'⁦':  true, // left-to-right isolate
		'‎':  true, // left-to-right mark
		'؜':  true, // Arabic letter mark
		' ':  true, // line separator
		' ':  true, // paragraph separator
		'a':  false,
		'日':  false,
		' ':  false,
		'\n': false, // a control character, for callers to treat as one
		'​':  false, // zero width space: invisible, but reorders nothing
	} {
		if got := ReordersText(r); got != want {
			t.Errorf("ReordersText(%U) = %v, want %v", r, got, want)
		}
	}
}
