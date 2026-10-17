//go:build linux

package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/robin/internal/protocol"
)

// opusSilence is a complete 20 ms Opus packet of silence.
var opusSilence = []byte{0xf8, 0xff, 0xfe}

// fakeAudio stands in for the desktop's sound: a silence packet every 20 ms
// until it is closed.
type fakeAudio struct {
	packets chan []byte
	stop    chan struct{}
	once    sync.Once
}

func newFakeAudio() *fakeAudio {
	a := &fakeAudio{packets: make(chan []byte, 8), stop: make(chan struct{})}
	go func() {
		defer close(a.packets)
		t := time.NewTicker(AudioFrame)
		defer t.Stop()
		for {
			select {
			case <-a.stop:
				return
			case <-t.C:
				select {
				case a.packets <- opusSilence:
				default:
				}
			}
		}
	}()
	return a
}

func (a *fakeAudio) Packets() <-chan []byte { return a.packets }
func (a *fakeAudio) Close() error           { a.once.Do(func() { close(a.stop) }); return nil }

func (a *fakeAudio) closed() bool {
	select {
	case <-a.stop:
		return true
	default:
		return false
	}
}

// TestSessionStreamsAudio: with Start.Audio the session starts the sound
// before the desktop, hands its programs the sound server's address, says
// so in Ready, and a browser that offers an audio line gets Opus RTP.
func TestSessionStreamsAudio(t *testing.T) {
	fake := newFakeAudio()
	asked := make(chan AudioOptions, 1)
	h := startAgent(t, func(o *Options) {
		o.NewAudio = func(_ context.Context, ao AudioOptions) (AudioSource, []string, error) {
			asked <- ao
			return fake, []string{"ROBIN_TEST_SOUND=unix:/nowhere"}, nil
		}
	})
	ready := h.start(func(s *protocol.Start) { s.Audio = true })
	if !ready.Audio || ready.AudioError != "" {
		t.Fatalf("ready reports audio %v (%q)", ready.Audio, ready.AudioError)
	}
	ao := <-asked
	if ao.Attached || !strings.HasPrefix(ao.RunDir, h.root+"/") {
		t.Errorf("NewAudio got %+v; want spawn mode in this session's run dir", ao)
	}
	if !sessionHasEnv(filepath.Join(ao.RunDir, "Xauthority"), "ROBIN_TEST_SOUND=unix:/nowhere") {
		t.Error("no program on the desktop has the sound server's address")
	}

	pc := offerFromBrowser(t, h, webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio)
	tracks := make(chan *webrtc.TrackRemote, 2)
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { tracks <- tr })
	answer(t, h, pc)
	deadline := time.After(15 * time.Second)
	for {
		select {
		case tr := <-tracks:
			if tr.Kind() != webrtc.RTPCodecTypeAudio {
				continue
			}
			if c := tr.Codec(); c.MimeType != webrtc.MimeTypeOpus || c.Channels != 2 || !strings.Contains(c.SDPFmtpLine, "useinbandfec=1") {
				t.Errorf("audio codec %+v, want stereo Opus with in-band FEC", c)
			}
			pkt, _, err := tr.ReadRTP()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(pkt.Payload, opusSilence) {
				t.Errorf("audio payload %x, want the source's packet %x", pkt.Payload, opusSilence)
			}
			next, _, err := tr.ReadRTP()
			if err != nil {
				t.Fatal(err)
			}
			if d := next.Timestamp - pkt.Timestamp; d != 960 {
				t.Errorf("RTP clock advanced %d between packets, want 960 (20 ms at 48 kHz)", d)
			}
			h.stdin.Close()
			h.waitGone(ready.Display)
			if !fake.closed() {
				t.Error("the session left the audio source open")
			}
			return
		case <-deadline:
			t.Fatalf("no audio track; ICE state %s", pc.ICEConnectionState())
		}
	}
}

func TestSessionWithoutAudioSaysWhy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		newAudio NewAudioFunc
		want     string
	}{
		{"no support", nil, "built without audio"},
		{"no server", func(context.Context, AudioOptions) (AudioSource, []string, error) {
			return nil, nil, errors.New("no sound server for the new desktop: install pulseaudio on the host")
		}, "install pulseaudio"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startAgent(t, func(o *Options) { o.NewAudio = tc.newAudio })
			ready := h.start(func(s *protocol.Start) { s.Audio = true })
			if ready.Audio || !strings.Contains(ready.AudioError, tc.want) {
				t.Errorf("ready reports audio %v (%q), want off because %q", ready.Audio, ready.AudioError, tc.want)
			}
			h.stdin.Close()
			h.waitGone(ready.Display)
		})
	}
}

// offerFromBrowser is a Pion peer playing the browser, with a recvonly
// transceiver per kind and the control channel, registered with the harness
// for the agent's candidates.
func offerFromBrowser(t *testing.T, h *harness, kinds ...webrtc.RTPCodecType) *webrtc.PeerConnection {
	t.Helper()
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	h.mu.Lock()
	h.pc = pc
	h.mu.Unlock()
	for _, k := range kinds {
		if _, err := pc.AddTransceiverFromKind(k, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pc.CreateDataChannel(protocol.ControlChannel, nil); err != nil {
		t.Fatal(err)
	}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		h.cli.Send(protocol.Message{Type: protocol.TypeCandidate, Candidate: &protocol.Candidate{
			Candidate: init.Candidate, SDPMid: init.SDPMid, SDPMLineIndex: init.SDPMLineIndex}})
	})
	return pc
}

// answer sends pc's offer through the session and applies the answer.
func answer(t *testing.T, h *harness, pc *webrtc.PeerConnection) {
	t.Helper()
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	pc.SetLocalDescription(offer)
	h.cli.Send(protocol.Message{Type: protocol.TypeOffer, SDP: offer.SDP})
	a := h.recv(protocol.TypeAnswer)
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: a.SDP}); err != nil {
		t.Fatal(err)
	}
}

// sessionHasEnv reports whether some process of the display that uses
// xauthority carries kv in its environment.
func sessionHasEnv(xauthority, kv string) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		ents, _ := os.ReadDir("/proc")
		for _, e := range ents {
			env, err := os.ReadFile(filepath.Join("/proc", e.Name(), "environ"))
			if err != nil {
				continue
			}
			vars := "\x00" + string(env)
			if strings.Contains(vars, "\x00XAUTHORITY="+xauthority+"\x00") && strings.Contains(vars, "\x00"+kv+"\x00") {
				return true
			}
		}
	}
	return false
}
