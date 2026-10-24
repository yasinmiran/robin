package peer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/transport/v5"
	"github.com/pion/transport/v5/stdnet"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/yasinmiran/robin/internal/protocol"
)

// TestBulkRefusalReasonIsBounded has the browser refuse a transfer with a
// reason as long as a text frame can carry: what reaches the caller, and
// from there robin-send's terminal and the agent's log, is cut to the
// bound the agent keeps for its own reasons.
func TestBulkRefusalReasonIsBounded(t *testing.T) {
	bp := newBulkPair(t, Config{}, Handlers{})
	bp.raw.Store(true)
	result := make(chan error, 1)
	go func() {
		result <- bp.agent.SendBulk(context.Background(), protocol.Bulk{Kind: testKind, ID: "long", Size: 10}, bytes.NewReader(make([]byte, 10)))
	}()
	var far *webrtc.DataChannel
	select {
	case far = <-bp.browserChannels:
	case <-time.After(10 * time.Second):
		t.Fatal("the channel never reached the browser")
	}
	waitOpen(t, far)
	sendJSON(t, far, protocol.Bulk{T: protocol.BulkError, Error: strings.Repeat("x", 60<<10)})
	select {
	case err := <-result:
		var r *BulkRefusedError
		if !errors.As(err, &r) {
			t.Fatalf("sender: %v, want a refusal", err)
		}
		if len(r.Reason) == 0 || len(r.Reason) > maxBulkReason {
			t.Errorf("the refusal's reason holds %d bytes, want 1 to %d", len(r.Reason), maxBulkReason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sender never finished")
	}
}

// mdnsWatch is the host network, except that it records every address on
// the mDNS port the peer resolves or listens on, and binds none of them.
type mdnsWatch struct {
	transport.Net
	mu   sync.Mutex
	seen []string
}

func (n *mdnsWatch) note(addr string) {
	n.mu.Lock()
	n.seen = append(n.seen, addr)
	n.mu.Unlock()
}

func (n *mdnsWatch) ResolveUDPAddr(network, address string) (*net.UDPAddr, error) {
	if strings.HasSuffix(address, ":5353") {
		n.note(address)
	}
	return n.Net.ResolveUDPAddr(network, address)
}

func (n *mdnsWatch) ListenUDP(network string, laddr *net.UDPAddr) (transport.UDPConn, error) {
	if laddr != nil && laddr.Port == 5353 {
		n.note(laddr.String())
		return nil, errors.New("mdnsWatch: not binding the mDNS port")
	}
	return n.Net.ListenUDP(network, laddr)
}

// connectedPair connects a peer built with cfg to a stand-in browser.
func connectedPair(t *testing.T, cfg Config, h Handlers) (*Peer, *browser) {
	t.Helper()
	cands := make(chan protocol.Candidate, 32)
	connected := make(chan struct{})
	var once sync.Once
	h.Candidate = func(c protocol.Candidate) { cands <- c }
	h.State = func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			once.Do(func() { close(connected) })
		}
	}
	cfg.MinBitrate, cfg.StartBitrate, cfg.MaxBitrate, cfg.IncludeLoopback = 100_000, 1_000_000, 5_000_000, true
	p, err := New(cfg, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	b := newBrowser(t)
	connect(t, b, p, cands)
	select {
	case <-connected:
	case <-time.After(15 * time.Second):
		t.Fatal("peer never connected")
	}
	return p, b
}

// TestPeerLeavesMDNSAlone checks that the agent neither resolves the
// browser's .local names nor listens for mDNS: whoever answers a query on
// the host's network would choose where its connectivity checks go, and it
// reaches the browser as a peer-reflexive candidate anyway.
func TestPeerLeavesMDNSAlone(t *testing.T) {
	host, err := stdnet.NewNet()
	if err != nil {
		t.Fatal(err)
	}
	watch := &mdnsWatch{Net: host}
	p, _ := connectedPair(t, Config{Net: watch}, Handlers{})
	name := "candidate:1 1 udp 2130706431 0f8d3d6c-4a5e-4b7c-9c1e-2d3f4a5b6c7d.local 50000 typ host"
	if err := p.AddCandidate(protocol.Candidate{Candidate: name, SDPMid: ptr("0")}); err != nil {
		t.Errorf("AddCandidate(%s): %v", name, err)
	}
	time.Sleep(200 * time.Millisecond)
	watch.mu.Lock()
	defer watch.mu.Unlock()
	if len(watch.seen) != 0 {
		t.Errorf("the peer went for mDNS at %v", watch.seen)
	}
}

func ptr[T any](v T) *T { return &v }

// hostCandidate is a host candidate on a loopback port nothing listens on,
// so the checks the agent sends to it go nowhere.
func hostCandidate(port int) protocol.Candidate {
	return protocol.Candidate{
		Candidate: fmt.Sprintf("candidate:%d 1 udp 2130706431 127.0.0.1 %d typ host", port, port),
		SDPMid:    ptr("0"),
	}
}

// TestPeerBoundsRemoteCandidates names more candidates than any browser
// gathers: those past the bound are dropped, a repeat is no new one, an
// ICE restart starts the count again, and an offer that names too many
// on its own is refused before it applies.
func TestPeerBoundsRemoteCandidates(t *testing.T) {
	p, err := New(Config{MinBitrate: 100_000, StartBitrate: 1_000_000, MaxBitrate: 5_000_000, IncludeLoopback: true}, Handlers{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	// The browser's own candidates stay with it, so the count is the test's.
	b := newBrowser(t)
	offer := func(o *webrtc.OfferOptions) string {
		offer, err := b.pc.CreateOffer(o)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.pc.SetLocalDescription(offer); err != nil {
			t.Fatal(err)
		}
		return offer.SDP // before the browser's gathering adds its own
	}
	answer, err := p.Answer(offer(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatal(err)
	}
	for i := range maxRemoteCandidates - 1 {
		if err := p.AddCandidate(hostCandidate(41000 + i)); err != nil {
			t.Fatalf("candidate %d: %v", i+1, err)
		}
	}
	if err := p.AddCandidate(hostCandidate(41000)); err != nil {
		t.Errorf("a candidate named again: %v", err)
	}
	if err := p.AddCandidate(protocol.Candidate{}); err != nil {
		t.Errorf("the end of candidates: %v", err)
	}
	if err := p.AddCandidate(hostCandidate(42000)); err != nil {
		t.Errorf("candidate %d, the last allowed: %v", maxRemoteCandidates, err)
	}
	if err := p.AddCandidate(hostCandidate(42001)); !errors.Is(err, ErrTooManyCandidates) {
		t.Errorf("candidate %d: %v, want ErrTooManyCandidates", maxRemoteCandidates+1, err)
	}

	restart := &webrtc.OfferOptions{ICERestart: true}
	if answer, err = p.Answer(offer(restart)); err != nil {
		t.Fatal(err)
	}
	if err := b.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatal(err)
	}
	if err := p.AddCandidate(hostCandidate(42001)); err != nil {
		t.Errorf("a candidate after an ICE restart: %v", err)
	}

	sdp := offer(restart)
	var lines strings.Builder
	for i := range maxRemoteCandidates + 1 {
		fmt.Fprintf(&lines, "a=%s\r\n", hostCandidate(43000+i).Candidate)
	}
	at := strings.Index(sdp, "a=mid:0\r\n")
	if at < 0 {
		t.Fatal("no mid 0 in the offer")
	}
	if _, err := p.Answer(sdp[:at] + lines.String() + sdp[at:]); err == nil || !strings.Contains(err.Error(), "candidates") {
		t.Errorf("an offer naming %d candidates: %v, want a refusal", maxRemoteCandidates+1, err)
	}
}

// TestPeerNeverDialsTCP names a passive TCP candidate, which Pion would
// connect to: the agent takes ICE-TCP only through its own listener, and
// connecting out would let the page aim it at any port the host reaches.
func TestPeerNeverDialsTCP(t *testing.T) {
	muxLn, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := ice.NewTCPMuxDefault(ice.TCPMuxParams{Listener: muxLn, ReadBufferSize: 64})
	t.Cleanup(func() { mux.Close() })
	p, _ := connectedPair(t, Config{TCPMux: mux}, Handlers{})

	target, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	dialed := make(chan struct{}, 1)
	go func() {
		if c, err := target.Accept(); err == nil {
			c.Close()
			dialed <- struct{}{}
		}
	}()
	port := target.Addr().(*net.TCPAddr).Port
	c := protocol.Candidate{
		Candidate: fmt.Sprintf("candidate:9 1 tcp 1518280447 127.0.0.1 %d typ host tcptype passive", port),
		SDPMid:    ptr("0"),
	}
	if err := p.AddCandidate(c); err != nil {
		t.Fatalf("AddCandidate: %v", err)
	}
	select {
	case <-dialed:
		t.Error("the agent connected to a TCP port the page named")
	case <-time.After(3 * time.Second):
	}
}

// idle is a bulk channel the browser opened and sends nothing on, with the
// agent's verdict on it and its close.
type idle struct {
	dc      *webrtc.DataChannel
	verdict chan protocol.Bulk
	closed  chan struct{}
}

func idleBulk(t *testing.T, b *browser) *idle {
	t.Helper()
	dc, err := b.pc.CreateDataChannel(protocol.BulkChannel, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &idle{dc: dc, verdict: make(chan protocol.Bulk, 1), closed: make(chan struct{})}
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		if v, err := protocol.ParseBulk(m.Data); m.IsString && err == nil {
			select {
			case c.verdict <- v:
			default:
			}
		}
	})
	dc.OnClose(func() { close(c.closed) })
	return c
}

// TestBulkChannelsWithoutAHeaderAreBounded opens far more bulk channels
// than the page ever runs at once and sends nothing on them: the agent
// keeps maxWaitingBulk waiting for a header and refuses the rest, a
// transfer too while the slots are full, and takes one again once a slot
// is free.
func TestBulkChannelsWithoutAHeaderAreBounded(t *testing.T) {
	bp := newBulkPair(t, Config{}, Handlers{})
	const opened = 4 * maxWaitingBulk
	var all []*idle
	for range opened {
		all = append(all, idleBulk(t, bp.browser))
	}
	var open []*idle
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		open = open[:0]
		for _, c := range all {
			select {
			case <-c.closed:
			default:
				open = append(open, c)
			}
		}
		if len(open) < maxWaitingBulk {
			t.Fatalf("%d idle bulk channels left open, fewer than the %d the agent waits for", len(open), maxWaitingBulk)
		}
		if len(open) == maxWaitingBulk {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d idle bulk channels still open, want %d", len(open), opened, maxWaitingBulk)
		}
	}

	var refused *BulkRefusedError
	err := bp.upload(context.Background(), t, protocol.Bulk{Kind: testKind, ID: "late", Size: 10}, bytes.NewReader(make([]byte, 10)))
	if !errors.As(err, &refused) || !strings.Contains(refused.Reason, "header") {
		t.Errorf("a transfer while every slot waits: %v, want a refusal", err)
	}
	for _, c := range all {
		select {
		case <-c.closed:
			select {
			case v := <-c.verdict:
				if v.T != protocol.BulkError {
					t.Errorf("a refused channel's verdict %+v", v)
				}
			default:
				t.Error("a channel refused without a verdict")
			}
		default:
		}
	}

	open[0].dc.Close()
	<-open[0].closed
	// The agent hears of the close after the browser does.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		err := bp.upload(context.Background(), t, protocol.Bulk{Kind: testKind, ID: "again", Size: 10}, bytes.NewReader(make([]byte, 10)))
		if err == nil {
			break
		}
		if !errors.As(err, &refused) || time.Now().After(deadline) {
			t.Fatalf("a transfer once a slot is free: %v", err)
		}
	}
	if err := receiveID(t, bp.atAgent, "again").end(t); err != nil {
		t.Errorf("the sink ended with %v", err)
	}
	for _, c := range open[1:] {
		select {
		case <-c.closed:
			t.Fatal("the agent closed a channel within the bound")
		default:
		}
	}
}

