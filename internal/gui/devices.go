package gui

import (
	"fmt"
	"strings"

	"github.com/yasinmiran/robin/internal/protocol"
)

// deviceNotes tells the user, once, where the microphone and camera they
// asked for went on the host, or why they could not; nil is one they did
// not ask for.
func deviceNotes(host string, mic, camera *protocol.Device) []string {
	var notes []string
	for _, d := range []struct {
		name, label string
		ready       *protocol.Device
	}{
		{"microphone", protocol.MicName, mic},
		{"camera", protocol.CameraName, camera},
	} {
		switch {
		case d.ready == nil:
		case !d.ready.Available:
			notes = append(notes, fmt.Sprintf("robin: no %s on %s: %s", d.name, host, d.ready.Error))
		case strings.HasPrefix(d.ready.Where, "/dev/"):
			notes = append(notes, fmt.Sprintf("robin: your %s can reach %s on %s; switch it on in the page", d.name, host, d.ready.Where))
		default:
			notes = append(notes, fmt.Sprintf("robin: your %s can reach %s as %s (%s); switch it on in the page", d.name, host, d.label, d.ready.Where))
		}
	}
	return notes
}

// askedDevices is what ready says of the devices o asks for, nil for one it
// does not. A resumed session keeps every device it started with, and this
// CLI offers those its user names this time; the agent refuses a resume
// that names one it lacks.
func askedDevices(o Options, ready *protocol.Ready) (mic, camera *protocol.Device) {
	if o.Mic {
		mic = ready.Mic
	}
	if o.Camera {
		camera = ready.Camera
	}
	return mic, camera
}

// offeredDevices says which devices the page may offer: those the host took.
func offeredDevices(mic, camera *protocol.Device) (bool, bool) {
	return mic != nil && mic.Available, camera != nil && camera.Available
}
