//go:build linux

package agent

import (
	"context"
	"errors"
	"log/slog"

	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/robin/internal/agent/track"
	"github.com/yasinmiran/robin/internal/protocol"
)

// Mic is the browser's microphone as a source the desktop's programs can
// record. It is there, silent, until Close.
type Mic interface {
	// Play feeds it one track from the browser until the track ends.
	Play(track.Reader) error
	// Where says what carries it on the host, for Ready.
	Where() string
	Close() error
}

// MicOptions say where the microphone goes.
type MicOptions struct {
	// Attached means the desktop's own sound server takes it. Otherwise it
	// goes to the sound server the agent started, Sound.
	Attached bool
	Sound    AudioSource
	Log      *slog.Logger
}

// NewMicFunc gives the desktop the browser's microphone.
type NewMicFunc func(ctx context.Context, o MicOptions) (Mic, error)

// Camera is the browser's camera as a device the host's programs can open.
// It shows black until Play, and until Close.
type Camera interface {
	// Play shows one track from the browser until the track ends, calling
	// keyframe when it needs the browser to send one.
	Play(r track.Reader, keyframe func()) error
	// Where says what carries it on the host, for Ready.
	Where() string
	Close() error
}

// CameraOptions say where the camera goes.
type CameraOptions struct {
	// Device is a v4l2loopback device to write; empty uses the user's
	// PipeWire, which only an attached desktop has.
	Device string
	Log    *slog.Logger
}

// NewCameraFunc gives the host the browser's camera.
type NewCameraFunc func(ctx context.Context, o CameraOptions) (Camera, error)

// errNoCameraRoute is what a spawned desktop without a v4l2loopback device
// gets: there is no PipeWire session of the user's to put a camera in.
var errNoCameraRoute = errors.New("a spawned desktop takes the camera only through v4l2loopback, and the host has no device this user can write")

// spawnMic says the session wants a microphone on the sound server it
// starts itself, which has to know before it starts.
func spawnMic(s protocol.Start) bool { return s.Mic && s.Attach == "" }

// startMic gives the desktop the microphone when Start asks for it. sound
// is the session's sound, and soundErr why it has none.
func startMic(ctx context.Context, start protocol.Start, newMic NewMicFunc, sound AudioSource, soundErr string, log *slog.Logger) (Mic, *protocol.Device) {
	if !start.Mic {
		return nil, nil
	}
	fail := func(why string) (Mic, *protocol.Device) {
		log.Warn("microphone unavailable", "err", why)
		return nil, &protocol.Device{Error: why}
	}
	switch {
	case newMic == nil:
		return fail("this agent was built without a microphone")
	case start.Attach == "" && sound == nil:
		return fail(soundErr)
	}
	m, err := newMic(ctx, MicOptions{Attached: start.Attach != "", Sound: sound, Log: log})
	if err != nil {
		return fail(err.Error())
	}
	return m, &protocol.Device{Available: true, Where: m.Where()}
}

// startCamera gives the host the camera when Start asks for it.
func startCamera(ctx context.Context, start protocol.Start, newCamera NewCameraFunc, log *slog.Logger) (Camera, *protocol.Device) {
	if !start.Camera {
		return nil, nil
	}
	fail := func(why string) (Camera, *protocol.Device) {
		log.Warn("camera unavailable", "err", why)
		return nil, &protocol.Device{Error: why}
	}
	switch {
	case newCamera == nil:
		return fail("this agent was built without a camera")
	case start.Attach == "" && start.CameraDevice == "":
		return fail(errNoCameraRoute.Error())
	}
	c, err := newCamera(ctx, CameraOptions{Device: start.CameraDevice, Log: log})
	if err != nil {
		return fail(err.Error())
	}
	return c, &protocol.Device{Available: true, Where: c.Where()}
}

// track hands a track the browser sends to the microphone or the camera,
// to play on a goroutine of its own until the track ends.
func (s *session) track(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
	var device string
	var play func() error
	switch {
	case tr.Kind() == webrtc.RTPCodecTypeAudio && s.mic != nil:
		device = "microphone"
		play = func() error { return s.mic.Play(tr) }
	case tr.Kind() == webrtc.RTPCodecTypeVideo && s.camera != nil:
		device = "camera"
		p, ssrc := s.currentOrNil(), uint32(tr.SSRC())
		play = func() error {
			return s.camera.Play(tr, func() {
				if p != nil {
					p.RequestKeyframe(ssrc)
				}
			})
		}
	default:
		s.log.Info("ignoring a track the session did not offer to take", "kind", tr.Kind().String())
		return
	}
	s.playsMu.Lock()
	defer s.playsMu.Unlock()
	if s.playsEnded {
		return
	}
	s.plays.Go(func() {
		if err := play(); err != nil {
			s.log.Debug(device+" track", "err", err)
		}
	})
}

// endPlays starts no more Plays and waits for those running. Run calls it
// once the peer is closed, which ends their tracks, and before the
// microphone and camera close under them.
func (s *session) endPlays() {
	s.playsMu.Lock()
	s.playsEnded = true
	s.playsMu.Unlock()
	s.plays.Wait()
}
