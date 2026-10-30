package peer

import (
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/yasinmiran/robin/internal/protocol"
)

// sendingBrowser is the page with its microphone and camera on: video in,
// audio both ways, and a video line of its own out, in the page's order.
type sendingBrowser struct {
	pc        *webrtc.PeerConnection
	mic       *webrtc.TrackLocalStaticSample
	camera    *webrtc.TrackLocalStaticRTP
	micSender *webrtc.RTPSender
	camSender *webrtc.RTPSender
}

func newSendingBrowser(t *testing.T) *sendingBrowser {
	t.Helper()
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	reg := &interceptor.Registry{}
	if err := webrtc.ConfigureTWCCHeaderExtensionSender(m, reg); err != nil {
		t.Fatal(err)
	}
	if err := webrtc.ConfigureRTCPReports(reg); err != nil {
		t.Fatal(err)
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(reg)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	b := &sendingBrowser{pc: pc}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	if b.mic, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "mic", "page"); err != nil {
		t.Fatal(err)
	}
	micLine, err := pc.AddTransceiverFromTrack(b.mic, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv})
	if err != nil {
		t.Fatal(err)
	}
	b.micSender = micLine.Sender()
	if b.camera, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: H264Fmtp,
	}, "camera", "page"); err != nil {
		t.Fatal(err)
	}
	tr, err := pc.AddTransceiverFromTrack(b.camera, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	if err != nil {
		t.Fatal(err)
	}
	b.camSender = tr.Sender()
	if _, err := pc.CreateDataChannel(protocol.ControlChannel, nil); err != nil {
		t.Fatal(err)
	}
	return b
}

// exchange runs the offer, answer and candidates between b and p.
func exchange(t *testing.T, b *sendingBrowser, p *Peer, cands <-chan protocol.Candidate) string {
	t.Helper()
	b.pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			init := c.ToJSON()
			p.AddCandidate(protocol.Candidate{Candidate: init.Candidate, SDPMid: init.SDPMid, SDPMLineIndex: init.SDPMLineIndex})
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
	if err := b.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatal(err)
	}
	go func() {
		for c := range cands {
			if c.Candidate != "" {
				b.pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: c.Candidate, SDPMid: c.SDPMid, SDPMLineIndex: c.SDPMLineIndex})
			}
		}
	}()
	return answer
}

// mediaSections splits an SDP into its m= sections.
func mediaSections(sdp string) []string {
	parts := strings.Split(sdp, "\nm=")
	return parts[1:]
}

