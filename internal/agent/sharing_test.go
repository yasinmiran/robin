//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/robin/internal/agent/desktop"
	"github.com/yasinmiran/robin/internal/agent/share"
	"github.com/yasinmiran/robin/internal/agent/track"
	"github.com/yasinmiran/robin/internal/protocol"
)

// cliEnd is the guest CLI's side of a guest's connection: what it reads
// lands in got.
type cliEnd struct {
	conn *protocol.Conn
	got  chan protocol.Message
	raw  net.Conn // the CLI end of the pipe
	// candidate, once set, takes the agent's candidates instead of got.
	candidate atomic.Pointer[func(protocol.Candidate)]
}

// guestConn makes the two ends of a guest's connection: the agent's, as
// the door hands it over, and the CLI's.
func guestConn(t *testing.T) (*protocol.Conn, *cliEnd) {
	t.Helper()
	a, b := net.Pipe()
	agentSide := protocol.NewConn(a, a, 0, 0)
	cli := &cliEnd{conn: protocol.NewConn(b, b, 0, 0), got: make(chan protocol.Message, 64), raw: b}
	go func() {
		<-agentSide.Done()
		a.Close()
	}()
	go func() {
		defer close(cli.got)
		for {
			m, err := cli.conn.Recv(context.Background())
			if err != nil {
				return
			}
			if f := cli.candidate.Load(); f != nil && m.Type == protocol.TypeCandidate && m.Candidate != nil {
				(*f)(*m.Candidate)
				continue
			}
			cli.got <- m
		}
	}()
	t.Cleanup(func() {
		cli.conn.Close()
		b.Close()
	})
	return agentSide, cli
}

// next is the next message of type typ the CLI reads, skipping the rest.
func (c *cliEnd) next(t *testing.T, typ protocol.Type) protocol.Message {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case m, ok := <-c.got:
			if !ok {
				t.Fatalf("the connection closed before %q", typ)
			}
			if m.Type == typ {
				return m
			}
		case <-timeout:
			t.Fatalf("no %q within 10 s", typ)
		}
	}
}

// closed waits for the connection to close.
func (c *cliEnd) closed(t *testing.T) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-c.got:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("the guest's connection is still open")
		}
	}
}

// guestSession is a test session that takes guests, owned by "owner".
func guestSession(desk *fakeTypist) *session {
	s := testSession(desk)
	s.shared.owner = "owner"
	s.ready = &protocol.Ready{Display: ":9", Mode: protocol.ModeSpawn, Encoder: protocol.EncoderSoftware, TunnelAddr: "127.0.0.1:9"}
	return s
}

