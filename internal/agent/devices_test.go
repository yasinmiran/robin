//go:build linux

package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/yasinmiran/robin/internal/agent/track"
	"github.com/yasinmiran/robin/internal/protocol"
)

// fakeMic and fakeCamera count the packets of each track they are given.
// A Play lingers a while after its track ends, as one still decoding or
// logging would, and Close notes whether one was still running.
type fakeMic struct {
	where   string
	packets chan int
	linger  time.Duration

	mu                 sync.Mutex
	closed             bool
	playing            int
	closedWhilePlaying bool
}

func newFakeMic(where string) *fakeMic { return &fakeMic{where: where, packets: make(chan int, 64)} }

func (m *fakeMic) Play(r track.Reader) error {
	m.mu.Lock()
	m.playing++
	m.mu.Unlock()
	defer func() {
		time.Sleep(m.linger)
		m.mu.Lock()
		m.playing--
		m.mu.Unlock()
	}()
	n := 0
	for {
		if _, _, err := r.ReadRTP(); err != nil {
			return err
		}
		n++
		select {
		case m.packets <- n:
		default:
		}
	}
}
func (m *fakeMic) Where() string { return m.where }
func (m *fakeMic) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.closedWhilePlaying = m.closedWhilePlaying || m.playing > 0
	return nil
}

func (m *fakeMic) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

func (m *fakeMic) wasClosedWhilePlaying() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closedWhilePlaying
}

type fakeCamera struct {
	*fakeMic
	askKeyframe bool
}

