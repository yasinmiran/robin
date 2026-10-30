//go:build linux

package main

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agent/agenttest"
	"github.com/yasinmiran/robin/internal/protocol"
)

// joinProxy is a running `robin-agent join`, as a guest's CLI runs it.
type joinProxy struct {
	*proxy
}

// joinAgent starts robin-agent join for the session id of owner uid, as
// cmd makes it (the stranger's account, or this one).
func joinAgent(t *testing.T, cmd *exec.Cmd) *joinProxy {
	t.Helper()
	cmd.Env = append(os.Environ(), "ROBIN_AGENT_MAIN=1")
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{t: t, cmd: cmd, in: in, log: &syncBuffer{}, done: make(chan struct{}), msgs: make(chan protocol.Message, 256)}
	cmd.Stderr = p.log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		p.exitErr = cmd.Wait()
		close(p.done)
	}()
	p.conn = protocol.NewConn(out, in, protocol.DefaultPingInterval, 0)
	go func() {
		defer close(p.msgs)
		for {
			m, err := p.conn.Recv(context.Background())
			if err != nil {
				return
			}
			p.msgs <- m
		}
	}()
	t.Cleanup(func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-p.done
	})
	return &joinProxy{p}
}

// lastWord is the last message the guest's proxy carried before it
// closed, and whether it closed within the wait.
func (p *joinProxy) lastWord(within time.Duration) (protocol.Message, bool) {
	var last protocol.Message
	timeout := time.After(within)
	for {
		select {
		case m, ok := <-p.msgs:
			if !ok {
				return last, true
			}
			last = m
		case <-timeout:
			return last, false
		}
	}
}

func invite(t *testing.T, p *proxy, user string) protocol.Invitation {
	t.Helper()
	p.conn.Send(protocol.Message{Type: protocol.TypeInvite, Invite: &protocol.Invite{User: user}})
	inv := p.recv(protocol.TypeInvited).Invitation
	if inv.Error != "" || inv.Code == "" {
		t.Fatalf("invitation for %s: %+v", user, inv)
	}
	return *inv
}

// The cross-user path end to end, with the real agent: the owner invites
// the second account, whose own robin-agent join reaches the session's
// door, presents the code and is a guest; a second use of the code, a code
// presented by an account it was not made for, and a join aimed at a
// socket its owner does not hold all fail; and when the owner's controller
// goes, so does the guest, its proxy included.
func TestAnotherUserJoinsAsAGuest(t *testing.T) {
	needXvfb(t)
	s := agenttest.NewStranger(t)
	exe := agentCopy(t)
	owner := serveAgent(t, exe, "-keep", "1h")
	ready, dir := start(owner, exe)
	ownerUID := strconv.Itoa(os.Getuid())
	join := func() *joinProxy {
		return joinAgent(t, s.Command(s.Exe, "join", "-session", ready.ID, "-owner", ownerUID))
	}

	// Another account the owner did not invite is hung up on unread.
	inv := invite(t, owner, "nobody")
	uninvited := join()
	uninvited.conn.Send(protocol.Message{Type: protocol.TypeJoin, Join: &protocol.Join{Version: protocol.Version, Code: inv.Code}})
	if m, closed := uninvited.lastWord(10 * time.Second); !closed || m.Type != "" {
		t.Fatalf("an uninvited account's join: closed %v after %+v", closed, m)
	}
	waitLog(t, dir, "guest refused: no invitation", "uid="+strconv.Itoa(int(s.UID)))

	inv = invite(t, owner, "robin-stranger")
	if code, err := protocol.ParseJoinCode(inv.Code); err != nil || code.Session != ready.ID || strconv.Itoa(code.Owner) != ownerUID {
		t.Fatalf("code %q: %+v, %v", inv.Code, code, err)
	}
	g := join()
	g.conn.Send(protocol.Message{Type: protocol.TypeJoin, Join: &protocol.Join{Version: protocol.Version, Code: inv.Code}})
	gr := g.recv(protocol.TypeReady).Ready
	if gr.Share == nil || gr.Share.Owner != userName(t) || gr.Share.Control || gr.Display != ready.Display || gr.TunnelAddr != ready.TunnelAddr {
		t.Fatalf("the guest's ready %+v", gr)
	}

	again := join()
	again.conn.Send(protocol.Message{Type: protocol.TypeJoin, Join: &protocol.Join{Version: protocol.Version, Code: inv.Code}})
	if m, closed := again.lastWord(10 * time.Second); !closed {
		t.Fatal("a second use of the code was kept")
	} else if m.Type != "" && m.Type != protocol.TypeError {
		t.Fatalf("a second use of the code got %+v", m)
	}

	// A join aimed at a socket the named owner does not hold stops at the
	// guest's own end.
	wrongOwner := joinAgent(t, s.Command(s.Exe, "join", "-session", ready.ID, "-owner", "4242"))
	if err := wrongOwner.exited(); err == nil || !strings.Contains(wrongOwner.log.String(), "not to its owner") {
		t.Fatalf("a join for the wrong owner: %v\n%s", err, wrongOwner.log)
	}

	// The owner's controller goes: the guest is let go and its proxy ends.
	owner.killGroup()
	m, closed := g.lastWord(10 * time.Second)
	if !closed || m.Type != protocol.TypeEnded || m.Error != "the owner disconnected" {
		t.Fatalf("the guest after its owner left: closed %v, last %+v", closed, m)
	}
	if err := g.exited(); err != nil {
		t.Errorf("the guest's proxy: %v\n%s", err, g.log)
	}
	if st := waitSession(t, dir, func(s protocol.Session) bool { return !s.Connected }); !alive(st.PID) {
		t.Fatal("the daemon died with its owner's controller")
	}
}

