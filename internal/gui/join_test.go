package gui

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/protocol"
	"github.com/yasinmiran/robin/internal/relay"
)

const testCode = "a1b2c3d4-1000-k3j4m5n6p7q2r3s4t5u6v7w2x3"

func TestCheckJoin(t *testing.T) {
	if err := checkJoin(Options{}); err != nil {
		t.Errorf("no join: %v", err)
	}
	if err := checkJoin(Options{Join: testCode, Mic: true, HiDPI: true}); err != nil {
		t.Errorf("a plain join: %v", err)
	}
	for want, o := range map[string]Options{
		"not a join code": {Join: "a1b2c3d4"},
		"--share":         {Join: testCode, Share: []string{"bob"}},
		"--attach":        {Join: testCode, Attach: ":0"},
		"--id":            {Join: testCode, Resume: "ab12"},
		"--new":           {Join: testCode, New: true},
		"camera":          {Join: testCode, Camera: true},
		"--curtain":       {Join: testCode, Silent: true},
	} {
		if err := checkJoin(o); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: %v, want it to name %q", o, err, want)
		}
	}
	// Run refuses before it dials anything.
	if err := Run(context.Background(), Options{Join: "nonsense", Dest: "nowhere.invalid"}); err == nil || !strings.Contains(err.Error(), "not a join code") {
		t.Errorf("Run with a bad code: %v", err)
	}
}

func TestJoinArgsCarryNoSecret(t *testing.T) {
	code, err := protocol.ParseJoinCode(testCode)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(joinArgs("/home/a/.cache/robin/robin-agent-x", code), " ")
	if got != "/home/a/.cache/robin/robin-agent-x join -session a1b2c3d4 -owner 1000" {
		t.Errorf("join args %q", got)
	}
}

// The owner's --share asks for an invitation per user and tells the owner
// how each joins; a user the session cannot take is reported, and the
// rest still invited.
func TestInviteTellsTheOwnerHowToJoin(t *testing.T) {
	a, f := agentOverPipes(t)
	exp := time.Date(2026, 10, 30, 15, 4, 0, 0, time.Local)
	go func() {
		for m := range f.calls {
			if m.Type != protocol.TypeInvite {
				continue
			}
			inv := protocol.Invitation{User: m.Invite.User, Code: testCode, Expires: exp}
			if m.Invite.User == "nobody" {
				inv = protocol.Invitation{User: "nobody", Error: "no user nobody on this host"}
			}
			f.conn.Send(protocol.Message{Type: protocol.TypeCandidate}) // anything else waits its turn
			f.conn.Send(protocol.Message{Type: protocol.TypeInvited, Invitation: &inv})
		}
	}()
	var stderr lockedBuffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := invite(ctx, a, []string{"alice", "nobody"}, "me@box.lan", &stderr); err != nil {
		t.Fatal(err)
	}
	want := "robin: alice may join once, until 3:04PM, with: robin gui --join " + testCode + " alice@box.lan\n" +
		"robin: cannot share with nobody: no user nobody on this host\n"
	if got := stderr.String(); got != want {
		t.Errorf("stderr:\n%s\nwant:\n%s", got, want)
	}
	if err := invite(ctx, a, nil, "box", &stderr); err != nil {
		t.Errorf("no users: %v", err)
	}
}

