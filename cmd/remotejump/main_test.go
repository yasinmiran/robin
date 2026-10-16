package main

import (
	"slices"
	"testing"
)

func TestParseSize(t *testing.T) {
	for in, want := range map[string][2]int{"1920x1080": {1920, 1080}, "1280X720": {1280, 720}} {
		w, h, err := parseSize(in)
		if err != nil || w != want[0] || h != want[1] {
			t.Errorf("%s: got %dx%d, %v", in, w, h, err)
		}
	}
	for _, bad := range []string{"", "1920", "x1080", "1920x", "axb", "10x10", "-5x700"} {
		if _, _, err := parseSize(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseSTUN(t *testing.T) {
	for in, want := range map[string][]string{
		"none":                   nil,
		"":                       nil,
		"stun:a:3478":            {"stun:a:3478"},
		" stun:a:1 , stun:b:2 ,": {"stun:a:1", "stun:b:2"},
	} {
		if got := parseSTUN(in); !slices.Equal(got, want) {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}