// addGuest admits a guest of uid and returns it with its CLI's end, once
// that end has read the guest's Ready.
func addGuest(t *testing.T, s *session, user string, uid int) (*guest, *cliEnd) {
	t.Helper()
	conn, cli := guestConn(t)
	s.admitGuest(t.Context(), &share.Guest{Conn: conn, UID: uid, User: user})
	r := cli.next(t, protocol.TypeReady).Ready
	if r.Share == nil || r.Share.Owner != "owner" || r.Share.Control || r.Width != 800 || r.Display != ":9" {
		t.Fatalf("the guest's Ready: %+v", r)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shared.guests[len(s.shared.guests)-1], cli
}

// pageOf gives g a page whose peer no browser answers: enough for what
// the session does with a page, short of sending it anything.
func pageOf(t *testing.T, s *session, g *guest) *viewer {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.openViewer(g)
	if err != nil {
		t.Fatal(err)
	}
	g.view = v
	return v
}

// openTestDoor opens a door of this user's for a session named after the
// test.
func openTestDoor(t *testing.T) *share.Door {
	t.Helper()
	door, err := share.Open(share.NewInvites(fmt.Sprintf("t%d%d", os.Getpid(), time.Now().UnixNano()), os.Getuid()), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { door.Close() })
	return door
}

func endGuestsAtCleanup(t *testing.T, s *session) {
	t.Cleanup(func() {
		s.endGuests()
		s.closePeer()
	})
}

// A guest's input applies only while it has control; what only the
// owner's page sends never does. One guest has control at a time, and
// taking it away lets go of what that guest held down.
func TestGuestControlGatesInputAndHandsOver(t *testing.T) {
	desk := &fakeTypist{}
	s := guestSession(desk)
	endGuestsAtCleanup(t, s)
	g1, _ := addGuest(t, s, "alice", 1001)
	g2, _ := addGuest(t, s, "bob", 1002)
	v1, v2 := pageOf(t, s, g1), pageOf(t, s, g2)
	key := func(k string) protocol.Control {
		return protocol.Control{T: protocol.ControlKey, Key: k, Code: "Key" + strings.ToUpper(k), Down: true}
	}
	applied := func() []protocol.Control {
		waitIdle(t, &s.typing)
		defer func() {
			desk.mu.Lock()
			desk.inputs = nil
			desk.mu.Unlock()
		}()
		return desk.got()
	}
	release := protocol.Control{T: protocol.ControlRelease}

	s.controlAs(v1, key("a"))
	s.controlAs(v1, protocol.Control{T: protocol.ControlText, Text: "typed without control"})
	if got := applied(); len(got) != 0 {
		t.Fatalf("a guest without control drove the desktop: %+v", got)
	}

	s.applyGuest(protocol.Control{Guest: &protocol.GuestChange{ID: g1.id, Action: protocol.GuestGrant}})
	s.controlAs(v1, key("b"))
	s.controlAs(v1, protocol.Control{T: protocol.ControlResize, W: 640, H: 480})
	s.controlAs(v1, protocol.Control{T: protocol.ControlCurtain, On: true})
	if got := applied(); !slices.Equal(got, []protocol.Control{key("b")}) {
		t.Fatalf("with control the guest applied %+v, want its key alone", got)
	}
	if len(desk.sizes) != 0 {
		t.Fatal("a guest resized the desktop")
	}
	asOwner(s, protocol.Control{T: protocol.ControlResize, W: 640, H: 480})
	if len(desk.sizes) != 1 {
		t.Fatal("the owner's resize did not apply")
	}

	// Granting the second takes control from the first, letting go of its
	// keys.
	s.applyGuest(protocol.Control{Guest: &protocol.GuestChange{ID: g2.id, Action: protocol.GuestGrant}})
	if g1.control.Load() || !g2.control.Load() {
		t.Fatalf("after the handover: alice %v, bob %v", g1.control.Load(), g2.control.Load())
	}
	s.controlAs(v1, key("c"))
	s.controlAs(v2, key("d"))
	if got := applied(); !slices.Equal(got, []protocol.Control{release, key("d")}) {
		t.Fatalf("after the handover the desktop got %+v, want the release and bob's key", got)
	}

	s.applyGuest(protocol.Control{Guest: &protocol.GuestChange{ID: g2.id, Action: protocol.GuestRevoke}})
	s.controlAs(v2, key("e"))
	if got := applied(); !slices.Equal(got, []protocol.Control{release}) {
		t.Fatalf("after the revoke the desktop got %+v, want the release alone", got)
	}
	// Revoking twice, or granting one who has it, changes nothing.
	s.revoke(g2)
	s.applyGuest(protocol.Control{Guest: &protocol.GuestChange{ID: g1.id, Action: protocol.GuestGrant}})
	s.grant(g1)
	if got := applied(); len(got) != 0 {
		t.Fatalf("a repeated change touched the desktop: %+v", got)
	}

	// The owner's view lists both, and who has control.
	view := s.guestsView()
	want := []protocol.Guest{{ID: g1.id, User: "alice", Control: true}, {ID: g2.id, User: "bob"}}
	if !slices.Equal(view.List, want) {
		t.Errorf("guests %+v, want %+v", view.List, want)
	}
}

func TestApplyGuestRefusesWhatItCannotDo(t *testing.T) {
	s := guestSession(&fakeTypist{})
	endGuestsAtCleanup(t, s)
	g, cli := addGuest(t, s, "alice", 1001)
	s.applyGuest(protocol.Control{})
	s.applyGuest(protocol.Control{Guest: &protocol.GuestChange{ID: "g99", Action: protocol.GuestGrant}})
	s.applyGuest(protocol.Control{Guest: &protocol.GuestChange{ID: g.id, Action: "promote"}})
	if g.control.Load() {
		t.Fatal("an unknown action gave control")
	}
	s.applyGuest(protocol.Control{Guest: &protocol.GuestChange{ID: g.id, Action: protocol.GuestRemove}})
	if m := cli.next(t, protocol.TypeEnded); m.Error != whyRemoved {
		t.Errorf("the removed guest heard %q", m.Error)
	}
	cli.closed(t)
	if s.findGuest(g.id) != nil || s.shared.uids.has(1001) {
		t.Error("the removed guest is still the session's")
	}
}

// A guest's transfer needs control as it opens and as it lands; one whose
// control went meanwhile never reaches the clipboard.
func TestAGuestsTransferNeedsControl(t *testing.T) {
	desk := &fakeTypist{}
	s := guestSession(desk)
	endGuestsAtCleanup(t, s)
	g, _ := addGuest(t, s, "alice", 1001)
	v := pageOf(t, s, g)
	clip := protocol.Bulk{T: protocol.BulkHeader, Kind: protocol.BulkClipboard, Size: 4}
	if _, err := s.bulkFrom(v, clip); !errors.Is(err, errNoControl) {
		t.Fatalf("a transfer without control: %v", err)
	}
	s.grant(g)
	payload, err := protocol.EncodeClip(protocol.Clip{Text: "late"})
	if err != nil {
		t.Fatal(err)
	}
	sink, err := s.bulkFrom(v, clip)
	if err != nil {
		t.Fatal(err)
	}
	sink.Write(payload)
	s.revoke(g)
	if err := sink.Commit(); !errors.Is(err, errNoControl) {
		t.Fatalf("committed after the revoke: %v", err)
	}
	if s.clipsIn.Load() != 0 {
		t.Error("the aborted transfer still counts")
	}
	if len(desk.clipboard) != 0 {
		t.Errorf("the clipboard was set to %q", desk.clipboard)
	}

	s.grant(g)
	sink, err = s.bulkFrom(v, clip)
	if err != nil {
		t.Fatal(err)
	}
	sink.Write(payload)
	sink.Commit()
	waitIdle(t, &s.typing)
	if !slices.Equal(desk.clipboard, []string{"late"}) {
		t.Errorf("with control the clipboard got %q", desk.clipboard)
	}
	// The owner's own transfer is not wrapped.
	if sink, err := s.bulkFrom(&viewer{}, clip); err != nil {
		t.Fatal(err)
	} else if _, wrapped := sink.(*controlledSink); wrapped {
		t.Error("the owner's transfer waits on control")
	}
}

func TestAGuestLeavingOrGoingIsLetGo(t *testing.T) {
	s := guestSession(&fakeTypist{})
	endGuestsAtCleanup(t, s)

	g, cli := addGuest(t, s, "alice", 1001)
	if !s.shared.uids.has(1001) {
		t.Fatal("the tunnel does not admit the guest")
	}
	cli.conn.Send(protocol.Message{Type: protocol.TypeBye})
	if m := cli.next(t, protocol.TypeEnded); m.Error != protocol.WhyGuestLeft {
		t.Errorf("a guest that left heard %q", m.Error)
	}
	cli.closed(t)
	waitUntil(t, func() bool { return s.findGuest(g.id) == nil && !s.shared.uids.has(1001) })

	g, cli = addGuest(t, s, "alice", 1001)
	cli.raw.Close()
	waitUntil(t, func() bool { return s.findGuest(g.id) == nil })

	// A message a guest has no business sending changes nothing.
	g, cli = addGuest(t, s, "alice", 1001)
	cli.conn.Send(protocol.Message{Type: protocol.TypeStart, Start: &protocol.Start{}})
	cli.conn.Send(protocol.Message{Type: protocol.TypeCandidate, Candidate: &protocol.Candidate{}})
	cli.conn.Send(protocol.Message{Type: protocol.TypeHangup})
	cli.conn.Send(protocol.Message{Type: protocol.TypeOffer, SDP: "not sdp"})
	if m := cli.next(t, protocol.TypeError); m.Error == "" {
		t.Error("a bad offer was answered without a reason")
	}
	if s.findGuest(g.id) == nil {
		t.Fatal("a bad offer cost the guest its place")
	}
}

// While the owner's controller is away no guest watches: those there are
// let go, invitations are withdrawn, and no more join until it is back.
func TestGuestsGoWithTheOwner(t *testing.T) {
	s := guestSession(&fakeTypist{})
	endGuestsAtCleanup(t, s)
	_, cli := addGuest(t, s, "alice", 1001)
	s.ownerAway()
	if m := cli.next(t, protocol.TypeEnded); m.Error != whyAway {
		t.Errorf("the guest heard %q", m.Error)
	}
	conn, late := guestConn(t)
	s.admitGuest(t.Context(), &share.Guest{Conn: conn, UID: 1002, User: "bob"})
	if m := late.next(t, protocol.TypeError); m.Error != whyAway {
		t.Errorf("a guest joining while the owner was away heard %q", m.Error)
	}
	late.closed(t)
	s.ownerBack()
	addGuest(t, s, "bob", 1002)
}

func TestTheSessionTakesSoManyGuests(t *testing.T) {
	s := guestSession(&fakeTypist{})
	endGuestsAtCleanup(t, s)
	for i := range maxGuests {
		addGuest(t, s, "g", 2000+i)
	}
	conn, cli := guestConn(t)
	s.admitGuest(t.Context(), &share.Guest{Conn: conn, UID: 3000, User: "late"})
	if m := cli.next(t, protocol.TypeError); m.Error != whyFull {
		t.Errorf("past maxGuests: %q", m.Error)
	}
	s.endGuests()
	if len(s.shared.guests) != 0 || s.shared.uids.has(2000) {
		t.Error("guests outlived the session")
	}
}

func TestInviteWithoutADoor(t *testing.T) {
	s := guestSession(&fakeTypist{})
	c, cli := guestConn(t) // stands in for the owner's controller
	s.conn = newController(c)
	if err := s.answerInvite(protocol.Message{Type: protocol.TypeInvite, Invite: &protocol.Invite{User: "alice"}}); err != nil {
		t.Fatal(err)
	}
	if inv := cli.next(t, protocol.TypeInvited).Invitation; inv.User != "alice" || inv.Code != "" || inv.Error != whyNotShare {
		t.Errorf("invitation %+v", inv)
	}
	s.answerInvite(protocol.Message{Type: protocol.TypeInvite})
	if inv := cli.next(t, protocol.TypeInvited).Invitation; inv.Error == "" {
		t.Errorf("an invite naming nobody: %+v", inv)
	}
	s.applyInvite(protocol.Control{})
	s.applyInvite(protocol.Control{Invite: &protocol.Invite{User: "alice"}})
}

// With a door, the owner CLI's invite comes back with a code the door
// redeems, and the guest it admits is the session's.
func TestInviteThroughTheDoor(t *testing.T) {
	user := "root"
	if os.Getuid() == 0 {
		user = "nobody"
	}
	s := guestSession(&fakeTypist{})
	s.shared.door = openTestDoor(t)
	endGuestsAtCleanup(t, s)
	c, cli := guestConn(t)
	s.conn = newController(c)
	if err := s.answerInvite(protocol.Message{Type: protocol.TypeInvite, Invite: &protocol.Invite{User: user}}); err != nil {
		t.Fatal(err)
	}
	got := cli.next(t, protocol.TypeInvited).Invitation
	if got.Error != "" || got.User != user {
		t.Fatalf("invitation %+v", got)
	}
	if open := s.guestsView().Invitations; len(open) != 1 || open[0].Code != got.Code {
		t.Errorf("the owner's view lists %+v", open)
	}
	s.applyInvite(protocol.Control{Invite: &protocol.Invite{User: "robin no such user"}})
	if _, err := s.invite("robin-no-such-user"); err == nil {
		t.Error("invited a user the host has not")
	}
}

// The owner's page and a guest's are two peers on the one stream: both get
// its frames, the owner's page hears of the guest and the guest's page of
// its control. Taking the guest off the session closes its peer at once:
// the browser's control channel closes, the stream stops sending to it,
// and its CLI is told why, while the owner's page keeps the stream.
func TestRemovingAGuestClosesItsPeerAtOnce(t *testing.T) {
	desk := &stillTypist{}
	s := guestSession(&desk.fakeTypist)
	s.desk = desk
	s.shared.door = openTestDoor(t)
	var encodes atomic.Int64
	s.stream = mustStream(newStream(desk, 60, 64, 64, func(int, int, int, int) (Encoder, error) { return countingEncoder{&encodes}, nil }, s.log))
	s.stream.refresh = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	streamed := make(chan struct{})
	go func() {
		s.stream.run(ctx)
		close(streamed)
	}()
	defer func() {
		cancel()
		<-streamed
	}()
	endGuestsAtCleanup(t, s)
	ownerConn, ownerCLI := guestConn(t)
	s.conn = newController(ownerConn)
	go s.relay(ctx, ownerConn, nil)
	owner := newGuestBrowser(t, ownerCLI)
	owner.waitFrames(t, 3)

	g, cli := addGuest(t, s, "alice", 1001)
	b := newGuestBrowser(t, cli)
	if g.view == nil {
		t.Fatal("the guest's offer made no page")
	}
	b.waitFrames(t, 5)
	subs := s.stream.subscribers()
	if len(subs) != 2 || !subs[0].tiles || subs[1].tiles {
		t.Fatalf("subscribers %+v, want the owner's taking tiles and the guest's", subs)
	}
	before := owner.frames.Load()
	b.waitFrames(t, b.frames.Load()+5)
	if owner.frames.Load() <= before {
		t.Fatal("the owner's page stopped getting frames once the guest came")
	}
	owner.waitControl(t, func(c protocol.Control) bool {
		return c.T == protocol.ControlGuests && len(c.Guests.List) == 1 && c.Guests.List[0].User == "alice"
	})
	b.waitControl(t, func(c protocol.Control) bool { return c.T == protocol.ControlShare && !c.Share.Control })
	s.inputMu.Lock()
	s.applyGuest(protocol.Control{Guest: &protocol.GuestChange{ID: g.id, Action: protocol.GuestGrant}})
	s.inputMu.Unlock()
	b.waitControl(t, func(c protocol.Control) bool {
		return c.T == protocol.ControlShare && c.Share.Control && c.Share.Owner == "owner"
	})
	owner.waitControl(t, func(c protocol.Control) bool {
		return c.T == protocol.ControlGuests && len(c.Guests.List) == 1 && c.Guests.List[0].Control
	})

	start := time.Now()
	s.applyGuest(protocol.Control{Guest: &protocol.GuestChange{ID: g.id, Action: protocol.GuestRemove}})
	if m := cli.next(t, protocol.TypeEnded); m.Error != whyRemoved {
		t.Errorf("the guest heard %q", m.Error)
	}
	select {
	case <-b.controlClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("the guest's control channel is still open 5 s after the removal")
	}
	t.Logf("the guest's control channel closed %s after the removal", time.Since(start).Round(time.Millisecond))
	if subs := s.stream.subscribers(); len(subs) != 1 || subs[0].peer != s.ownerPeer() {
		t.Error("the stream still sends to the removed guest")
	}
	before = owner.frames.Load()
	owner.waitFrames(t, before+5)
}

// stillTypist is a fakeTypist with a still screen.
type stillTypist struct{ fakeTypist }

func (*stillTypist) Capture(force bool) (*desktop.Frame, bool, error) {
	return (&stillDesktop{}).Capture(force)
}

// guestBrowser is a stand-in browser on a guest's CLI: it offers through
// the guest's connection as the CLI relays a page, and counts frames.
type guestBrowser struct {
	pc            *webrtc.PeerConnection
	frames        atomic.Int64
	controlClosed chan struct{}
	mu            sync.Mutex
	controls      []protocol.Control
}

// waitControl waits for a control message from the agent that ok takes.
func (b *guestBrowser) waitControl(t *testing.T, ok func(protocol.Control) bool) {
	t.Helper()
	waitUntil(t, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return slices.ContainsFunc(b.controls, ok)
	})
}

