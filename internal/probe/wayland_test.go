package probe

import (
	"slices"
	"testing"
)

func TestWaylandMissing(t *testing.T) {
	session := func(r *Report) *Report {
		r.Wayland = []string{"/run/user/1000/wayland-0"}
		r.SessionBus = "/run/user/1000/bus"
		r.UID = 1000
		return r
	}
	for _, tc := range []struct {
		name    string
		r       *Report
		missing []string
		hint    string
	}{
		{
			name: "ready",
			r: session(&Report{
				Distro: Distro{ID: "ubuntu"}, Commands: map[string]string{"gst-launch-1.0": "/usr/bin/gst-launch-1.0"},
				GstDirs: []string{"/usr/lib/x86_64-linux-gnu/gstreamer-1.0"}, GstPlugins: []string{"pipewire", "videoconvertscale", "y4menc"},
			}),
		},
		{
			name: "older and newer plugin file names",
			r: session(&Report{
				Distro: Distro{ID: "arch"}, Commands: map[string]string{"gst-launch-1.0": "/usr/bin/gst-launch-1.0"},
				GstDirs: []string{"/usr/lib/gstreamer-1.0"}, GstPlugins: []string{"pipewire", "videoconvert", "y4m"},
			}),
		},
		{
			name: "no tools on ubuntu",
			r: session(&Report{
				Distro: Distro{ID: "ubuntu"}, Commands: map[string]string{},
				GstDirs: []string{"/usr/lib/x86_64-linux-gnu/gstreamer-1.0"}, GstPlugins: []string{"videoconvertscale"},
			}),
			missing: []string{"gst-launch-1.0", "GStreamer's PipeWire plugin", "GStreamer's good plugins"},
			hint:    "sudo apt install gstreamer1.0-tools gstreamer1.0-pipewire gstreamer1.0-plugins-good",
		},
		{
			name: "no plugin directory to look in",
			r: session(&Report{
				Distro: Distro{ID: "fedora"}, Commands: map[string]string{},
			}),
			missing: []string{"gst-launch-1.0"},
			hint:    "sudo dnf install gstreamer1",
		},
		{
			name:    "nobody logged in",
			r:       &Report{Distro: Distro{ID: "ubuntu"}, Commands: map[string]string{"gst-launch-1.0": "/usr/bin/gst-launch-1.0"}},
			missing: []string{"a running Wayland session", "the session's D-Bus socket"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.WaylandMissing(); !slices.Equal(got, tc.missing) {
				t.Errorf("missing %q, want %q", got, tc.missing)
			}
			if got := tc.r.WaylandInstallHint(); got != tc.hint {
				t.Errorf("hint %q, want %q", got, tc.hint)
			}
		})
	}
}

func TestParseGstDirs(t *testing.T) {
	r, err := Parse([]byte("probe=1\ngst_dir=/usr/lib64/gstreamer-1.0\ngst_plugin=y4m\ngst_dir=/usr/lib64/gstreamer-1.0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.GstDirs, []string{"/usr/lib64/gstreamer-1.0"}) || !slices.Equal(r.GstPlugins, []string{"y4m"}) {
		t.Errorf("dirs %q plugins %q", r.GstDirs, r.GstPlugins)
	}
}
