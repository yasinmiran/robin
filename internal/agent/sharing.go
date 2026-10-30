//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"

	"github.com/yasinmiran/robin/internal/agent/peer"
	"github.com/yasinmiran/robin/internal/agent/share"
	"github.com/yasinmiran/robin/internal/agent/track"
	"github.com/yasinmiran/robin/internal/protocol"
)

// maxGuests bounds the guests of one session.
const maxGuests = 8

// Why a guest's session ended, as its CLI and page are told.
const (
	whyRemoved  = "the owner removed you from the session"
	whyAway     = "the owner disconnected"
	whyEnded    = "the session ended"
	whyFull     = "the session has as many guests as it takes"
	whyNotShare = "this session cannot be shared"
)

func init() {
	handleOwner(protocol.ControlInvite, (*session).applyInvite)
	handleOwner(protocol.ControlGuest, (*session).applyGuest)
}

// sharing is the session's guests and the door they come in by.
type sharing struct {
	door  *share.Door      // nil when the session takes no guests
	owner string           // the owner's user name, as a guest's page shows it
	uids  *uidSet          // guests connected now, whom the tunnel admits
	now   func() time.Time // nil is time.Now; tests move it by hand
	wg    sync.WaitGroup
	// guests, seq and away are guarded by session.mu.
	guests []*guest
	seq    int
	// away says the owner's controller is gone: guests are dropped and no
	// more join until it is back.
	away bool
}

// guest is another account watching the session through a connection of
// its own, its CLI's, which the door admitted.
type guest struct {
	id      string
	user    string
	uid     uint32 // as peercred and the tunnel have it
	conn    *protocol.Conn
	control atomic.Bool
	// view is the guest's page, nil between pages; gone is set once the
	// session let the guest go. session.mu guards both.
	view *viewer
	gone bool
	// opened is when the guest's last page got its peer; session.mu guards it.
	opened time.Time
}

// reopenWait is how soon after its last a guest's page may get a new peer.
const reopenWait = time.Second

func (sh *sharing) clock() time.Time {
	if sh.now == nil {
		return time.Now()
	}
	return sh.now()
}

// uidSet counts the guests of each uid connected now.
type uidSet struct {
	mu sync.Mutex
	n  map[uint32]int
}

func newUIDSet() *uidSet { return &uidSet{n: map[uint32]int{}} }

func (u *uidSet) add(uid uint32) {
	u.mu.Lock()
	u.n[uid]++
	u.mu.Unlock()
}

func (u *uidSet) remove(uid uint32) {
	u.mu.Lock()
	if u.n[uid]--; u.n[uid] <= 0 {
		delete(u.n, uid)
	}
	u.mu.Unlock()
}

// has reports whether a guest of uid is connected now.
func (u *uidSet) has(uid uint32) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.n[uid] > 0
}

// acceptGuests takes the guests the door admits until ctx ends.
func (s *session) acceptGuests(ctx context.Context) {
	if s.shared.door == nil {
		return
	}
	for {
		dg, err := s.shared.door.Next(ctx)
		if err != nil {
			return
		}
		s.admitGuest(ctx, dg)
	}
}

// admitGuest makes dg a guest of the session and serves its signaling, or
// tells it why not.
func (s *session) admitGuest(ctx context.Context, dg *share.Guest) {
	s.mu.Lock()
	why := ""
	switch {
	case s.shared.away:
		why = whyAway
	case len(s.shared.guests) >= maxGuests:
		why = whyFull
	}
	if why != "" {
		s.mu.Unlock()
		s.log.Warn("guest refused", "user", dg.User, "why", why)
		dg.Conn.Send(protocol.Message{Type: protocol.TypeError, Error: why})
		dg.Conn.Close()
		return
	}
	s.shared.seq++
	g := &guest{id: "g" + strconv.Itoa(s.shared.seq), user: dg.User, uid: uint32(dg.UID), conn: dg.Conn}
	s.shared.guests = append(s.shared.guests, g)
	s.mu.Unlock()
	s.shared.uids.add(g.uid)
	s.log.Info("guest admitted", "guest", g.id, "user", g.user, "uid", g.uid)
	s.shared.wg.Go(func() { s.serveGuest(ctx, g) })
	s.sendGuests()
}