func newGuestBrowser(t *testing.T, cli *cliEnd) *guestBrowser {
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
	b := &guestBrowser{pc: pc, controlClosed: make(chan struct{})}
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	dc, err := pc.CreateDataChannel(protocol.ControlChannel, nil)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	dc.OnClose(func() { once.Do(func() { close(b.controlClosed) }) })
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		var c protocol.Control
		if json.Unmarshal(m.Data, &c) == nil && c.T != protocol.ControlHeartbeat {
			b.mu.Lock()
			b.controls = append(b.controls, c)
			b.mu.Unlock()
		}
	})
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			for {
				pkt, _, err := tr.ReadRTP()
				if err != nil {
					return
				}
				if pkt.Marker {
					b.frames.Add(1)
				}
			}
		}()
	})
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			j := c.ToJSON()
			cli.conn.Send(protocol.Message{Type: protocol.TypeCandidate, Candidate: &protocol.Candidate{
				Candidate: j.Candidate, SDPMid: j.SDPMid, SDPMLineIndex: j.SDPMLineIndex, UsernameFragment: j.UsernameFragment,
			}})
		}
	})
	// The agent's candidates can overtake its answer; they wait for it.
	var mu sync.Mutex
	var early []webrtc.ICECandidateInit
	answered := false
	add := func(c protocol.Candidate) {
		if c.Candidate == "" {
			return
		}
		init := webrtc.ICECandidateInit{Candidate: c.Candidate, SDPMid: c.SDPMid, SDPMLineIndex: c.SDPMLineIndex}
		mu.Lock()
		defer mu.Unlock()
		if !answered {
			early = append(early, init)
			return
		}
		pc.AddICECandidate(init)
	}
	cli.candidate.Store(&add)
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	cli.conn.Send(protocol.Message{Type: protocol.TypeOffer, SDP: offer.SDP})
	answer := cli.next(t, protocol.TypeAnswer)
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	answered = true
	for _, c := range early {
		pc.AddICECandidate(c)
	}
	mu.Unlock()
	return b
}

