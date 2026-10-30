//go:build linux

package agent

import (
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/yasinmiran/robin/internal/agent/track"
	"github.com/yasinmiran/robin/internal/protocol"
)

// Control is the owner's to hand out while the owner can see who has it.
// A guest given control keeps none once the owner's page goes: no page is
// left to take it back from.
func TestTheOwnersPageGoingTakesControlFromGuests(t *testing.T) {
	desk := &fakeTypist{}
	s := guestSession(desk)
	ownerConn, _ := guestConn(t)
	s.conn = newController(ownerConn)
	endGuestsAtCleanup(t, s)
	if _, err := s.currentPeer(); err != nil {
		t.Fatal(err)
	}
	g, _ := addGuest(t, s, "alice", 1001)
	v := pageOf(t, s, g)
	s.inputMu.Lock()
	s.applyGuest(protocol.Control{Guest: &protocol.GuestChange{ID: g.id, Action: protocol.GuestGrant}})
	s.inputMu.Unlock()
	if !g.control.Load() {
		t.Fatal("the grant gave no control")
	}

	if _, err := s.handle(protocol.Message{Type: protocol.TypeHangup}); err != nil {
		t.Fatal(err)
	}
	if s.ownerPeer() != nil {
		t.Fatal("the owner's page is still the session's")
	}
	if g.control.Load() {
		t.Error("the guest keeps control with no owner's page to take it back")
	}
	desk.mu.Lock()
	desk.inputs = nil
	desk.mu.Unlock()
	s.controlFrom(v)(protocol.Control{T: protocol.ControlKey, Key: "x", Code: "KeyX", Down: true})
	waitIdle(t, &s.typing)
	for _, m := range desk.got() {
		if m.T == protocol.ControlKey {
			t.Fatalf("the guest typed with the owner's page gone: %+v", desk.got())
		}
	}
}

// turnMic is a fakeMic whose tracks take turns, as audio.Mic's do: a
// second Play waits for the first to end.
type turnMic struct {
	*fakeMic
	turn    sync.Mutex
	entered chan struct{}
}

func (m *turnMic) Play(r track.Reader) error {
	m.turn.Lock()
	defer m.turn.Unlock()
	select {
	case m.entered <- struct{}{}:
	default:
	}
	return m.fakeMic.Play(r)
}

// micTracks connects a stand-in page sending n microphone tracks to a
// receiving peer, and returns the tracks as the receiver gets them, each
// fed a silence packet every 20 ms until the test ends.
func micTracks(t *testing.T, n int) []*webrtc.TrackRemote {
	t.Helper()
	var se webrtc.SettingEngine
	se.SetIncludeLoopbackCandidate(true)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	send, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { send.Close() })
	recv, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { recv.Close() })
	got := make(chan *webrtc.TrackRemote, n)
	recv.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { got <- tr })
	var locals []*webrtc.TrackLocalStaticSample
	for i := range n {
		l, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "mic", "page"+string(rune('a'+i)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := send.AddTrack(l); err != nil {
			t.Fatal(err)
		}
		locals = append(locals, l)
	}
	exchange := func(from, to *webrtc.PeerConnection, sdp webrtc.SessionDescription) {
		done := webrtc.GatheringCompletePromise(from)
		if err := from.SetLocalDescription(sdp); err != nil {
			t.Fatal(err)
		}
		<-done
		if err := to.SetRemoteDescription(*from.LocalDescription()); err != nil {
			t.Fatal(err)
		}
	}
	offer, err := send.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	exchange(send, recv, offer)
	answer, err := recv.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	exchange(recv, send, answer)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			for _, l := range locals {
				l.WriteSample(media.Sample{Data: opusSilence, Duration: 20 * time.Millisecond})
			}
		}
	}()
	var trs []*webrtc.TrackRemote
	for range n {
		select {
		case tr := <-got:
			trs = append(trs, tr)
		case <-time.After(15 * time.Second):
			t.Fatalf("the receiver got %d of %d tracks", len(trs), n)
		}
	}
	return trs
}

// A guest without control reaches nothing of the microphone: its track,
// there before the owner's, does not keep the owner's from being heard.
func TestAGuestWithoutControlHoldsNoMicrophone(t *testing.T) {
	s := guestSession(&fakeTypist{})
	mic := &turnMic{fakeMic: newFakeMic("the private pulseaudio"), entered: make(chan struct{}, 4)}
	s.mic = mic
	endGuestsAtCleanup(t, s)
	g, _ := addGuest(t, s, "alice", 1001)
	v := pageOf(t, s, g)
	trs := micTracks(t, 2)

	s.track(v, trs[0], nil)
	select {
	case <-mic.entered:
	case <-time.After(time.Second):
	}
	s.track(nil, trs[1], nil)
	select {
	case <-mic.packets:
	case <-time.After(5 * time.Second):
		t.Fatal("the owner's microphone is not heard while a guest without control has a track open")
	}
}
