package main

import (
	"slices"
	"testing"
)

func TestParseSize(t *testing.T) {
	for in, want := range map[string][2]int{"1920x1080": {1920, 1080}, "1280X720": {1280, 720}} {
		t.Run(in, func(t *testing.T) {
			w, h, err := parseSize(in)
			if err != nil || w != want[0] || h != want[1] {
				t.Errorf("got %dx%d, %v", w, h, err)
			}
		})
	}
	for _, bad := range []string{"", "1920", "x1080", "1920x", "axb", "10x10", "-5x700"} {
		t.Run("rejects "+bad, func(t *testing.T) {
			if _, _, err := parseSize(bad); err == nil {
				t.Errorf("%q accepted", bad)
			}
		})
	}
}

func TestParseSTUN(t *testing.T) {
	for in, want := range map[string][]string{
		"none":                   nil,
		"":                       nil,
		"stun:a:3478":            {"stun:a:3478"},
		" stun:a:1 , stun:b:2 ,": {"stun:a:1", "stun:b:2"},
	} {
		t.Run(in, func(t *testing.T) {
			if got := parseSTUN(in); !slices.Equal(got, want) {
				t.Errorf("got %q want %q", got, want)
			}
		})
	}
}

func TestDefaultSSHMux(t *testing.T) {
	for goos, want := range map[string]bool{"linux": true, "darwin": true, "windows": false} {
		if got := defaultSSHMux(goos); got != want {
			t.Errorf("defaultSSHMux(%q) = %v, want %v", goos, got, want)
		}
	}
}
