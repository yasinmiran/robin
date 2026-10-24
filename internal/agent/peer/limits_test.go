package peer

import (
	"bytes"
	"context"
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
