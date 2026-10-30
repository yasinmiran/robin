//go:build linux

package agent

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/robin/internal/agent/files"
	"github.com/yasinmiran/robin/internal/agent/peer"
	"github.com/yasinmiran/robin/internal/protocol"
)

// peerBrowser connects a stand-in browser to a peer of s over loopback,
// with no desktop or CLI: the transfers alone.
func peerBrowser(t *testing.T, s *session) *fileBrowser {
	t.Helper()
	return pageBrowser(t, s, nil)
}

// pageBrowser is peerBrowser for the page of guest g, or the owner's for
// a nil g.
func pageBrowser(t *testing.T, s *session, g *guest) *fileBrowser {
	t.Helper()
	// The candidate relays end with the test, before the peers close.
	quit := t.Context().Done()
	cands := make(chan protocol.Candidate, 64)
	v := newViewer(1, g)
	v.sub = newSubscriber(startBitrate, g == nil)
	cfg := peer.Config{IncludeLoopback: true, MinBitrate: minBitrate, StartBitrate: startBitrate, MaxBitrate: startBitrate, Log: s.log}
	if s.stream != nil {
		cfg.Video = s.stream.video
	}
	p, err := peer.New(cfg,
		peer.Handlers{
			Candidate: func(c protocol.Candidate) {
				select {
				case cands <- c:
				case <-quit:
				}
			},
			Control:     s.controlFrom(v),
			Bulk:        func(h protocol.Bulk) (peer.BulkSink, error) { return s.bulkFrom(v, h) },
			ControlOpen: func() { s.resendOffers(v) },
		})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	v.peer, v.sub.peer = p, p
	s.mu.Lock()
	if g == nil {
		s.owner = v
	} else {
		g.view = v
	}
	s.mu.Unlock()

	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	dc, err := pc.CreateDataChannel(protocol.ControlChannel, nil)
	if err != nil {
		t.Fatal(err)
	}
	browserCands := make(chan webrtc.ICECandidateInit, 64)
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		select {
		case browserCands <- c.ToJSON():
		case <-quit:
		}
	})
	offer, _ := pc.CreateOffer(nil)
	pc.SetLocalDescription(offer)
	answer, err := p.Answer(offer.SDP)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			select {
			case c := <-browserCands:
				p.AddCandidate(protocol.Candidate{Candidate: c.Candidate, SDPMid: c.SDPMid, SDPMLineIndex: c.SDPMLineIndex})
			case <-quit:
				return
			}
		}
	}()
	go func() {
		for {
			select {
			case c := <-cands:
				if c.Candidate != "" {
					pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: c.Candidate, SDPMid: c.SDPMid, SDPMLineIndex: c.SDPMLineIndex})
				}
			case <-quit:
				return
			}
		}
	}()
	return wrapBrowser(t, pc, dc)
}

// Uploads that go wrong in transit leave nothing behind: a corrupt one, a
// truncated one and one the browser gives up on.
func TestUploadsThatFailLeaveNothing(t *testing.T) {
	s, _, _ := sharedSession(t, &fakeDesktop{})
	b := peerBrowser(t, s)
	downloads := s.files.inbox.Dir()

	good := noise(150 << 10)
	if v := b.upload(protocol.Bulk{Kind: protocol.BulkFile, ID: "ok", Name: "kept.bin", Size: int64(len(good))}, good, nil, 0); v.T != protocol.BulkDone {
		t.Fatalf("a good upload: %+v", v)
	}
	if saved := b.control(protocol.ControlFileSaved).File; saved.ID != "ok" || saved.Name != "kept.bin" {
		t.Errorf("saved %+v", saved)
	}
	corrupt := func(e *protocol.Bulk) { e.SHA256 = strings.Repeat("f", 64) }
	if v := b.upload(protocol.Bulk{Kind: protocol.BulkFile, Name: "corrupt.bin", Size: int64(len(good))}, good, corrupt, 0); !strings.Contains(v.Error, "SHA-256 mismatch") {
		t.Errorf("a corrupt upload: %+v", v)
	}
	if v := b.upload(protocol.Bulk{Kind: protocol.BulkFile, Name: "short.bin", Size: int64(len(good)) + 1}, good, nil, 0); !strings.Contains(v.Error, "ended early") {
		t.Errorf("a truncated upload: %+v", v)
	}
	b.upload(protocol.Bulk{Kind: protocol.BulkFile, Name: "gone.bin", Size: 1 << 20}, noise(1<<20), nil, 2)
	if v := b.upload(protocol.Bulk{Kind: protocol.BulkFile, Name: "..", Size: 1}, []byte{1}, nil, 0); !strings.Contains(v.Error, "bad file name") {
		t.Errorf("an upload named ..: %+v", v)
	}

	deadline := time.Now().Add(5 * time.Second)
	for !slices.Equal(dirNames(t, downloads), []string{"kept.bin"}) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := dirNames(t, downloads); !slices.Equal(got, []string{"kept.bin"}) {
		t.Errorf("Downloads holds %q, want only kept.bin", got)
	}
	if got, _ := os.ReadFile(filepath.Join(downloads, "kept.bin")); !bytes.Equal(got, good) {
		t.Errorf("kept.bin holds %d bytes, want %d", len(got), len(good))
	}
}

