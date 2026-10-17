package gui

import (
	"testing"

	"github.com/yasinmiran/remotejump/internal/probe"
)

func TestStartMessageDefaults(t *testing.T) {
	for _, tc := range []struct {
		name         string
		o            Options
		cpus         int
		w, h, fps, k int
	}{
		{"defaults", Options{}, 8, 1920, 1080, 30, 10_000},
		{"small host", Options{}, 2, 1280, 720, 20, 10_000},
		{"unknown cores", Options{}, 0, 1920, 1080, 30, 10_000},
		{"asked for", Options{MaxWidth: 2560, MaxHeight: 1440, FPS: 60, MaxKbps: 4000}, 2, 2560, 1440, 60, 4000},
	} {
		s := startMessage(tc.o, &probe.Report{CPUs: tc.cpus})
		if s.MaxWidth != tc.w || s.MaxHeight != tc.h || s.FPS != tc.fps || s.MaxKbps != tc.k {
			t.Errorf("%s: %dx%d at %d fps, %d kbps; want %dx%d at %d fps, %d kbps",
				tc.name, s.MaxWidth, s.MaxHeight, s.FPS, s.MaxKbps, tc.w, tc.h, tc.fps, tc.k)
		}
	}
}
