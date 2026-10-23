//go:build linux

package agent

import (
	"strings"
	"testing"

	"github.com/yasinmiran/robin/internal/protocol"
)

// yasinsDesk is a three-monitor desktop whose primary monitor is listed
// first, with a laptop panel below between two external screens.
var yasinsDesk = []protocol.Monitor{
	{Name: "DP-3-1-8", X: 0, Y: 0, W: 2560, H: 1440, Primary: true},
	{Name: "eDP-1", X: 1638, Y: 1440, W: 1920, H: 1200},
	{Name: "DP-3-2-8", X: 2560, Y: 0, W: 2560, H: 1440},
}

func TestPickMonitor(t *testing.T) {
	noPrimary := []protocol.Monitor{
		{Name: "B", X: 1920, Y: 0, W: 1920, H: 1080},
		{Name: "A", X: 0, Y: 0, W: 1920, H: 1080},
	}
	for _, tc := range []struct {
		name string
		list []protocol.Monitor
		want string
		pick string
	}{
		{"default is the primary", yasinsDesk, "", "DP-3-1-8"},
		{"primary by word", yasinsDesk, "primary", "DP-3-1-8"},
		{"by name", yasinsDesk, "eDP-1", "eDP-1"},
		{"by number", yasinsDesk, "3", "DP-3-2-8"},
		{"all of them", yasinsDesk, "all", protocol.MonitorAll},
		{"no primary: the one at the origin", noPrimary, "", "A"},
		{"no monitors listed: the whole desktop", nil, "", protocol.MonitorAll},
		{"no monitors listed, all asked", nil, "all", protocol.MonitorAll},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pickMonitor(tc.list, tc.want)
			if err != nil || got != tc.pick {
				t.Errorf("pickMonitor(%q) = %q, %v; want %q", tc.want, got, err, tc.pick)
			}
		})
	}

	for _, want := range []string{"HDMI-1", "0", "4", "dp-3-1-8"} {
		_, err := pickMonitor(yasinsDesk, want)
		if err == nil {
			t.Errorf("pickMonitor(%q) found a monitor", want)
			continue
		}
		for _, part := range []string{"1 DP-3-1-8 2560x1440+0+0 (primary)", "2 eDP-1 1920x1200+1638+1440", "3 DP-3-2-8", `"all"`} {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("pickMonitor(%q): %q does not list %q", want, err, part)
			}
		}
	}
	if _, err := pickMonitor(nil, "eDP-1"); err == nil || !strings.Contains(err.Error(), "lists no monitors") {
		t.Errorf("pickMonitor on a display without monitors: %v", err)
	}
}
