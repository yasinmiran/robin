//go:build linux

package agent

import (
	"encoding/binary"
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

	"github.com/pion/ice/v4"

	"github.com/yasinmiran/robin/internal/tcpowner"
)

// TestTunnelRefusesAnotherUsersConnection connects to the ICE-TCP listener
// as another local user could, and checks the agent hangs up at once
// rather than hold the connection for the mux's first-packet wait, while
// a connection of this user's, as sshd's forward is, stays.
func TestTunnelRefusesAnotherUsersConnection(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stranger := uint32(os.Getuid()) + 1
	var mu sync.Mutex
	owners := map[string]uint32{} // by the dialling end's address
	own := newOwnConns(ln, slog.New(slog.DiscardHandler), func(c net.Conn) (uint32, error) {
		mu.Lock()
		defer mu.Unlock()
		if uid, ok := owners[c.RemoteAddr().String()]; ok {
			return uid, nil
		}
		return 0, errors.New("no such connection")
	})
	mux := ice.NewTCPMuxDefault(ice.TCPMuxParams{Listener: own, ReadBufferSize: 64})
	// After the connections close, or the mux waits out its first read.
	t.Cleanup(func() { mux.Close() })

	// dial connects as uid from a port picked first, so the owner is on
	// record before the agent accepts.
	dial := func(uid uint32) net.Conn {
		t.Helper()
		probe, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := probe.Addr().(*net.TCPAddr)
		probe.Close()
		mu.Lock()
		owners[addr.String()] = uid
		mu.Unlock()
		d := net.Dialer{LocalAddr: addr}
		c, err := d.Dial("tcp4", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}

	theirs := dial(stranger)
	theirs.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := theirs.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !isReset(err) {
		t.Errorf("another user's connection: read %v, want the agent to hang up", err)
	}

	mine := dial(uint32(os.Getuid()))
	mine.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := mine.Read(make([]byte, 1)); !isTimeout(err) {
		t.Errorf("this user's connection: read %v, want it kept waiting for ICE", err)
	}
}

func isReset(err error) bool { return errors.Is(err, syscall.ECONNRESET) }

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// TestListenTunnelKeepsThisUsersConnection runs the listener Serve opens,
// with the kernel's word on who connected: this process did, and the
// connection is kept for ICE.
func TestListenTunnelKeepsThisUsersConnection(t *testing.T) {
	mux, addr, err := listenTunnel(slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mux.Close() })
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("tunnel listener %q must be loopback-only", addr)
	}
	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := c.Read(make([]byte, 1)); !isTimeout(err) {
		t.Errorf("this user's connection: read %v, want it kept waiting for ICE", err)
	}
}

// closedDialer dials ln and closes its end before ln accepts, as a
// stranger can, and returns the dialling end's address once the kernel no
// longer shows that socket established.
func closedDialer(t *testing.T, ln net.Listener) *net.TCPAddr {
	t.Helper()
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	from := c.LocalAddr().(*net.TCPAddr)
	c.Close()
	for deadline := time.Now().Add(2 * time.Second); tcpState(t, from, ln.Addr().(*net.TCPAddr)) == "01"; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the closed dialer stayed established")
		}
	}
	return from
}

// tcpState is the state /proc/net/tcp shows for the socket from local to
// remote, "01" for established, or "" without one.
func tcpState(t *testing.T, local, remote *net.TCPAddr) string {
	t.Helper()
	b, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		t.Fatal(err)
	}
	addr := func(a *net.TCPAddr) string {
		return fmt.Sprintf("%08X:%04X", binary.NativeEndian.Uint32(a.IP.To4()), a.Port)
	}
	for _, line := range strings.Split(string(b), "\n")[1:] {
		if f := strings.Fields(line); len(f) > 3 && f[1] == addr(local) && f[2] == addr(remote) {
			return f[3]
		}
	}
	return ""
}

