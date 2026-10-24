package probe

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestParseLoopbackDevices(t *testing.T) {
	out := "probe=1\nos=Linux\nv4l2loopback=/dev/video4 OBS Virtual Camera\nv4l2loopback=/dev/video9 Robin camera\nv4l2loopback=/dev/video10 \n"
	r, err := Parse([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	want := []VideoDevice{{"/dev/video4", "OBS Virtual Camera"}, {"/dev/video9", "Robin camera"}, {"/dev/video10", ""}}
	if !slices.Equal(r.V4L2Loopback, want) {
		t.Errorf("V4L2Loopback = %q, want %q", r.V4L2Loopback, want)
	}
}

func TestCameraDevice(t *testing.T) {
	for _, tc := range []struct {
		devices []VideoDevice
		want    string
	}{
		{nil, ""},
		{[]VideoDevice{{"/dev/video4", "OBS Virtual Camera"}}, "/dev/video4"},
		{[]VideoDevice{{"/dev/video4", "OBS Virtual Camera"}, {"/dev/video9", "ROBIN"}}, "/dev/video9"},
		{[]VideoDevice{{"/dev/video2", ""}, {"/dev/video3", "Dummy video device (0x0000)"}}, "/dev/video2"},
	} {
		r := &Report{V4L2Loopback: tc.devices}
		if got := r.CameraDevice(); got != tc.want {
			t.Errorf("CameraDevice of %q = %q, want %q", tc.devices, got, tc.want)
		}
	}
}

// TestScriptFindsNoCameraHere: the probe's camera search runs cleanly on a
// machine without v4l2loopback and reports nothing.
func TestScriptFindsNoCameraHere(t *testing.T) {
	if _, err := os.Stat("/sys/devices/virtual/video4linux"); err == nil {
		t.Skip("this machine has v4l2loopback devices")
	}
	if !strings.Contains(Script, "\tcameras\n") || !strings.Contains(Script, "kv v4l2loopback") {
		t.Fatal("the probe does not look for cameras")
	}
	cmd := exec.Command("sh", "-s", "--")
	cmd.Stdin = strings.NewReader(Script)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	r, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.V4L2Loopback) != 0 {
		t.Errorf("found %q", r.V4L2Loopback)
	}
}