// An offer reaches the page, waits for its click, and a failed transfer
// leaves it on offer for another try.
func TestOfferFetchedOverAPeer(t *testing.T) {
	s, runDir, _ := sharedSession(t, &fakeDesktop{})
	content := noise(120 << 10)
	path := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(path, content, 0o600)
	res := offerFiles(t, filepath.Join(runDir, files.SocketName), path)
	openOffers(t, s, 1)

	// A page that connects later hears of it.
	b := peerBrowser(t, s)
	offered := b.control(protocol.ControlFileOffer).File
	if offered.Name != "notes.txt" || offered.Size != int64(len(content)) {
		t.Fatalf("offer %+v", offered)
	}
	b.sendControl(protocol.Control{T: protocol.ControlFileFetch, File: &protocol.File{ID: "no-such-offer"}})
	b.sendControl(protocol.Control{T: protocol.ControlFileFetch})
	b.sendControl(protocol.Control{T: protocol.ControlFileFetch, File: &protocol.File{ID: offered.ID}})
	got := b.bulk()
	if got.header.ID != offered.ID || !bytes.Equal(got.data, content) {
		t.Errorf("fetched %+v, %d bytes", got.header, len(got.data))
	}
	if r := waitSent(t, res); r.err != nil || !strings.Contains(r.out, "notes.txt: saved by the browser") {
		t.Errorf("robin-agent send: %v, %q", r.err, r.out)
	}

	// The page refuses this one, so the offer stands and is still on offer.
	b.refuse.Store(true)
	res = offerFiles(t, filepath.Join(runDir, files.SocketName), path)
	offered = b.control(protocol.ControlFileOffer).File
	b.sendControl(protocol.Control{T: protocol.ControlFileFetch, File: &protocol.File{ID: offered.ID}})
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.files.mu.Lock()
		o := s.files.offers[offered.ID]
		idle := b.refused.Load() > 0 && o != nil && o.cancel == nil
		s.files.mu.Unlock()
		if idle {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the refused transfer never settled back to an offer")
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.declineOffer(protocol.Control{T: protocol.ControlFileDecline, File: &protocol.File{ID: offered.ID}})
	if r := waitSent(t, res); !errors.Is(r.err, files.ErrNotSaved) || !strings.Contains(r.out, "notes.txt: transfer failed, still on offer: the browser refused it: no room\n") {
		t.Errorf("after a refused transfer: %v, %q", r.err, r.out)
	}
}

// Taking control back from a guest ends the file the guest was fetching
// at once, and the file stays on offer for a page with control.
func TestRevokingAGuestEndsWhatItIsFetching(t *testing.T) {
	s, runDir, _ := sharedSession(t, &fakeDesktop{})
	s.shared.owner = "owner"
	s.ready = &protocol.Ready{Display: ":9", Mode: protocol.ModeSpawn, Encoder: protocol.EncoderSoftware, TunnelAddr: "127.0.0.1:9"}
	endGuestsAtCleanup(t, s)
	g, _ := addGuest(t, s, "alice", 1001)
	b := pageBrowser(t, s, g)
	change := func(a protocol.GuestAction) {
		s.inputMu.Lock()
		defer s.inputMu.Unlock()
		s.applyGuest(protocol.Control{Guest: &protocol.GuestChange{ID: g.id, Action: a}})
	}
	change(protocol.GuestGrant)

	path := filepath.Join(t.TempDir(), "big.bin")
	os.WriteFile(path, noise(4*protocol.BulkWindow), 0o600)
	res := offerFiles(t, filepath.Join(runDir, files.SocketName), path)
	offered := b.control(protocol.ControlFileOffer).File
	b.stall.Store(true)
	b.sendControl(protocol.Control{T: protocol.ControlFileFetch, File: &protocol.File{ID: offered.ID}})
	select {
	case h := <-b.started:
		if h.ID != offered.ID {
			t.Fatalf("the guest got %+v", h)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the fetch never started")
	}

	change(protocol.GuestRevoke)
	select {
	case <-b.cut:
	case <-time.After(5 * time.Second):
		t.Fatal("the guest's download goes on after its control was taken")
	}
	if w := b.control(protocol.ControlFileWithdraw).File; w.ID != offered.ID {
		t.Errorf("withdrew %+v", w)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.files.mu.Lock()
		o := s.files.offers[offered.ID]
		idle := o != nil && o.cancel == nil
		s.files.mu.Unlock()
		if idle {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the cut transfer never settled back to an offer")
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.declineOffer(protocol.Control{T: protocol.ControlFileDecline, File: &protocol.File{ID: offered.ID}})
	if r := waitSent(t, res); !strings.Contains(r.out, "still on offer: "+errControlTaken.Error()) {
		t.Errorf("robin-agent send printed %q", r.out)
	}
}