func TestAwaitInvited(t *testing.T) {
	a, f := agentOverPipes(t)
	go f.conn.Send(protocol.Message{Type: protocol.TypeError, Error: "no"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := a.awaitInvited(ctx); err == nil || !strings.Contains(err.Error(), "agent: no") {
		t.Errorf("an error answer: %v", err)
	}
	f.outW.Close()
	if _, err := a.awaitInvited(ctx); err == nil {
		t.Error("a closed connection answered")
	}
	a2, _ := agentOverPipes(t)
	short, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if _, err := a2.awaitInvited(short); err == nil {
		t.Error("silence answered")
	}
}

func TestGuestNote(t *testing.T) {
	r := &protocol.Ready{Display: ":5", Share: &protocol.Share{Owner: "robin"}}
	got := guestNote(Options{Dest: "alice@box"}, r)
	if got != "robin: joined robin's desktop :5 on alice@box; view only until robin gives you control; press Ctrl+C to leave" {
		t.Errorf("note %q", got)
	}
	if got := guestNote(Options{Dest: "alice@box", Mic: true}, r); !strings.Contains(got, "takes no microphone") {
		t.Errorf("--mic on a session without one: %q", got)
	}
	r.Mic = &protocol.Device{Available: true}
	if got := guestNote(Options{Dest: "alice@box", Mic: true}, r); strings.Contains(got, "microphone") {
		t.Errorf("--mic on a session with one: %q", got)
	}
}

// guestOverPipes runs h.guest with a fake host agent that answers the
// join with ready, and returns the agent and what the guest returns.
func guestOverPipes(t *testing.T, ctx context.Context, ready protocol.Message) (*fakeHostAgent, *lockedBuffer, <-chan error) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	h, _, stderr := sshHost(t)
	h.o.Join, h.o.Transport, h.o.NoBrowser, h.o.Listen = testCode, relay.Direct, true, "127.0.0.1:0"
	h.o.Stdout = &lockedBuffer{}
	a, f := agentOverPipes(t)
	go func() {
		if m := <-f.calls; m.Type == protocol.TypeJoin && m.Join.Code == testCode && m.Join.Version == protocol.Version {
			f.conn.Send(ready)
		}
	}()
	done := make(chan error, 1)
	go func() { done <- h.guest(ctx, a) }()
	return f, stderr, done
}

func guestReady() protocol.Message {
	return protocol.Message{Type: protocol.TypeReady, Ready: &protocol.Ready{
		Version: protocol.Version, Display: ":5", Width: 800, Height: 600, Share: &protocol.Share{Owner: "robin"},
	}}
}

// Ctrl+C leaves: the guest says bye, which the agent answers, and returns
// without an error.
func TestGuestLeavesOnCtrlC(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f, stderr, done := guestOverPipes(t, ctx, guestReady())
	waitText(t, stderr, "joined robin's desktop :5")
	cancel()
	select {
	case m := <-f.calls:
		if m.Type != protocol.TypeBye {
			t.Fatalf("the guest sent %q on Ctrl+C", m.Type)
		}
		f.conn.Send(protocol.Message{Type: protocol.TypeEnded, Error: "you left the session"})
	case <-time.After(10 * time.Second):
		t.Fatal("no bye")
	}
	if err := waitDone(t, done); err != nil {
		t.Errorf("leaving: %v", err)
	}
}

// The owner removing the guest ends the guest's CLI, which says why.
func TestGuestEndsWhenRemoved(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f, stderr, done := guestOverPipes(t, ctx, guestReady())
	waitText(t, stderr, "joined robin's desktop")
	f.conn.Send(protocol.Message{Type: protocol.TypeEnded, Error: "the owner removed you from the session"})
	if err := waitDone(t, done); err != nil {
		t.Errorf("removed: %v", err)
	}
	waitText(t, stderr, "robin: session ended: the owner removed you from the session")
}

func TestGuestRefusesWhatIsNoGuestsReady(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, done := guestOverPipes(t, ctx, protocol.Message{Type: protocol.TypeReady, Ready: &protocol.Ready{Version: protocol.Version}})
	if err := waitDone(t, done); err == nil || !strings.Contains(err.Error(), "as no guest's") {
		t.Errorf("an owner's Ready to a join: %v", err)
	}
	_, _, done = guestOverPipes(t, ctx, protocol.Message{Type: protocol.TypeError, Error: "this join code is not valid"})
	var agentErr *agentError
	if err := waitDone(t, done); !errors.As(err, &agentErr) || !strings.Contains(err.Error(), "not valid") {
		t.Errorf("a refused join: %v", err)
	}
}

func waitDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("the guest did not return")
		return nil
	}
}