// guestReady is what a guest's CLI hears once admitted: the desktop as it
// is now, and whose it is.
func (s *session) guestReady() *protocol.Ready {
	w, h := s.desk.Size()
	r := &protocol.Ready{
		Version: protocol.Version, AgentBuild: s.ready.AgentBuild, Display: s.ready.Display, Session: s.ready.Session,
		Width: w, Height: h, TunnelAddr: s.ready.TunnelAddr, Mode: s.ready.Mode, Audio: s.ready.Audio, Encoder: s.ready.Encoder,
		Share: &protocol.Share{Owner: s.shared.owner},
	}
	if s.mic != nil {
		r.Mic = &protocol.Device{Available: true, Where: s.mic.Where()}
	}
	return r
}

// serveGuest relays guest g's signaling until it leaves, its connection
// goes, the session lets it go, or ctx ends.
func (s *session) serveGuest(ctx context.Context, g *guest) {
	why := whyEnded
	defer func() { s.removeGuest(g, why) }()
	if err := g.conn.Send(protocol.Message{Type: protocol.TypeReady, Ready: s.guestReady()}); err != nil {
		return
	}
	for {
		m, err := g.conn.Recv(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Info("guest disconnected", "guest", g.id, "why", g.conn.Err())
			}
			return
		}
		switch m.Type {
		case protocol.TypeOffer:
			s.guestOffer(g, m.SDP)
		case protocol.TypeCandidate:
			if p := s.guestPeer(g); p != nil && m.Candidate != nil {
				if err := p.AddCandidate(*m.Candidate); err != nil && !errors.Is(err, peer.ErrTooManyCandidates) {
					s.log.Warn("guest candidate rejected", "guest", g.id, "err", err)
				}
			}
		case protocol.TypeHangup:
			s.closeGuestView(g)
		case protocol.TypeBye:
			why = protocol.WhyGuestLeft
			return
		default:
			s.log.Warn("ignoring guest message", "guest", g.id, "type", m.Type)
		}
	}
}

// guestOffer answers the offer of g's page, making its peer for a fresh
// page.
func (s *session) guestOffer(g *guest, sdp string) {
	s.mu.Lock()
	if g.gone {
		s.mu.Unlock()
		return
	}
	v := g.view
	if v == nil {
		// A page's peer costs the host a connection's worth of goroutines
		// and buffers; a guest's CLI that churns them is held to one a
		// second, more than any reload needs.
		now := s.shared.clock()
		if now.Sub(g.opened) < reopenWait {
			s.mu.Unlock()
			g.conn.Send(protocol.Message{Type: protocol.TypeError, Error: "a new page within a second of the last; try again"})
			return
		}
		g.opened = now
		var err error
		if v, err = s.openViewer(g); err != nil {
			s.mu.Unlock()
			s.log.Warn("guest peer", "guest", g.id, "err", err)
			g.conn.Send(protocol.Message{Type: protocol.TypeError, Error: err.Error()})
			return
		}
		g.view = v
	}
	s.mu.Unlock()
	answer, err := v.peer.Answer(sdp)
	if err != nil {
		s.log.Warn("guest offer rejected", "guest", g.id, "err", err)
		g.conn.Send(protocol.Message{Type: protocol.TypeError, Error: err.Error()})
		return
	}
	g.conn.Send(protocol.Message{Type: protocol.TypeAnswer, SDP: answer})
}

func (s *session) guestPeer(g *guest) *peer.Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g.view == nil {
		return nil
	}
	return g.view.peer
}

// closeGuestView lets g's page go and keeps g, whose CLI may bring
// another: a reload.
func (s *session) closeGuestView(g *guest) {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	s.mu.Lock()
	v := g.view
	g.view = nil
	s.mu.Unlock()
	if v != nil {
		s.closeViewer(v)
	}
}

// removeGuest lets g go: its peer and data channels close at once, its
// CLI is told why, and its connection closes.
func (s *session) removeGuest(g *guest, why string) {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	s.dropGuest(g, why)
}