func (c *fakeCamera) Play(r track.Reader, keyframe func()) error {
	if c.askKeyframe {
		keyframe()
	}
	return c.fakeMic.Play(r)
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestStartMic(t *testing.T) {
	mic := newFakeMic("PipeWire")
	var asked MicOptions
	newMic := func(_ context.Context, o MicOptions) (Mic, error) { asked = o; return mic, nil }
	sound := newFakeAudio()
	defer sound.Close()

	for _, tc := range []struct {
		name     string
		start    protocol.Start
		newMic   NewMicFunc
		sound    AudioSource
		soundErr string
		want     *protocol.Device
	}{
		{"not asked", protocol.Start{}, newMic, nil, "", nil},
		{"attached", protocol.Start{Mic: true, Attach: ":0"}, newMic, nil, "", &protocol.Device{Available: true, Where: "PipeWire"}},
		{"spawned", protocol.Start{Mic: true}, newMic, sound, "", &protocol.Device{Available: true, Where: "PipeWire"}},
		{"spawned without sound", protocol.Start{Mic: true}, newMic, nil, "install pulseaudio", &protocol.Device{Error: "install pulseaudio"}},
		{"no support", protocol.Start{Mic: true, Attach: ":0"}, nil, nil, "", &protocol.Device{Error: "this agent was built without a microphone"}},
		{"refused", protocol.Start{Mic: true, Attach: ":0"}, func(context.Context, MicOptions) (Mic, error) {
			return nil, errors.New("gst-launch-1.0 is not installed")
		}, nil, "", &protocol.Device{Error: "gst-launch-1.0 is not installed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked = MicOptions{}
			m, d := startMic(context.Background(), tc.start, tc.newMic, tc.sound, tc.soundErr, discard())
			if (d == nil) != (tc.want == nil) || d != nil && *d != *tc.want {
				t.Fatalf("device %+v, want %+v", d, tc.want)
			}
			if (m != nil) != (tc.want != nil && tc.want.Available) {
				t.Errorf("mic %v with device %+v", m, d)
			}
			if m != nil && (asked.Attached != (tc.start.Attach != "") || asked.Sound != tc.sound) {
				t.Errorf("NewMic got %+v", asked)
			}
		})
	}
}

func TestStartCamera(t *testing.T) {
	cam := &fakeCamera{fakeMic: newFakeMic("/dev/video9")}
	var asked CameraOptions
	newCamera := func(_ context.Context, o CameraOptions) (Camera, error) { asked = o; return cam, nil }
	for _, tc := range []struct {
		name      string
		start     protocol.Start
		newCamera NewCameraFunc
		want      *protocol.Device
	}{
		{"not asked", protocol.Start{}, newCamera, nil},
		{"spawned with a loopback device", protocol.Start{Camera: true, CameraDevice: "/dev/video9"}, newCamera, &protocol.Device{Available: true, Where: "/dev/video9"}},
		{"attached to PipeWire", protocol.Start{Camera: true, Attach: protocol.AttachWayland}, newCamera, &protocol.Device{Available: true, Where: "/dev/video9"}},
		{"spawned without one", protocol.Start{Camera: true}, newCamera, &protocol.Device{Error: noCameraRoute}},
		{"no support", protocol.Start{Camera: true, Attach: ":0"}, nil, &protocol.Device{Error: "this agent was built without a camera"}},
		{"refused", protocol.Start{Camera: true, Attach: ":0"}, func(context.Context, CameraOptions) (Camera, error) {
			return nil, errors.New("no element pipewiresink")
		}, &protocol.Device{Error: "no element pipewiresink"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, d := startCamera(context.Background(), tc.start, tc.newCamera, discard())
			if (d == nil) != (tc.want == nil) || d != nil && *d != *tc.want {
				t.Fatalf("device %+v, want %+v", d, tc.want)
			}
			if (c != nil) != (tc.want != nil && tc.want.Available) {
				t.Errorf("camera %v with device %+v", c, d)
			}
			if c != nil && asked.Device != tc.start.CameraDevice {
				t.Errorf("NewCamera got %+v", asked)
			}
		})
	}
}

// sendingPage is a Pion peer playing the page with both devices on: video
// in, sound both ways with a microphone track, and a camera track out.
func sendingPage(t *testing.T, h *harness) (pc *webrtc.PeerConnection, mic, camera *webrtc.TrackLocalStaticSample, camSender *webrtc.RTPSender) {
	t.Helper()
	pc = offerFromBrowser(t, h, webrtc.RTPCodecTypeVideo)
	var err error
	if mic, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "mic", "page"); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.AddTransceiverFromTrack(mic, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv}); err != nil {
		t.Fatal(err)
	}
	if camera, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
	}, "camera", "page"); err != nil {
		t.Fatal(err)
	}
	tr, err := pc.AddTransceiverFromTrack(camera, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	if err != nil {
		t.Fatal(err)
	}
	return pc, mic, camera, tr.Sender()
}