// TestBulkHeaderDeadline leaves one bulk channel idle past the deadline,
// which the agent closes with a refusal, while a transfer that sent its
// header in time may take as long as it likes over the rest.
func TestBulkHeaderDeadline(t *testing.T) {
	saved := bulkHeaderWait
	bulkHeaderWait = 300 * time.Millisecond
	t.Cleanup(func() { bulkHeaderWait = saved })
	bp := newBulkPair(t, Config{}, Handlers{})

	c := idleBulk(t, bp.browser)
	select {
	case <-c.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent kept a bulk channel without a header")
	}
	select {
	case v := <-c.verdict:
		if v.T != protocol.BulkError || !strings.Contains(v.Error, "header") {
			t.Errorf("verdict %+v, want an error about the header", v)
		}
	default:
		t.Error("the agent closed the channel without a verdict")
	}

	dc, frames, done := bp.rawBulk(t)
	payload := []byte("slow but sure")
	sendJSON(t, dc, protocol.Bulk{T: protocol.BulkHeader, Kind: testKind, ID: "slow", Size: int64(len(payload))})
	s := receiveID(t, bp.atAgent, "slow")
	time.Sleep(3 * bulkHeaderWait)
	if err := dc.Send(payload); err != nil {
		t.Fatal(err)
	}
	sendJSON(t, dc, protocol.Bulk{T: protocol.BulkEnd, SHA256: sumOf(payload)})
	select {
	case v := <-frames:
		if v.T != protocol.BulkDone {
			t.Errorf("slow transfer: verdict %+v, want done", v)
		}
	case <-done:
		t.Fatal("the agent closed a transfer that had sent its header")
	case <-time.After(10 * time.Second):
		t.Fatal("no verdict on the slow transfer")
	}
	if err := s.end(t); err != nil {
		t.Errorf("slow transfer: sink ended with %v", err)
	}
}