func (b *guestBrowser) waitFrames(t *testing.T, n int64) {
	t.Helper()
	waitUntil(t, func() bool { return b.frames.Load() >= n })
}

func waitUntil(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("timed out")
}

// A guest's microphone hears only what arrives while it has control, and
// each spell without control ends its turn at the microphone.
func TestAGuestsMicrophoneFollowsItsControl(t *testing.T) {
	src := &packets{n: 6}
	s := testSession(&fakeDesktop{})
	mic := &seqMic{}
	s.mic = mic
	// Packets 3 and 4 arrive without control.
	allowed := func() bool { return src.i != 3 && src.i != 4 }
	if err := s.playGuestMic(src, allowed); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if !slices.Equal(mic.got, []uint16{1, 2, 5, 6}) || mic.plays != 2 {
		t.Errorf("heard %v over %d turns, want the packets that came with control over 2", mic.got, mic.plays)
	}
}

// seqMic records the sequence numbers it is given and how many turns.
type seqMic struct {
	got   []uint16
	plays int
}

func (m *seqMic) Play(r track.Reader) error {
	m.plays++
	for {
		p, _, err := r.ReadRTP()
		if err != nil {
			return err
		}
		m.got = append(m.got, p.SequenceNumber)
	}
}
func (*seqMic) Where() string { return "" }
func (*seqMic) Close() error  { return nil }