// TestSessionTakesTheMicrophoneAndCamera: Ready says where each went, the
// page's tracks reach them, the camera's keyframe requests reach the page,
// and both are closed when the session ends, only once every Play is done.
func TestSessionTakesTheMicrophoneAndCamera(t *testing.T) {
	mic := newFakeMic("the private pulseaudio")
	cam := &fakeCamera{fakeMic: newFakeMic("/dev/video9"), askKeyframe: true}
	mic.linger, cam.linger = 300*time.Millisecond, 300*time.Millisecond
	audioAsked := make(chan AudioOptions, 1)
	sound := newFakeAudio()
	h := startAgent(t, func(o *Options) {
		o.NewAudio = func(_ context.Context, ao AudioOptions) (AudioSource, []string, error) {
			audioAsked <- ao
			return sound, nil, nil
		}
		o.NewMic = func(_ context.Context, mo MicOptions) (Mic, error) {
			if mo.Sound != sound {
				t.Errorf("the microphone was not given the session's sound server")
			}
			return mic, nil
		}
		o.NewCamera = func(context.Context, CameraOptions) (Camera, error) { return cam, nil }
	})
	ready := h.start(func(s *protocol.Start) { s.Mic, s.Camera, s.CameraDevice = true, true, "/dev/video9" })
	if ao := <-audioAsked; !ao.Mic || !ao.NoCapture {
		t.Errorf("NewAudio got %+v; want a server for the microphone alone", ao)
	}
	if ready.Audio || ready.AudioError != "" {
		t.Errorf("ready reports audio %v (%q) though Start asked for none", ready.Audio, ready.AudioError)
	}
	if ready.Mic == nil || !ready.Mic.Available || ready.Mic.Where != "the private pulseaudio" {
		t.Errorf("ready.Mic = %+v", ready.Mic)
	}
	if ready.Camera == nil || !ready.Camera.Available || ready.Camera.Where != "/dev/video9" {
		t.Errorf("ready.Camera = %+v", ready.Camera)
	}

	pc, micTrack, camTrack, camSender := sendingPage(t, h)
	answer(t, h, pc)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			micTrack.WriteSample(media.Sample{Data: opusSilence, Duration: 20 * time.Millisecond})
			camTrack.WriteSample(media.Sample{Data: []byte{0, 0, 0, 1, 0x65, 0x88, 0x84, 0x21}, Duration: 20 * time.Millisecond})
		}
	}()
	for name, ch := range map[string]chan int{"microphone": mic.packets, "camera": cam.packets} {
		select {
		case <-ch:
		case <-time.After(15 * time.Second):
			t.Fatalf("no %s packets reached the session; ICE %s", name, pc.ICEConnectionState())
		}
	}
	pli := false
	for deadline := time.Now().Add(5 * time.Second); !pli && time.Now().Before(deadline); {
		camSender.SetReadDeadline(time.Now().Add(time.Second))
		pkts, _, err := camSender.ReadRTCP()
		if err != nil {
			continue
		}
		for _, p := range pkts {
			_, pli = p.(*rtcp.PictureLossIndication)
			if pli {
				break
			}
		}
	}
	if !pli {
		t.Error("the camera's keyframe request never reached the page")
	}

	h.stdin.Close()
	h.waitGone(ready.Display)
	if !mic.isClosed() || !cam.isClosed() || !sound.closed() {
		t.Errorf("left open: microphone %v, camera %v, sound server %v", !mic.isClosed(), !cam.isClosed(), !sound.closed())
	}
	if mic.wasClosedWhilePlaying() || cam.wasClosedWhilePlaying() {
		t.Errorf("closed under a running Play: microphone %v, camera %v", mic.wasClosedWhilePlaying(), cam.wasClosedWhilePlaying())
	}
}

// TestSessionWithoutDevicesSaysWhy: a spawned desktop with no loopback
// device and no sound server reports both, and the session goes on.
func TestSessionWithoutDevicesSaysWhy(t *testing.T) {
	h := startAgent(t, func(o *Options) {
		o.NewAudio = func(context.Context, AudioOptions) (AudioSource, []string, error) {
			return nil, nil, errors.New("no sound server for the new desktop: install pulseaudio on the host")
		}
		o.NewMic = func(context.Context, MicOptions) (Mic, error) { return nil, errors.New("not reached") }
		o.NewCamera = func(context.Context, CameraOptions) (Camera, error) { return nil, errors.New("not reached") }
	})
	ready := h.start(func(s *protocol.Start) { s.Mic, s.Camera = true, true })
	if ready.Mic == nil || ready.Mic.Available || !strings.Contains(ready.Mic.Error, "install pulseaudio") {
		t.Errorf("ready.Mic = %+v", ready.Mic)
	}
	if ready.Camera == nil || ready.Camera.Available || !strings.Contains(ready.Camera.Error, "v4l2loopback") {
		t.Errorf("ready.Camera = %+v", ready.Camera)
	}
	if ready.AudioError != "" {
		t.Errorf("ready.AudioError = %q though Start asked for no sound", ready.AudioError)
	}
	h.stdin.Close()
	h.waitGone(ready.Display)
}
