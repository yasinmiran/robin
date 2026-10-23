package gui

import (
	"testing"

	"github.com/yasinmiran/robin/internal/protocol"
)

func TestMonitorNote(t *testing.T) {
	three := []protocol.Monitor{
		{Name: "DP-3-1-8", W: 2560, H: 1440, Primary: true},
		{Name: "eDP-1", X: 1638, Y: 1440, W: 1920, H: 1200},
		{Name: "DP-3-2-8", X: 2560, W: 2560, H: 1440},
	}
	for _, tc := range []struct {
		name  string
		ready protocol.Ready
		want  string
	}{
		{"one monitor", protocol.Ready{Monitors: three[:1], Monitor: "DP-3-1-8"}, ""},
		{"a spawned display", protocol.Ready{}, ""},
		{"one of three", protocol.Ready{Monitors: three, Monitor: "eDP-1"},
			", showing monitor eDP-1 (1920x1200), one of 3 (switch in the page)"},
		{"all of them", protocol.Ready{Monitors: three, Monitor: protocol.MonitorAll},
			", showing all 3 monitors as one picture (switch in the page)"},
	} {
		if got := monitorNote(&tc.ready); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestStartMessagePassesTheMonitor(t *testing.T) {
	s, _ := startMessage(Options{Attach: ":1", Monitor: "eDP-1", Encoder: protocol.EncoderSoftware}, fixture(t, "real-host-ubuntu-22.04.txt"))
	if s.Monitor != "eDP-1" {
		t.Errorf("Start.Monitor %q, want eDP-1", s.Monitor)
	}
}