// packets yields n packets numbered from 1, then EOF.
type packets struct{ i, n int }

func (p *packets) ReadRTP() (*rtp.Packet, interceptor.Attributes, error) {
	if p.i == p.n {
		return nil, nil, io.EOF
	}
	p.i++
	return &rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(p.i)}}, nil, nil
}

// The encoder runs at the lowest target any subscriber's link allows, and
// only the subscriber that takes tiles gets them.
func TestTheStreamRunsAtTheLowestTarget(t *testing.T) {
	owner, guest := newSubscriber(2_000_000, true), newSubscriber(2_000_000, false)
	if target(nil) != 0 || tilePeer(nil) != nil {
		t.Error("no subscribers, yet a target or a tile peer")
	}
	subs := []*subscriber{owner, guest}
	guest.setBitrate(600_000)
	if got := target(subs); got != 600_000 {
		t.Errorf("target %d, want the guest's 600000", got)
	}
	owner.setBitrate(400_000)
	if got := target(subs); got != 400_000 {
		t.Errorf("target %d, want the owner's 400000", got)
	}
	if tilePeer(subs) != owner.peer || tilePeer(subs[1:]) != nil {
		t.Error("tiles go to someone other than the owner's page")
	}
}

// Removing a guest that reads nothing returns at once: the farewell waits
// on its own goroutine, not under the input lock every page needs.
func TestRemovingAGuestThatReadsNothingHoldsUpNobody(t *testing.T) {
	s := guestSession(&fakeTypist{})
	a, b := net.Pipe() // nobody reads b
	conn := protocol.NewConn(a, a, 0, 0)
	s.mu.Lock()
	s.shared.seq++
	g := &guest{id: "g1", user: "alice", uid: 1001, conn: conn}
	s.shared.guests = append(s.shared.guests, g)
	s.mu.Unlock()
	s.shared.uids.add(g.uid)
	done := make(chan struct{})
	go func() {
		s.inputMu.Lock()
		s.applyGuest(protocol.Control{Guest: &protocol.GuestChange{ID: "g1", Action: protocol.GuestRemove}})
		s.inputMu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the removal waited on a guest that reads nothing")
	}
	if s.findGuest("g1") != nil {
		t.Error("the guest is still the session's")
	}
	// The farewell ends once the guest's end is gone.
	a.Close()
	b.Close()
	s.endGuests()
}

