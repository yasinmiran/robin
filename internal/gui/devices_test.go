package gui

import (
	"slices"
	"strings"
	"testing"

	"github.com/yasinmiran/robin/internal/probe"
	"github.com/yasinmiran/robin/internal/protocol"
)

func TestStartMessageDevices(t *testing.T) {
	loopback := &probe.Report{V4L2Loopback: []probe.VideoDevice{{Path: "/dev/video9", Label: "Robin camera"}}}
	s, _ := startMessage(Options{Mic: true, Camera: true}, loopback)
	if !s.Mic || !s.Camera || s.CameraDevice != "/dev/video9" {
		t.Errorf("start %+v, want both devices and the loopback device", s)
	}
	s, _ = startMessage(Options{Mic: true}, loopback)
	if !s.Mic || s.Camera || s.CameraDevice != "" {
		t.Errorf("start %+v, want the microphone alone", s)
	}
	s, _ = startMessage(Options{}, loopback)
	if s.Mic || s.Camera {
		t.Errorf("start %+v offers devices nobody asked for", s)
	}
}

func TestDeviceNotes(t *testing.T) {
	ready := &protocol.Ready{
		Mic:    &protocol.Device{Available: true, Where: "PipeWire"},
		Camera: &protocol.Device{Error: "a spawned desktop takes the camera only through v4l2loopback"},
	}
	want := []string{
		"robin: your microphone can reach box as Robin microphone (PipeWire); switch it on in the page",
		"robin: no camera on box: a spawned desktop takes the camera only through v4l2loopback",
	}
	if got := deviceNotes("box", ready.Mic, ready.Camera); !slices.Equal(got, want) {
		t.Errorf("notes\n got  %q\n want %q", got, want)
	}
	if mic, camera := offeredDevices(ready.Mic, ready.Camera); !mic || camera {
		t.Errorf("offered mic %v camera %v, want the microphone alone", mic, camera)
	}
	loopback := &protocol.Device{Available: true, Where: "/dev/video9"}
	if got := deviceNotes("box", nil, loopback); !slices.Equal(got, []string{"robin: your camera can reach box on /dev/video9; switch it on in the page"}) {
		t.Errorf("loopback note %q", got)
	}
	if got := deviceNotes("box", nil, nil); len(got) != 0 {
		t.Errorf("notes for devices nobody asked for: %q", got)
	}
}

// TestAskedDevices checks that a resumed session, which keeps the devices
// it started with, offers the page only those this robin gui names.
func TestAskedDevices(t *testing.T) {
	ready := &protocol.Ready{
		Resumed: true,
		Mic:     &protocol.Device{Available: true, Where: "PipeWire"},
		Camera:  &protocol.Device{Available: true, Where: "PipeWire"},
	}
	if mic, camera := askedDevices(Options{}, ready); mic != nil || camera != nil {
		t.Errorf("a resume without --mic or --camera asks for mic %+v camera %+v", mic, camera)
	}
	mic, camera := askedDevices(Options{Camera: true}, ready)
	if mic != nil || camera != ready.Camera {
		t.Errorf("a resume with --camera asks for mic %+v camera %+v", mic, camera)
	}
	if offerMic, offerCamera := offeredDevices(mic, camera); offerMic || !offerCamera {
		t.Errorf("a resume with --camera offers mic %v camera %v", offerMic, offerCamera)
	}
	if got := deviceNotes("box", mic, camera); len(got) != 1 || !strings.Contains(got[0], "camera") {
		t.Errorf("notes %q, want the camera's alone", got)
	}
	if mic, camera := askedDevices(Options{Mic: true, Camera: true}, ready); mic != ready.Mic || camera != ready.Camera {
		t.Errorf("a resume with --mic --camera asks for mic %+v camera %+v", mic, camera)
	}
}