// TestPeerTakesTheMicrophoneAndCamera: the agent answers the page's lines
// so that the page may send on both, hands the tracks to Handlers.Track,
// and gives the page what a receiver owes it: the camera's sender a NACK
// for a lost packet and a PLI on request, and the transport its feedback.
func TestPeerTakesTheMicrophoneAndCamera(t *testing.T) {
	cands := make(chan protocol.Candidate, 32)
	tracks := make(chan *webrtc.TrackRemote, 4)
	var cameraSequenced atomic.Bool
	p, err := New(Config{
		MinBitrate: 100_000, StartBitrate: 1_000_000, MaxBitrate: 5_000_000, IncludeLoopback: true,
		Audio: true, Mic: true, Camera: true,
	}, Handlers{
		Candidate: func(c protocol.Candidate) { cands <- c },
		Track: func(tr *webrtc.TrackRemote, r *webrtc.RTPReceiver) {
			var transportCC uint8
			for _, e := range r.GetParameters().HeaderExtensions {
				if tr.Kind() == webrtc.RTPCodecTypeVideo && e.URI == sdp.TransportCCURI {
					transportCC = uint8(e.ID)
				}
			}
			go func() {
				for {
					pkt, _, err := tr.ReadRTP()
					if err != nil {
						return
					}
					if transportCC != 0 && pkt.GetExtension(transportCC) != nil {
						cameraSequenced.Store(true)
					}
				}
			}()
			tracks <- tr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	b := newSendingBrowser(t)
	answer := exchange(t, b, p, cands)
	ms := mediaSections(answer)
	if len(ms) != 4 {
		t.Fatalf("answer has %d media sections:\n%s", len(ms), answer)
	}
	for i, want := range []string{"a=sendonly", "a=sendrecv", "a=recvonly"} {
		if !strings.Contains(ms[i], want) {
			t.Errorf("section %d lacks %s:\n%s", i, want, ms[i])
		}
	}
	if !strings.Contains(ms[2], "H264") || !strings.Contains(ms[2], "nack") || !strings.Contains(ms[2], "transport-wide-cc") {
		t.Errorf("the camera's line does not take H.264 with NACK and transport-wide sequence numbers:\n%s", ms[2])
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	// Closing the page's connection ends a goroutine blocked in ReadRTCP.
	defer func() {
		close(stop)
		b.pc.Close()
		wg.Wait()
	}()
	// lose takes the camera's next packet once the test asks for it. A
	// packet dropped before the camera reaches the agent is never missed,
	// so it asks only after Handlers.Track has the camera.
	lose := make(chan uint16)
	wg.Go(func() {
		seq := uint16(1)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			b.mic.WriteSample(media.Sample{Data: []byte{0xf8, 0xff, 0xfe}, Duration: 20 * time.Millisecond})
			select {
			case lose <- seq:
				seq++ // lost on the way
			default:
			}
			b.camera.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(i) * 3000, Marker: true},
				Payload: []byte{0x65, 0x88, 0x84, 0x21},
			})
			seq++
		}
	})

	got := map[webrtc.RTPCodecType]*webrtc.TrackRemote{}
	for len(got) < 2 {
		select {
		case tr := <-tracks:
			got[tr.Kind()] = tr
		case <-time.After(15 * time.Second):
			t.Fatalf("tracks at the agent: %v", got)
		}
	}
	if c := got[webrtc.RTPCodecTypeAudio].Codec().MimeType; !strings.EqualFold(c, webrtc.MimeTypeOpus) {
		t.Errorf("microphone codec %s", c)
	}
	if c := got[webrtc.RTPCodecTypeVideo].Codec().MimeType; !strings.EqualFold(c, webrtc.MimeTypeH264) {
		t.Errorf("camera codec %s", c)
	}

	lost := <-lose
	if err := p.RequestKeyframe(uint32(got[webrtc.RTPCodecTypeVideo].SSRC())); err != nil {
		t.Fatal(err)
	}
	type heard struct {
		camera bool
		pkts   []rtcp.Packet
	}
	rtcpIn := make(chan heard)
	// Transport-wide feedback names the SSRC of whichever packet the agent
	// recorded last, so it may reach the microphone's sender instead of the
	// camera's.
	for _, s := range []*webrtc.RTPSender{b.micSender, b.camSender} {
		wg.Go(func() {
			for {
				pkts, _, err := s.ReadRTCP()
				if err != nil {
					return
				}
				select {
				case rtcpIn <- heard{s == b.camSender, pkts}:
				case <-stop:
					return
				}
			}
		})
	}
	var pli, nack, twcc bool
	deadline := time.After(10 * time.Second)
wait:
	for !(pli && nack && twcc && cameraSequenced.Load()) {
		var h heard
		select {
		case h = <-rtcpIn:
		case <-deadline:
			break wait
		}
		for _, pkt := range h.pkts {
			if _, ok := pkt.(*rtcp.TransportLayerCC); ok {
				twcc = true
				continue
			}
			if !h.camera {
				continue
			}
			switch pkt := pkt.(type) {
			case *rtcp.PictureLossIndication:
				pli = true
			case *rtcp.TransportLayerNack:
				for _, n := range pkt.Nacks {
					nack = nack || slices.Contains(n.PacketList(), lost)
				}
			}
		}
	}
	if !pli || !nack || !twcc {
		t.Errorf("the camera's sender heard PLI %v and a NACK for packet %d %v; the page heard transport-wide feedback %v; want all three", pli, lost, nack, twcc)
	}
	if !cameraSequenced.Load() {
		t.Error("the camera's packets reached the agent without transport-wide sequence numbers")
	}
}

// TestPeerTakesAMicrophoneWithoutSound: with no desktop sound to send, the
// agent still answers the audio line so the page may send on it.
func TestPeerTakesAMicrophoneWithoutSound(t *testing.T) {
	p, err := New(Config{MinBitrate: 100_000, StartBitrate: 500_000, MaxBitrate: 1_000_000, Mic: true}, Handlers{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	b := newSendingBrowser(t)
	answer := exchange(t, b, p, make(chan protocol.Candidate))
	ms := mediaSections(answer)
	if len(ms) < 3 || !strings.Contains(ms[1], "opus") || !strings.Contains(ms[1], "a=recvonly") {
		t.Errorf("audio line answered as:\n%s", answer)
	}
}