// TestSecondControlChannelClosesFirst opens a second control channel from
// the same page: the agent lets the first go, so only one ever dispatches,
// and sends on the second.
func TestSecondControlChannelClosesFirst(t *testing.T) {
	bp := newBulkPair(t, Config{}, Handlers{})
	first := bp.browser.control
	firstClosed := make(chan struct{})
	first.OnClose(func() { close(firstClosed) })
	ordered := true
	second, err := bp.browser.pc.CreateDataChannel(protocol.ControlChannel, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan protocol.Control, 4)
	second.OnMessage(func(msg webrtc.DataChannelMessage) {
		var c protocol.Control
		if json.Unmarshal(msg.Data, &c) == nil {
			got <- c
		}
	})
	waitOpen(t, second)
	select {
	case <-firstClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("the first control channel stayed open beside the second")
	}
	if err := bp.agent.SendControl(protocol.Control{T: protocol.ControlRelease}); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-got:
		if c.T != protocol.ControlRelease {
			t.Errorf("second channel got %+v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SendControl never reached the second channel")
	}
}

// receives reports whether an answer's media section lets the page send
// media on it.
func receives(section string) bool {
	return !strings.HasPrefix(section, "application") &&
		(strings.Contains(section, "a=recvonly") || strings.Contains(section, "a=sendrecv"))
}

