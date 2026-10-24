package gui

import (
	"fmt"
	"strings"

	"github.com/yasinmiran/robin/internal/protocol"
)

// deviceNotes tells the user, once, where the microphone and camera they
// asked for went on the host, or why they could not.
func deviceNotes(host string, ready *protocol.Ready) []string {
	var notes []string
	for _, d := range []struct {
		name, label string
		ready       *protocol.Device
	}{
		{"microphone", protocol.MicName, ready.Mic},
		{"camera", protocol.CameraName, ready.Camera},
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

// askedDevices is ready with only the devices o asks for. A resumed session
// keeps every device it started with, and this CLI offers those its user
// names this time; the agent refuses a resume that names one it lacks.
func askedDevices(o Options, ready *protocol.Ready) *protocol.Ready {
	r := *ready
	if !o.Mic {
		r.Mic = nil
	}
	if !o.Camera {
		r.Camera = nil
	}
	return &r
}

// offeredDevices says which devices the page may offer: those the host took.
func offeredDevices(ready *protocol.Ready) (mic, camera bool) {
	return ready.Mic != nil && ready.Mic.Available, ready.Camera != nil && ready.Camera.Available
}
