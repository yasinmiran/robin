package main

import (
	"slices"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/gui"
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

func TestParseKeep(t *testing.T) {
	if d, err := parseKeep(""); err != nil || d != gui.KeepDefault {
		t.Errorf("empty: %v, %v", d, err)
	}
	if d, err := parseKeep("90m"); err != nil || d != 90*time.Minute {
		t.Errorf("90m: %v, %v", d, err)
	}
	if d, err := parseKeep("0"); err != nil || d != 0 {
		t.Errorf("0: %v, %v", d, err)
	}
	for _, bad := range []string{"-1h", "soon", "8"} {
		if _, err := parseKeep(bad); err == nil {
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
		t.Run(in, func(t *testing.T) {
			if got := parseSTUN(in); !slices.Equal(got, want) {
				t.Errorf("got %q want %q", got, want)
			}
		})
	}
}
