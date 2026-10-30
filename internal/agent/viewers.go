//go:build linux

package agent

import (
	"errors"
	"fmt"

	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/robin/internal/agent/peer"
	"github.com/yasinmiran/robin/internal/protocol"
)

// viewer is one page watching the session, with its own peer connection
// on the session's one stream: the owner's page, whose signaling comes
// through the controller, or a guest's, through that guest's connection.
type viewer struct {
	gen   uint64 // the gen-th viewer the session made
	guest *guest // nil for the owner's page
	peer  *peer.Peer
	sub   *subscriber
	// cursorsShown are the pointer images this page has; session.mu
	// guards it.
	cursorsShown map[uint32]bool
}

// control reports whether what the page sends may drive the desktop: the
// owner's always does, a guest's once the owner gave it control.
func (v *viewer) control() bool { return v.guest == nil || v.guest.control.Load() }

// openViewer makes the peer of a page's first offer: the owner's page for
// a nil g, else guest g's. A guest's peer takes no camera, which stays the
// owner's. s.mu must be held.
func (s *session) openViewer(g *guest) (*viewer, error) {
	s.gens++
	v := &viewer{gen: s.gens, guest: g, cursorsShown: map[uint32]bool{}}
	signal := s.conn.Send
	if g != nil {
		signal = g.conn.Send
	}
	maxBitrate := max(s.start.MaxKbps*1000, startBitrate)
	v.sub = newSubscriber(min(startBitrate, maxBitrate), g == nil)
	p, err := peer.New(peer.Config{
		STUN: s.start.STUN, TCPMux: s.tcpMux, IncludeLoopback: true, Certificate: s.cert,
		Audio: s.audio, Video: s.stream.video, AudioTrack: s.sound,
		Mic: s.mic != nil, Camera: s.camera != nil && g == nil,
		MinBitrate: minBitrate, StartBitrate: min(startBitrate, maxBitrate), MaxBitrate: maxBitrate,
		Log: s.log,
	}, peer.Handlers{
		Candidate: func(c protocol.Candidate) {
			if err := signal(protocol.Message{Type: protocol.TypeCandidate, Candidate: &c}); err != nil {
				s.log.Warn("send candidate", "err", err)
			}
		},
		Control:     s.controlFrom(v),
		Bulk:        func(h protocol.Bulk) (peer.BulkSink, error) { return s.bulkFrom(v, h) },
		Track:       func(tr *webrtc.TrackRemote, r *webrtc.RTPReceiver) { s.track(v, tr, r) },
		ControlOpen: func() { s.opened(v) },
		Keyframe:    s.stream.requestKeyframe,
		Bitrate:     v.sub.setBitrate,
		State: func(st webrtc.PeerConnectionState) {
			// A guest's page arriving must not let go of what the owner holds.
			if st == webrtc.PeerConnectionStateConnected && g == nil {
				s.desk.Input(protocol.Control{T: protocol.ControlRelease})
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create peer: %w", err)
	}
	v.peer, v.sub.peer = p, p
	s.stream.add(v.sub)
	return v, nil
}

// opened sends a page whose control channel just opened what it holds:
// the pointer, the monitors, the files on offer when it has control, the
// owner's page its private session and guests, a guest's its share.
func (s *session) opened(v *viewer) {
	s.sendCurrentCursor(v)
	s.sendMonitorsTo(v)
	if v.control() {
		s.resendOffers(v)
	}
	if v.guest == nil {
		s.sendPrivate()
		s.sendGuests()
		return
	}
	s.sendShare(v.guest)
}

// controlFrom returns the handler of the control messages of viewer v.
// Once the session has let that page go, nothing more it sends applies:
// its data channel can still deliver a message or two while the peer
// closes, and text among them would start the lane typing again.
func (s *session) controlFrom(v *viewer) func(protocol.Control) {
	return func(m protocol.Control) {
		s.inputMu.Lock()
		defer s.inputMu.Unlock()
		if s.live(v) {
			s.controlAs(v, m)
		}
	}
}

// live reports whether v is a page the session still serves.
func (s *session) live(v *viewer) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v.guest == nil {
		return s.owner == v
	}
	return v.guest.view == v
}

// closeViewer closes v's peer once nothing it sent is applying, takes it
// off the stream, and lets go of what it held down when it had control.
// The caller has unlinked v, under s.mu, and holds s.inputMu.
func (s *session) closeViewer(v *viewer) {
	s.stream.remove(v.sub)
	if v.control() {
		// Whatever the departing page held down must not stay pressed, nor
		// what it typed last go on typing.
		s.typing.discard()
		s.desk.Input(protocol.Control{T: protocol.ControlRelease})
	}
	v.peer.Close()
}

// currentPeer returns the owner page's peer, creating one for a fresh page.
func (s *session) currentPeer() (*peer.Peer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner != nil {
		return s.owner.peer, nil
	}
	v, err := s.openViewer(nil)
	if err != nil {
		return nil, err
	}
	s.owner = v
	return v.peer, nil
}

// closePeer lets the owner's page go, and takes control from every guest:
// no page of the owner's is left to take it back.
func (s *session) closePeer() {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	s.mu.Lock()
	v := s.owner
	s.owner = nil
	s.mu.Unlock()
	if v != nil {
		s.closeViewer(v)
	}
	s.revokeGuests()
}

// ownerPeer is the owner page's peer, or nil.
func (s *session) ownerPeer() *peer.Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.owner == nil {
		return nil
	}
	return s.owner.peer
}

// viewers lists the pages with a peer: the owner's first, then the
// guests' in the order they joined.
func (s *session) viewers() []*viewer {
	s.mu.Lock()
	defer s.mu.Unlock()
	var vs []*viewer
	if s.owner != nil {
		vs = append(vs, s.owner)
	}
	for _, g := range s.shared.guests {
		if g.view != nil {
			vs = append(vs, g.view)
		}
	}
	return vs
}

// controlling lists the pages whose input drives the desktop, and which
// get what the clipboard and the files on offer carry.
func (s *session) controlling() []*viewer {
	var vs []*viewer
	for _, v := range s.viewers() {
		if v.control() {
			vs = append(vs, v)
		}
	}
	return vs
}

// peerOf is v's peer; a nil v is the owner's page.
func (s *session) peerOf(v *viewer) *peer.Peer {
	if v == nil {
		return s.ownerPeer()
	}
	return v.peer
}

// errNoControl refuses what a guest without control sends.
var errNoControl = errors.New("the owner has not given you control of this desktop")

// bulkFrom picks the handler for a transfer v's page opened. A guest's
// transfer needs control as it opens and still as it lands: one whose
// control was revoked meanwhile is dropped.
func (s *session) bulkFrom(v *viewer, h protocol.Bulk) (peer.BulkSink, error) {
	if !v.control() {
		return nil, errNoControl
	}
	sink, err := s.bulk(h)
	if err != nil {
		return nil, err
	}
	if a, ok := sink.(answersSender); ok {
		a.sentBy(v)
	}
	if v.guest == nil {
		return sink, nil
	}
	return &controlledSink{BulkSink: sink, s: s, v: v}, nil
}

// answersSender is a sink that tells the page that sent it how it went.
type answersSender interface{ sentBy(*viewer) }

// controlledSink is a guest's transfer, which lands only while the guest
// has control and is still the session's.
type controlledSink struct {
	peer.BulkSink
	s *session
	v *viewer
}

func (c *controlledSink) Commit() error {
	if !c.v.control() || !c.s.live(c.v) {
		c.BulkSink.Abort(errNoControl)
		return errNoControl
	}
	return c.BulkSink.Commit()
}
