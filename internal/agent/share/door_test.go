//go:build linux

package share

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agent/agenttest"
	"github.com/yasinmiran/robin/internal/protocol"
)

// openDoor opens a door for a session of its own, owned by uid 4242, whose
// one account "me" is this process's uid, and returns it with its log.
func openDoor(t *testing.T, users map[string]int) (*Door, *agenttest.LockedBuffer) {
	t.Helper()
	return openDoorWriting(t, users, writeWait)
}

// openDoorWriting is openDoor with each write to a guest bounded by wait.
func openDoorWriting(t *testing.T, users map[string]int, wait time.Duration) (*Door, *agenttest.LockedBuffer) {
	t.Helper()
	var logged agenttest.LockedBuffer
	return openDoorLogging(t, users, wait, &logged), &logged
}

// openDoorLogging is openDoorWriting with the door's log going to log.
func openDoorLogging(t *testing.T, users map[string]int, wait time.Duration, log io.Writer) *Door {
	t.Helper()
	in, _ := testInvites(users)
	in.now = nil
	in.session = fmt.Sprintf("t%d%d", os.Getpid(), time.Now().UnixNano())
	d, err := open(in, slog.New(slog.NewTextHandler(log, nil)), wait)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// gatedWriter holds every write until gate closes.
type gatedWriter struct {
	gate <-chan struct{}
	w    io.Writer
}

func (g gatedWriter) Write(p []byte) (int, error) {
	<-g.gate
	return g.w.Write(p)
}

func dialDoor(t *testing.T, d *Door) net.Conn {
	t.Helper()
	c, err := net.Dial("unix", protocol.ShareSocket(d.inv.session))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func joinLine(t *testing.T, code string) []byte {
	t.Helper()
	b, err := json.Marshal(protocol.Message{Type: protocol.TypeJoin, Join: &protocol.Join{Version: protocol.Version, Code: code}})
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// readReply reads the door's first non-ping message, or the error that
// ended the connection.
func readReply(c net.Conn) (protocol.Message, error) {
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	for {
		n, err := c.Read(chunk)
		buf = append(buf, chunk[:n]...)
		for {
			line, rest, ok := bytes.Cut(buf, []byte("\n"))
			if !ok {
				break
			}
			buf = rest
			var m protocol.Message
			if json.Unmarshal(line, &m) == nil && m.Type != protocol.TypePing {
				return m, nil
			}
		}
		if err != nil {
			return protocol.Message{}, err
		}
	}
}

func TestTheDoorAdmitsAnInvitedUIDWithItsCode(t *testing.T) {
	d, _ := openDoor(t, map[string]int{"me": os.Getuid()})
	inv, err := d.Invites().Invite("me")
	if err != nil {
		t.Fatal(err)
	}
	c := dialDoor(t, d)
	c.Write(joinLine(t, inv.Code))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g, err := d.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Conn.Close()
	if g.UID != os.Getuid() || g.User != "me" {
		t.Errorf("admitted %+v", g)
	}
	// The connection is the guest's signaling from here.
	g.Conn.Send(protocol.Message{Type: protocol.TypeReady, Ready: &protocol.Ready{Share: &protocol.Share{Owner: "o"}}})
	if m, err := readReply(c); err != nil || m.Type != protocol.TypeReady || m.Ready.Share.Owner != "o" {
		t.Fatalf("the guest read %+v, %v", m, err)
	}
	if d.Invites().Invited(os.Getuid()) {
		t.Error("the invitation is still open after it was used")
	}
}

// A connection whose uid holds no invitation is hung up on before a byte
// of it is read: what it wrote is unread when the door closes, which
// resets it, where a read would have let it see EOF.
func TestTheDoorHangsUpOnAnUninvitedUIDUnread(t *testing.T) {
	// The door logs a refusal before it hangs up, so holding its log until
	// the write is in puts the bytes on the socket before the close: a door
	// quicker than the write would otherwise leave nothing unread.
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	var logged agenttest.LockedBuffer
	d := openDoorLogging(t, map[string]int{"other": 4243}, writeWait, gatedWriter{gate, &logged})
	t.Cleanup(release)
	if _, err := d.Invites().Invite("other"); err != nil {
		t.Fatal(err)
	}
	c := dialDoor(t, d)
	if _, err := c.Write([]byte("\x1b]0;robin\x07" + strings.Repeat("x", 1024) + "\n")); err != nil {
		t.Fatal(err)
	}
	release()
	_, err := readReply(c)
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("read after the refusal: %v, want a reset", err)
	}
	waitFor(t, func() bool { return strings.Contains(logged.String(), "no invitation") })
	if l := logged.String(); !strings.Contains(l, fmt.Sprintf("uid=%d", os.Getuid())) || strings.Contains(l, "\x1b") {
		t.Errorf("log of the refusal:\n%q", l)
	}
}

func TestTheDoorRefusesAWrongCode(t *testing.T) {
	d, logged := openDoor(t, map[string]int{"me": os.Getuid()})
	inv, _ := d.Invites().Invite("me")
	c, _ := protocol.ParseJoinCode(inv.Code)
	c.Secret = strings.Repeat("b", len(c.Secret))
	conn := dialDoor(t, d)
	conn.Write(joinLine(t, c.String()))
	m, err := readReply(conn)
	if err != nil || m.Type != protocol.TypeError || m.Error != ErrBadCode.Error() {
		t.Fatalf("reply %+v, %v", m, err)
	}
	if _, err := readReply(conn); !agenttest.HungUp(err) {
		t.Errorf("after the refusal: %v, want hung up", err)
	}
	waitFor(t, func() bool { return strings.Contains(logged.String(), "wrong code") })
	if !d.Invites().Invited(os.Getuid()) {
		t.Error("one wrong code withdrew the invitation")
	}
}

func TestTheDoorRefusesAnythingButJoin(t *testing.T) {
	d, _ := openDoor(t, map[string]int{"me": os.Getuid()})
	if _, err := d.Invites().Invite("me"); err != nil {
		t.Fatal(err)
	}
	for name, line := range map[string]string{
		"start":   `{"type":"start","start":{"version":2}}`,
		"version": fmt.Sprintf(`{"type":"join","join":{"version":%d,"code":"x"}}`, protocol.Version+1),
	} {
		c := dialDoor(t, d)
		c.Write([]byte(line + "\n"))
		if m, err := readReply(c); err != nil || m.Type != protocol.TypeError {
			t.Errorf("%s: reply %+v, %v", name, m, err)
		}
	}
	if !d.Invites().Invited(os.Getuid()) {
		t.Error("a message that was no join spent the invitation")
	}
}

func TestTheDoorWaitsForAJoinOnlySoLong(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out joinWait")
	}
	d, _ := openDoor(t, map[string]int{"me": os.Getuid()})
	d.Invites().Invite("me")
	c := dialDoor(t, d)
	start := time.Now()
	m, err := readReply(c)
	if err != nil || m.Type != protocol.TypeError || !strings.Contains(m.Error, "no join message") {
		t.Fatalf("reply %+v, %v", m, err)
	}
	if took := time.Since(start); took < joinWait-time.Second || took > joinWait+3*time.Second {
		t.Errorf("refused after %s, want about %s", took, joinWait)
	}
}

// A guest the session does not take within handoffWait is told so, and
// its invitation is spent all the same.
func TestTheDoorTellsAGuestNobodyTookIt(t *testing.T) {
	d, logged := openDoor(t, map[string]int{"me": os.Getuid()})
	inv, _ := d.Invites().Invite("me")
	c := dialDoor(t, d)
	c.Write(joinLine(t, inv.Code))
	if m, err := readReply(c); err != nil || m.Type != protocol.TypeError || m.Error != errNoGuests.Error() {
		t.Fatalf("reply %+v, %v", m, err)
	}
	waitFor(t, func() bool { return strings.Contains(logged.String(), "takes no guests") })
}

// At most maxJoining connections present their code at once; the next is
// hung up on, and a slot frees once one of them is done.
func TestTheDoorBoundsThoseJoining(t *testing.T) {
	d, logged := openDoor(t, map[string]int{"me": os.Getuid()})
	inv, _ := d.Invites().Invite("me")
	for range maxJoining {
		dialDoor(t, d) // says nothing, holding a slot
	}
	waitFor(t, func() bool { return len(d.joining) == maxJoining })
	extra := dialDoor(t, d)
	if _, err := readReply(extra); !agenttest.HungUp(err) {
		t.Fatalf("past the bound: %v, want hung up", err)
	}
	waitFor(t, func() bool { return strings.Contains(logged.String(), "too many joining") })
	if !d.Invites().Invited(os.Getuid()) || inv.Code == "" {
		t.Fatal("the invitation went")
	}
}

func TestCloseEndsThoseJoiningAndTheInvitations(t *testing.T) {
	d, _ := openDoor(t, map[string]int{"me": os.Getuid()})
	d.Invites().Invite("me")
	c := dialDoor(t, d)
	waitFor(t, func() bool { return len(d.joining) == 1 })
	done := make(chan error, 1)
	go func() { done <- d.Close() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close waited for a connection that said nothing")
	}
	if m, err := readReply(c); err == nil && m.Type != protocol.TypeError {
		t.Errorf("the connection outlived the door: %+v", m)
	}
	if len(d.Invites().Open()) != 0 {
		t.Error("Close left invitations open")
	}
	if _, err := net.Dial("unix", protocol.ShareSocket(d.inv.session)); err == nil {
		t.Error("the socket still accepts")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Next on a done context: %v", err)
	}
}

func TestDialChecksTheListenerIsTheOwner(t *testing.T) {
	id := fmt.Sprintf("d%d%d", os.Getpid(), time.Now().UnixNano())
	ln, err := net.Listen("unix", protocol.ShareSocket(id))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	c, err := Dial(protocol.JoinCode{Session: id, Owner: os.Getuid(), Secret: "x"})
	if err != nil {
		t.Fatalf("the owner's own socket: %v", err)
	}
	c.Close()
	if _, err := Dial(protocol.JoinCode{Session: id, Owner: os.Getuid() + 1, Secret: "x"}); err == nil || !strings.Contains(err.Error(), "not to its owner") {
		t.Errorf("a socket held by another uid: %v", err)
	}
	if _, err := Dial(protocol.JoinCode{Session: id + "x", Owner: os.Getuid(), Secret: "x"}); err == nil || !strings.Contains(err.Error(), "no session") {
		t.Errorf("no socket: %v", err)
	}
}

// Relay carries both ways until either side ends: the door hanging up
// ends it, and so does its context.
func TestRelayCarriesBothWays(t *testing.T) {
	unixA, unixB := unixPair(t)
	defer unixB.Close()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- Relay(context.Background(), unixA, inR, outW) }()
	go inW.Write([]byte("to the door\n"))
	buf := make([]byte, 64)
	n, _ := unixB.Read(buf)
	if string(buf[:n]) != "to the door\n" {
		t.Errorf("the door read %q", buf[:n])
	}
	go unixB.Write([]byte("to the guest\n"))
	n, _ = outR.Read(buf)
	if string(buf[:n]) != "to the guest\n" {
		t.Errorf("the guest read %q", buf[:n])
	}
	unixB.Close()
	if err := <-done; err != nil {
		t.Errorf("Relay after the door hung up: %v", err)
	}

	unixA, unixB = unixPair(t)
	defer unixB.Close()
	ctx, cancel := context.WithCancel(context.Background())
	inR, _ = io.Pipe()
	go func() { done <- Relay(ctx, unixA, inR, io.Discard) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Relay ended by its context: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Relay outlived its context")
	}
}

func unixPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("unix", "@"+fmt.Sprintf("robin-pair-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	a, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	return a, b
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("timed out")
}

// A guest that stops reading fails its connection within writeWait rather
// than hold up whoever writes to it.
func TestAGuestThatStopsReadingIsDropped(t *testing.T) {
	d, _ := openDoorWriting(t, map[string]int{"me": os.Getuid()}, 200*time.Millisecond)
	inv, _ := d.Invites().Invite("me")
	c := dialDoor(t, d)
	c.Write(joinLine(t, inv.Code))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g, err := d.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Conn.Close()
	big := protocol.Message{Type: protocol.TypeError, Error: strings.Repeat("x", 64<<10)}
	start := time.Now()
	for i := 0; ; i++ {
		if err := g.Conn.Send(big); err != nil {
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("send %d: %v, want the write bound", i, err)
			}
			break
		}
		if time.Since(start) > 5*time.Second {
			t.Fatal("a guest that reads nothing took writes for 5 s")
		}
	}
}