// A guest's CLI that churns peers gets one a second.
func TestAGuestGetsOnePeerASecond(t *testing.T) {
	s := guestSession(&fakeTypist{})
	var now atomic.Int64
	now.Store(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC).UnixNano())
	s.shared.now = func() time.Time { return time.Unix(0, now.Load()) }
	endGuestsAtCleanup(t, s)
	g, cli := addGuest(t, s, "alice", 1001)
	offer := func() string {
		b, err := webrtc.NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { b.Close() })
		b.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
		if _, err := b.CreateDataChannel(protocol.ControlChannel, nil); err != nil {
			t.Fatal(err)
		}
		o, err := b.CreateOffer(nil)
		if err != nil {
			t.Fatal(err)
		}
		return o.SDP
	}
	cli.conn.Send(protocol.Message{Type: protocol.TypeOffer, SDP: offer()})
	cli.next(t, protocol.TypeAnswer)
	cli.conn.Send(protocol.Message{Type: protocol.TypeHangup})
	cli.conn.Send(protocol.Message{Type: protocol.TypeOffer, SDP: offer()})
	if m := cli.next(t, protocol.TypeError); !strings.Contains(m.Error, "within a second") {
		t.Errorf("a second peer at once: %q", m.Error)
	}
	now.Add(int64(reopenWait - time.Nanosecond))
	cli.conn.Send(protocol.Message{Type: protocol.TypeOffer, SDP: offer()})
	if m := cli.next(t, protocol.TypeError); !strings.Contains(m.Error, "within a second") {
		t.Errorf("a peer just short of a second on: %q", m.Error)
	}
	now.Add(int64(time.Nanosecond))
	cli.conn.Send(protocol.Message{Type: protocol.TypeOffer, SDP: offer()})
	cli.next(t, protocol.TypeAnswer)
	if s.viewOf(g) == nil {
		t.Error("the guest has no page after its second offer")
	}
}

// What the session sends a page ends once the page loses control, with
// that for its cause, and what it sends the page afterwards does not.
func TestATransferEndsWithItsPagesControl(t *testing.T) {
	s := testSession(&fakeDesktop{})
	v := newViewer(1, &guest{})
	ctx, cancel := s.transfer(t.Context(), v)
	defer cancel()
	s.endTransfersTo(v, errControlTaken)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("the transfer outlived the page's control")
	}
	if !errors.Is(context.Cause(ctx), errControlTaken) {
		t.Errorf("ended for %v", context.Cause(ctx))
	}
	next, stop := s.transfer(t.Context(), v)
	if next.Err() != nil {
		t.Fatalf("a transfer after the revoke began ended: %v", next.Err())
	}
	stop()
	if !errors.Is(context.Cause(next), context.Canceled) {
		t.Errorf("a stopped transfer ended for %v", context.Cause(next))
	}
}
