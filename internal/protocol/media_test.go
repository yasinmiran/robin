package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDeviceWireNames pins the names the page and an agent of another build
// read: config.mic and config.camera in the page, the rest across stdio.
func TestDeviceWireNames(t *testing.T) {
	for _, c := range []struct {
		v    any
		want string
	}{
		{Start{Mic: true, Camera: true, CameraDevice: "/dev/video9"}, `"mic":true,"camera":true,"cameraDevice":"/dev/video9"`},
		{Ready{Mic: &Device{Available: true, Where: "PipeWire"}, Camera: &Device{Error: "no v4l2loopback"}},
			`"mic":{"available":true,"where":"PipeWire"},"camera":{"available":false,"error":"no v4l2loopback"}`},
		{Config{ICEServers: []ICEServer{}, Mic: true, Camera: true}, `{"iceServers":[],"mic":true,"camera":true}`},
	} {
		b, err := json.Marshal(c.v)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), c.want) {
			t.Errorf("%T encodes as %s, want it to hold %s", c.v, b, c.want)
		}
	}
	b, _ := json.Marshal(Ready{})
	if strings.Contains(string(b), "mic") || strings.Contains(string(b), "camera") {
		t.Errorf("a Ready that was not asked about devices mentions them: %s", b)
	}
}