// TestPeerUIDRefusesAClosedDialer: once a process closes its end of a
// connection, the kernel shows the socket as root's, which the listener
// trusts, so a stranger who connects, writes and closes would pass.
func TestPeerUIDRefusesAClosedDialer(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	closedDialer(t, ln)
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if uid, err := tcpowner.PeerUID(c); err == nil {
		t.Errorf("PeerUID of a connection whose dialer has closed it = %d, want a refusal", uid)
	}
}

// TestTunnelSkipsAClosedDialersConnection runs the listener with the
// kernel's word on who connected: a connection whose dialer closed it
// before the check never reaches the mux, and the next, this user's, does.
func TestTunnelSkipsAClosedDialersConnection(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedDialer(t, ln)
	mine, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer mine.Close()
	// Both wait in the backlog, so the check comes after the close.
	own := newOwnConns(ln, slog.New(slog.DiscardHandler), tcpowner.PeerUID)
	defer own.Close()
	c, err := own.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got, want := c.RemoteAddr().String(), mine.LocalAddr().String(); got != want {
		t.Errorf("Accept passed on the connection from %s, want this user's from %s", got, want)
	}
}

// TestTunnelMuxForgetsStrangersQuickly: what a connection of this user's
// or root's holds in the mux before ICE takes it, for a first STUN request
// or for a ufrag nobody asked for, goes within seconds, not Pion's 30.
func TestTunnelMuxForgetsStrangersQuickly(t *testing.T) {
	p := tunnelMuxParams(nil)
	for name, d := range map[string]time.Duration{
		"FirstStunBindTimeout": p.FirstStunBindTimeout, "AliveDurationForConnFromStun": p.AliveDurationForConnFromStun,
	} {
		if d <= 0 || d > 5*time.Second {
			t.Errorf("%s = %v, want at most 5s", name, d)
		}
	}
}

// TestTunnelAcceptDoesNotWaitOnALookup: a lookup that takes its time, as
// one can while the host holds many sockets, holds up only the connection
// it is about.
func TestTunnelAcceptDoesNotWaitOnALookup(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer theirs.Close()
	mine, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer mine.Close()

	// The stranger's lookup takes until the test ends. It is told by its
	// address, not by coming first: the checks run in no set order.
	release := make(chan struct{})
	defer close(release)
	own := newOwnConns(ln, slog.New(slog.DiscardHandler), func(c net.Conn) (uint32, error) {
		if c.RemoteAddr().String() == theirs.LocalAddr().String() {
			<-release
			return uint32(os.Getuid()) + 1, nil
		}
		return uint32(os.Getuid()), nil
	})
	defer own.Close()

	got := make(chan net.Conn, 1)
	go func() {
		if c, err := own.Accept(); err == nil {
			got <- c
		}
	}()
	select {
	case c := <-got:
		defer c.Close()
		if c.RemoteAddr().String() != mine.LocalAddr().String() {
			t.Errorf("Accept passed on %s, want this user's %s", c.RemoteAddr(), mine.LocalAddr())
		}
	case <-time.After(time.Second):
		t.Error("Accept waited on the lookup of another connection")
	}
}

// TestOwnConnsClosesWhatItCannotCheck: with every checker busy, a new
// connection is closed at once rather than queued behind them.
func TestOwnConnsClosesWhatItCannotCheck(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	busy := make(chan struct{}, maxOwnerChecks)
	own := newOwnConns(ln, slog.New(slog.DiscardHandler), func(net.Conn) (uint32, error) {
		busy <- struct{}{}
		<-release
		return uint32(os.Getuid()), nil
	})
	dial := func() net.Conn {
		t.Helper()
		c, err := net.Dial("tcp4", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	for range maxOwnerChecks {
		dial()
		<-busy
	}
	late := dial()
	late.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := late.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !isReset(err) {
		t.Errorf("a connection while every checker is busy: read %v, want it closed", err)
	}
	// Once the listener closes, Accept fails, after at most the
	// connections already being checked.
	own.Close()
	close(release)
	for range maxOwnerChecks + 1 {
		c, err := own.Accept()
		if err != nil {
			return
		}
		c.Close()
	}
	t.Error("Accept went on after Close")
}