// dropGuest is removeGuest for a caller that holds s.inputMu, which no
// control message of g's can be applying under.
func (s *session) dropGuest(g *guest, why string) {
	s.mu.Lock()
	if g.gone {
		s.mu.Unlock()
		return
	}
	g.gone = true
	v := g.view
	g.view = nil
	s.shared.guests = slices.DeleteFunc(s.shared.guests, func(o *guest) bool { return o == g })
	s.mu.Unlock()
	if v != nil {
		s.closeViewer(v)
	}
	g.control.Store(false)
	s.shared.uids.remove(g.uid)
	// The farewell goes off this goroutine, which holds the input lock: a
	// guest that stopped reading holds up nobody but itself, and only until
	// its connection's write bound.
	s.shared.wg.Go(func() {
		g.conn.Send(protocol.Message{Type: protocol.TypeEnded, Error: why})
		g.conn.Close()
	})
	s.log.Info("guest gone", "guest", g.id, "user", g.user, "why", why)
	s.sendGuests()
}

// dropGuests lets every guest go, for why; the caller holds s.inputMu or
// knows no page sends any more.
func (s *session) dropGuests(why string) {
	s.mu.Lock()
	gs := slices.Clone(s.shared.guests)
	s.mu.Unlock()
	for _, g := range gs {
		s.dropGuest(g, why)
	}
}

// ownerAway drops the guests and the invitations once the owner's
// controller is gone: nobody watches the desktop while its owner cannot
// see who does.
func (s *session) ownerAway() {
	s.mu.Lock()
	s.shared.away = true
	s.mu.Unlock()
	if s.shared.door != nil {
		s.shared.door.Invites().Clear()
	}
	s.inputMu.Lock()
	s.dropGuests(whyAway)
	s.inputMu.Unlock()
}

// ownerBack lets guests join again.
func (s *session) ownerBack() {
	s.mu.Lock()
	s.shared.away = false
	s.mu.Unlock()
}

// endGuests lets every guest go as the session ends, and waits until
// nothing of theirs runs.
func (s *session) endGuests() {
	s.inputMu.Lock()
	s.dropGuests(whyEnded)
	s.inputMu.Unlock()
	s.shared.wg.Wait()
}

// invite opens an invitation for user.
func (s *session) invite(user string) (protocol.Invitation, error) {
	if s.shared.door == nil {
		return protocol.Invitation{}, errors.New(whyNotShare)
	}
	inv, err := s.shared.door.Invites().Invite(user)
	if err != nil {
		return protocol.Invitation{}, err
	}
	s.log.Info("guest invited", "user", user, "expires", inv.Expires)
	return inv, nil
}

// answerInvite is the agent's answer to the owner CLI's TypeInvite.
func (s *session) answerInvite(m protocol.Message) error {
	var inv protocol.Invitation
	err := errors.New("an invite names a user")
	if m.Invite != nil {
		inv, err = s.invite(m.Invite.User)
		inv.User = m.Invite.User
	}
	if err != nil {
		inv.Error = err.Error()
	}
	if err == nil {
		s.sendGuests()
	}
	return s.conn.Send(protocol.Message{Type: protocol.TypeInvited, Invitation: &inv})
}

func (s *session) applyInvite(m protocol.Control) {
	if m.Invite == nil {
		return
	}
	_, err := s.invite(m.Invite.User)
	s.sendGuestsError(err)
}

// applyGuest makes the owner page's change to one guest. It runs under
// s.inputMu, so a guest's control message is either applied already or
// will find the change made.
func (s *session) applyGuest(m protocol.Control) {
	if m.Guest == nil {
		return
	}
	g := s.findGuest(m.Guest.ID)
	if g == nil {
		s.sendGuestsError(fmt.Errorf("no guest %q", m.Guest.ID))
		return
	}
	switch m.Guest.Action {
	case protocol.GuestGrant:
		s.grant(g)
	case protocol.GuestRevoke:
		s.revoke(g)
	case protocol.GuestRemove:
		s.dropGuest(g, whyRemoved)
		return
	default:
		s.sendGuestsError(fmt.Errorf("no action %q", m.Guest.Action))
		return
	}
	s.sendGuests()
}

func (s *session) findGuest(id string) *guest {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.shared.guests {
		if g.id == id {
			return g
		}
	}
	return nil
}

// grant gives g control, taking it from any other guest: one guest drives
// at a time, beside the owner. g's page gets the files on offer.
func (s *session) grant(g *guest) {
	s.mu.Lock()
	others := slices.Clone(s.shared.guests)
	s.mu.Unlock()
	for _, o := range others {
		if o != g && o.control.Load() {
			s.revoke(o)
		}
	}
	if g.control.Swap(true) {
		return
	}
	s.log.Info("guest given control", "guest", g.id)
	s.sendShare(g)
	if v := s.viewOf(g); v != nil {
		s.resendOffers(v)
	}
}

