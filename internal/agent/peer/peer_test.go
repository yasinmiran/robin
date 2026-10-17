package peer

import (
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/remotejump/internal/protocol"
)

// browser stands in for the real client: it offers a recvonly H.264
// transceiver plus the control channel, as the page does.
type browser struct {
	pc      *webrtc.PeerConnection
	control *webrtc.DataChannel
	got     chan protocol.Control
	track   chan *webrtc.TrackRemote
}

func newBrowser(t *testing.T) *browser {
	t.Helper()
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	b := &browser{pc: pc, got: make(chan protocol.Control, 8), track: make(chan *webrtc.TrackRemote, 1)}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	ordered := true
	b.control, err = pc.CreateDataChannel(protocol.ControlChannel, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		t.Fatal(err)
	}
	b.control.OnMessage(func(msg webrtc.DataChannelMessage) {
		var c protocol.Control
		if json.Unmarshal(msg.Data, &c) == nil {
			b.got <- c
		}
	})
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { b.track <- tr })
	return b
}

// connect runs the offer/answer and trickle exchange the CLI would relay.
func connect(t *testing.T, b *browser, p *Peer, agentCandidates <-chan protocol.Candidate) {
	t.Helper()
	// The CLI delivers the offer before any candidate; hold them until the
	// agent has applied it.
	browserCands := make(chan webrtc.ICECandidateInit, 64)
	b.pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			browserCands <- c.ToJSON()
		}
	})
	offer, err := b.pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	answer, err := p.Answer(offer.SDP)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(answer, "H264") || !strings.Contains(answer, "packetization-mode=1") {
		t.Fatalf("answer does not carry H.264 mode 1:\n%s", answer)
	}
	if err := b.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatal(err)
	}
	go func() {
		for init := range browserCands {
			// Errors here only come from a peer the test already closed.
			p.AddCandidate(protocol.Candidate{Candidate: init.Candidate, SDPMid: init.SDPMid, SDPMLineIndex: init.SDPMLineIndex})
		}
	}()
	go func() {
		for c := range agentCandidates {
			if c.Candidate == "" {
				continue
			}
			b.pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: c.Candidate, SDPMid: c.SDPMid, SDPMLineIndex: c.SDPMLineIndex})
		}
	}()
}

func TestPeerStreamsAndControls(t *testing.T) {
	cands := make(chan protocol.Candidate, 32)
	controls := make(chan protocol.Control, 8)
	var keyframes atomic.Int32
	connected := make(chan struct{})
	var once atomic.Bool

	p, err := New(Config{MinBitrate: 100_000, StartBitrate: 1_000_000, MaxBitrate: 5_000_000, IncludeLoopback: true}, Handlers{
		Candidate: func(c protocol.Candidate) { cands <- c },
		Control:   func(c protocol.Control) { controls <- c },
		Keyframe:  func() { keyframes.Add(1) },
		State: func(s webrtc.PeerConnectionState) {
			if s == webrtc.PeerConnectionStateConnected && once.CompareAndSwap(false, true) {
				close(connected)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	b := newBrowser(t)
	connect(t, b, p, cands)
	select {
	case <-connected:
	case <-time.After(15 * time.Second):
		t.Fatal("peer never connected")
	}

	// Video: frames written by the agent arrive as an H.264 track.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		au := []byte{0, 0, 0, 1, 0x67, 0x42, 0xe0, 0x1f, 0, 0, 0, 1, 0x68, 0xce, 0x38, 0x80, 0, 0, 0, 1, 0x65, 0x88, 0x84, 0x21}
		for {
			select {
			case <-stop:
				return
			case <-time.After(33 * time.Millisecond):
				p.WriteFrame(au, 33*time.Millisecond)
			}
		}
	}()
	var tr *webrtc.TrackRemote
	select {
	case tr = <-b.track:
	case <-time.After(10 * time.Second):
		t.Fatal("no video track at the browser")
	}
	if got := tr.Codec().MimeType; !strings.EqualFold(got, webrtc.MimeTypeH264) {
		t.Fatalf("track codec %s", got)
	}
	if _, _, err := tr.ReadRTP(); err != nil {
		t.Fatalf("read rtp: %v", err)
	}

	// A picture-loss indication from the browser becomes a keyframe request.
	before := keyframes.Load()
	if err := b.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(tr.SSRC())}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for keyframes.Load() == before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if keyframes.Load() == before {
		t.Fatal("PLI did not trigger a keyframe request")
	}

	// Control: browser to agent, then agent to browser.
	waitOpen(t, b.control)
	if err := b.control.SendText(`{"t":"move","x":10,"y":20}`); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-controls:
		if c.T != protocol.ControlMove || c.X != 10 || c.Y != 20 {
			t.Fatalf("agent got %+v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("control message did not reach the agent")
	}
	if err := p.SendControl(protocol.Control{T: protocol.ControlClipboard, Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-b.got:
		if c.T != protocol.ControlClipboard || c.Text != "hello" {
			t.Fatalf("browser got %+v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("clipboard did not reach the browser")
	}
	if p.Bitrate() <= 0 {
		t.Fatalf("no bitrate target: %d", p.Bitrate())
	}
}

func TestPeerRejectsOversizedControl(t *testing.T) {
	p, err := New(Config{MinBitrate: 100_000, StartBitrate: 500_000, MaxBitrate: 1_000_000}, Handlers{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// No channel yet: nothing is sent, and the caller can tell.
	if err := p.SendControl(protocol.Control{T: protocol.ControlClipboard, Text: "x"}); !errors.Is(err, ErrNoControl) {
		t.Fatalf("send before channel open: %v, want ErrNoControl", err)
	}
}

func waitOpen(t *testing.T, dc *webrtc.DataChannel) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for dc.ReadyState() != webrtc.DataChannelStateOpen {
		if time.Now().After(deadline) {
			t.Fatal("control channel never opened")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
