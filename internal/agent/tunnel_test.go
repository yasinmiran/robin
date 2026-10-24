//go:build linux

package agent

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/ice/v4"
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
	own := &ownConns{Listener: ln, log: slog.New(slog.DiscardHandler), uidOf: func(c net.Conn) (uint32, error) {
		mu.Lock()
		defer mu.Unlock()
		if uid, ok := owners[c.RemoteAddr().String()]; ok {
			return uid, nil
		}
		return 0, errors.New("no such connection")
	}}
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

func isReset(err error) bool { return err != nil && strings.Contains(err.Error(), "reset") }

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

// TestConnUIDIsTheDialer looks a loopback connection's other end up in the
// kernel's socket table: this process dialled it, so it is this user's.
func TestConnUIDIsTheDialer(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	accepted, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	uid, err := connUID(accepted)
	if err != nil || uid != uint32(os.Getuid()) {
		t.Errorf("connUID = %d, %v; want %d", uid, err, os.Getuid())
	}
	if _, err := connUID(&net.UnixConn{}); err == nil {
		t.Error("connUID of a unix connection succeeded")
	}
	gone := addrConn{
		local:  &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: ln.Addr().(*net.TCPAddr).Port},
		remote: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1},
	}
	if _, err := connUID(gone); err == nil {
		t.Error("connUID found the other end of a connection that does not exist")
	}
	v6 := addrConn{local: &net.TCPAddr{IP: net.IPv6loopback, Port: 1}, remote: &net.TCPAddr{IP: net.IPv6loopback, Port: 2}}
	if _, err := connUID(v6); err == nil {
		t.Error("connUID took a connection over IPv6")
	}
}

// addrConn is a connection that is nothing but its two addresses.
type addrConn struct {
	net.Conn
	local, remote net.Addr
}

func (c addrConn) LocalAddr() net.Addr  { return c.local }
func (c addrConn) RemoteAddr() net.Addr { return c.remote }