// revoke takes control back from g at once: what it holds down is let go
// and what it was typing dropped, the files on offer leave its page, and
// what the session was sending it, a file or the clipboard, ends.
func (s *session) revoke(g *guest) {
	if !g.control.Swap(false) {
		return
	}
	s.log.Info("guest control revoked", "guest", g.id)
	s.typing.discard()
	s.desk.Input(protocol.Control{T: protocol.ControlRelease})
	s.sendShare(g)
	if v := s.viewOf(g); v != nil {
		s.endTransfersTo(v, errControlTaken)
		s.withdrawOffers(v)
	}
}

// revokeGuests takes control from every guest; the caller holds s.inputMu.
func (s *session) revokeGuests() {
	s.mu.Lock()
	gs := slices.Clone(s.shared.guests)
	s.mu.Unlock()
	for _, g := range gs {
		s.revoke(g)
	}
}

func (s *session) viewOf(g *guest) *viewer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return g.view
}

// guestsView is the owner page's view of the sharing.
func (s *session) guestsView() *protocol.Guests {
	view := &protocol.Guests{List: []protocol.Guest{}, Invitations: []protocol.Invitation{}}
	if s.shared.door != nil {
		view.Invitations = s.shared.door.Invites().Open()
	}
	s.mu.Lock()
	for _, g := range s.shared.guests {
		view.List = append(view.List, protocol.Guest{ID: g.id, User: g.user, Control: g.control.Load()})
	}
	s.mu.Unlock()
	return view
}

// sendGuests tells the owner's page who its guests are.
func (s *session) sendGuests() { s.sendGuestsError(nil) }

// sendGuestsError tells the owner's page who its guests are, and why its
// last change failed, if it did. A session that takes no guests sends
// nothing, and its page offers no sharing.
func (s *session) sendGuestsError(err error) {
	p := s.ownerPeer()
	if p == nil || s.shared.door == nil {
		return
	}
	view := s.guestsView()
	if err != nil {
		view.Error = err.Error()
	}
	if err := p.SendControl(protocol.Control{T: protocol.ControlGuests, Guests: view}); err != nil && !errors.Is(err, peer.ErrNoControl) {
		s.log.Debug("send guests", "err", err)
	}
}

// sendShare tells g's page whose desktop it is and whether it has control.
func (s *session) sendShare(g *guest) {
	v := s.viewOf(g)
	if v == nil {
		return
	}
	share := protocol.Share{Owner: s.shared.owner, Control: g.control.Load()}
	if err := v.peer.SendControl(protocol.Control{T: protocol.ControlShare, Share: &share}); err != nil && !errors.Is(err, peer.ErrNoControl) {
		s.log.Debug("send share", "err", err)
	}
}

// errControlTaken ends a guest's turn at the microphone, and what the
// session was sending its page, once its control is taken.
var errControlTaken = errors.New("the guest's control was taken")

// gated is a guest's microphone track as the microphone reads it while the
// guest has control, starting with first, the packet that found it had
// control. The first packet to arrive without control ends the read with
// errControlTaken, so the guest's Play gives the microphone up.
type gated struct {
	r       track.Reader
	allowed func() bool
	first   *rtp.Packet
	attrs   interceptor.Attributes
}

func (g *gated) ReadRTP() (*rtp.Packet, interceptor.Attributes, error) {
	if p := g.first; p != nil {
		// The turn may have come after control went.
		g.first = nil
		if !g.allowed() {
			return nil, nil, errControlTaken
		}
		return p, g.attrs, nil
	}
	p, a, err := g.r.ReadRTP()
	if err == nil && !g.allowed() {
		return nil, nil, errControlTaken
	}
	return p, a, err
}

// playGuestMic feeds the microphone from guest track r only while allowed
// says the guest has control. Without control r is read and dropped and
// the guest holds no turn at the microphone, whose tracks take turns: a
// guest that only watches never keeps another page's from being heard.
func (s *session) playGuestMic(r track.Reader, allowed func() bool) error {
	for {
		p, a, err := r.ReadRTP()
		if err != nil {
			return err
		}
		if !allowed() {
			continue
		}
		if err := s.mic.Play(&gated{r: r, allowed: allowed, first: p, attrs: a}); !errors.Is(err, errControlTaken) {
			return err
		}
	}
}
