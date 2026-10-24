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
	for _, tc := range []struct {
		name, in string
		want     time.Duration
		bad      bool
	}{
		{"empty leaves it to the mode", "", 0, false},
		{"a duration", "90m", 90 * time.Minute, false},
		{"zero keeps nothing", "0", gui.KeepNone, false},
		{"negative", "-1h", 0, true},
		{"words", "soon", 0, true},
		{"bare number", "8", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := parseKeep(tc.in)
			if (err != nil) != tc.bad || d != tc.want {
				t.Errorf("parseKeep(%q) = %v, %v; want %v, bad %v", tc.in, d, err, tc.want, tc.bad)
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