// A guest never outlives the owner's daemon: a bye ends the session, and
// the guest's proxy with it.
func TestAGuestGoesWithTheSession(t *testing.T) {
	needXvfb(t)
	s := agenttest.NewStranger(t)
	exe := agentCopy(t)
	owner := serveAgent(t, exe)
	ready, dir := start(owner, exe)
	inv := invite(t, owner, "robin-stranger")
	g := joinAgent(t, s.Command(s.Exe, "join", "-session", ready.ID, "-owner", strconv.Itoa(os.Getuid())))
	g.conn.Send(protocol.Message{Type: protocol.TypeJoin, Join: &protocol.Join{Version: protocol.Version, Code: inv.Code}})
	g.recv(protocol.TypeReady)

	owner.conn.Send(protocol.Message{Type: protocol.TypeBye})
	owner.recv(protocol.TypeEnded)
	m, closed := g.lastWord(10 * time.Second)
	if !closed || m.Type != protocol.TypeEnded || m.Error != "the session ended" {
		t.Fatalf("the guest after the session ended: closed %v, last %+v", closed, m)
	}
	if err := g.exited(); err != nil {
		t.Errorf("the guest's proxy: %v\n%s", err, g.log)
	}
	waitRunDirGone(t, dir, 10*time.Second)
	if c, err := net.Dial("unix", protocol.ShareSocket(ready.ID)); err == nil {
		c.Close()
		t.Error("the guests' door outlived the session")
	}
}

// join takes only a session and an owner it could have been given, and
// says so when there is no session to join.
func TestJoinRefusesWhatNamesNoSession(t *testing.T) {
	exe := agentCopy(t)
	for _, args := range [][]string{
		{"join"}, {"join", "-session", "../x", "-owner", "0"}, {"join", "-session", "abc", "-owner", "-3"}, {"join", "-bogus"},
	} {
		cmd := exec.Command(exe, args...)
		cmd.Env = append(os.Environ(), "ROBIN_AGENT_MAIN=1")
		out, err := cmd.CombinedOutput()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
			t.Errorf("%q: %v\n%s", args, err, out)
		}
	}
	cmd := exec.Command(exe, "join", "-session", "zz"+strconv.Itoa(os.Getpid()), "-owner", "0")
	cmd.Env = append(os.Environ(), "ROBIN_AGENT_MAIN=1")
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(string(out), "no session") {
		t.Errorf("join of no session: %v\n%s", err, out)
	}
}

// waitLog waits for the daemon's log in dir to hold every one of want.
func waitLog(t *testing.T, dir string, want ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, _ := os.ReadFile(filepath.Join(dir, "agent.log"))
		ok := true
		for _, w := range want {
			ok = ok && strings.Contains(string(b), w)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon's log lacks %q:\n%s", want, b)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func userName(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("id", "-un").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}
