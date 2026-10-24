package gui

import (
	"slices"
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
	if got := deviceNotes("box", ready); !slices.Equal(got, want) {
		t.Errorf("notes\n got  %q\n want %q", got, want)
	}
	if mic, camera := offeredDevices(ready); !mic || camera {
		t.Errorf("offered mic %v camera %v, want the microphone alone", mic, camera)
	}
	if got := deviceNotes("box", &protocol.Ready{}); len(got) != 0 {
		t.Errorf("notes for devices nobody asked for: %q", got)
	}
}