func waitText(t *testing.T, b *lockedBuffer, want string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(b.String(), want); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%q never appeared in:\n%s", want, b.String())
		}
	}
}

// A join of a session the host does not have fails with the agent's word
// for it, or, in a checkout without built agents, at the deploy.
func TestRunGuestOfNoSession(t *testing.T) {
	o, _, _ := localhost(t, "XDG_CACHE_HOME="+t.TempDir())
	o.Join = "zz000000-1000-k3j4m5n6p7q2r3s4t5u6v7w2x3"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := Run(ctx, o)
	if err == nil {
		t.Fatal("joined a session that is not there")
	}
	if !strings.Contains(err.Error(), "no session zz000000") && !strings.Contains(err.Error(), "agent") {
		t.Errorf("Run: %v", err)
	}
	t.Log(err)
}

// guestWith is guestOverPipes with the host's options changed by change
// first, and the host returned too.
// What the guest sends after its join comes on the channel returned.
func guestWith(t *testing.T, ctx context.Context, ready protocol.Message, change func(*host)) (*host, *fakeHostAgent, <-chan protocol.Message, <-chan error) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	h, _, _ := sshHost(t)
	h.o.Join, h.o.Transport, h.o.NoBrowser, h.o.Listen = testCode, relay.Direct, true, "127.0.0.1:0"
	h.o.Stdout = &lockedBuffer{}
	change(h)
	a, f := agentOverPipes(t)
	rest := make(chan protocol.Message, 8)
	go func() {
		if m := <-f.calls; m.Type == protocol.TypeJoin {
			f.conn.Send(ready)
		}
		for m := range f.calls {
			rest <- m
		}
	}()
	done := make(chan error, 1)
	go func() { done <- h.guest(ctx, a) }()
	return h, f, rest, done
}

// A page server that cannot listen ends the join, with a bye so the
// session lets the guest go at once.
func TestGuestWhosePageCannotListenSaysBye(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, f, rest, done := guestWith(t, ctx, guestReady(), func(h *host) { h.o.Listen = "127.0.0.1:not-a-port" })
	select {
	case m := <-rest:
		if m.Type != protocol.TypeBye {
			t.Fatalf("the guest sent %q", m.Type)
		}
		f.conn.Send(protocol.Message{Type: protocol.TypeEnded, Error: "you left the session"})
	case <-time.After(10 * time.Second):
		t.Fatal("no bye")
	}
	if err := waitDone(t, done); err == nil {
		t.Error("joined without a page")
	}
}

// Through the tunnel alone the guest's page asks no STUN server, and a
// lost SSH connection ends the guest with the reason.
func TestGuestOverTheTunnelEndsWithItsConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _, _, done := guestWith(t, ctx, guestReady(), func(h *host) {
		h.o.Transport = relay.Tunnel
		h.o.STUN = []string{"stun:stun.example:3478"}
	})
	time.Sleep(500 * time.Millisecond)
	h.m.Close()
	if err := waitDone(t, done); err == nil || !strings.Contains(err.Error(), "SSH connection") {
		t.Errorf("the guest after its connection went: %v", err)
	}
}

func TestInviteOverADeadAgent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, _ := agentOverPipes(t)
	a.conn.Close()
	if err := invite(ctx, a, []string{"alice"}, "box", io.Discard); err == nil {
		t.Error("invited over a closed connection")
	}
	a, f := agentOverPipes(t)
	f.outW.Close()
	if err := invite(ctx, a, []string{"alice"}, "box", io.Discard); err == nil {
		t.Error("an invite with no answer went through")
	}
}

// A join whose code the agent never gets fails as the agent's.
func TestGuestOverADeadAgent(t *testing.T) {
	h, _, _ := sshHost(t)
	h.o.Join = testCode
	a, _ := agentOverPipes(t)
	a.conn.Close()
	if err := h.guest(context.Background(), a); err == nil {
		t.Error("joined over a closed connection")
	}
}