// line is one media line of a page: its kind and direction.
type line struct {
	kind webrtc.RTPCodecType
	dir  webrtc.RTPTransceiverDirection
}

// mediaPage builds a page with lines, in order, and a control channel; it
// returns the page and the tracks it sends.
func mediaPage(t *testing.T, lines ...line) (*sendingBrowser, []*webrtc.TrackLocalStaticSample) {
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
	var sending []*webrtc.TrackLocalStaticSample
	for i, l := range lines {
		if l.dir == webrtc.RTPTransceiverDirectionRecvonly {
			if _, err := pc.AddTransceiverFromKind(l.kind, webrtc.RTPTransceiverInit{Direction: l.dir}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		c := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
		if l.kind == webrtc.RTPCodecTypeVideo {
			c = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: H264Fmtp}
		}
		tr, err := webrtc.NewTrackLocalStaticSample(c, fmt.Sprintf("line%d", i), "page")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pc.AddTransceiverFromTrack(tr, webrtc.RTPTransceiverInit{Direction: l.dir}); err != nil {
			t.Fatal(err)
		}
		sending = append(sending, tr)
	}
	if _, err := pc.CreateDataChannel(protocol.ControlChannel, nil); err != nil {
		t.Fatal(err)
	}
	return &sendingBrowser{pc: pc}, sending
}

// TestPeerRefusesInboundMedia offers the agent media it did not ask for:
// a microphone and a camera to a peer that takes neither, extra lines of
// each to one that takes one of each, and media on the agent's own lines.
// The answer lets the page send on the lines the peer takes and on the
// agent's own, which it cannot refuse without their sending side, and
// only the tracks the peer takes reach Handlers.Track.
func TestPeerRefusesInboundMedia(t *testing.T) {
	const (
		audio = webrtc.RTPCodecTypeAudio
		video = webrtc.RTPCodecTypeVideo
		recv  = webrtc.RTPTransceiverDirectionRecvonly
		send  = webrtc.RTPTransceiverDirectionSendonly
		both  = webrtc.RTPTransceiverDirectionSendrecv
	)
	for _, tc := range []struct {
		name       string
		cfg        Config
		lines      []line
		allowed    map[int]bool // answer sections that may take media
		wantTracks int
	}{
		{name: "neither taken", lines: []line{{video, recv}, {video, send}, {video, send}}},
		{
			name: "one of each taken", cfg: Config{Mic: true, Camera: true},
			lines:   []line{{video, recv}, {audio, both}, {video, send}, {audio, send}, {video, send}},
			allowed: map[int]bool{1: true, 2: true}, wantTracks: 2,
		},
		{
			name: "media on the agent's own lines", cfg: Config{Audio: true},
			lines:   []line{{video, both}, {audio, both}},
			allowed: map[int]bool{0: true, 1: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cands := make(chan protocol.Candidate, 32)
			tracks := make(chan *webrtc.TrackRemote, 8)
			tc.cfg.MinBitrate, tc.cfg.StartBitrate, tc.cfg.MaxBitrate, tc.cfg.IncludeLoopback = 100_000, 1_000_000, 5_000_000, true
			p, err := New(tc.cfg, Handlers{
				Candidate: func(c protocol.Candidate) { cands <- c },
				Track:     func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { tracks <- tr },
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { p.Close() })
			b, sending := mediaPage(t, tc.lines...)
			answer := exchange(t, b, p, cands)
			for i, s := range mediaSections(answer) {
				if receives(s) && !tc.allowed[i] {
					t.Errorf("the answer takes media on line %d:\n%s", i, s)
				}
			}

			stop := make(chan struct{})
			var writers sync.WaitGroup
			t.Cleanup(func() {
				close(stop)
				writers.Wait()
			})
			writers.Go(func() {
				for {
					select {
					case <-stop:
						return
					case <-time.After(20 * time.Millisecond):
					}
					for _, tr := range sending {
						data := []byte{0xf8, 0xff, 0xfe}
						if tr.Kind() == video {
							data = []byte{0, 0, 0, 1, 0x65, 0x88, 0x84, 0x21}
						}
						tr.WriteSample(media.Sample{Data: data, Duration: 20 * time.Millisecond})
					}
				}
			})
			got := 0
			for timeout := time.After(4 * time.Second); ; {
				select {
				case <-tracks:
					got++
					continue
				case <-timeout:
				}
				break
			}
			if got != tc.wantTracks {
				t.Errorf("%d tracks reached Handlers.Track, want %d", got, tc.wantTracks)
			}
		})
	}
}
