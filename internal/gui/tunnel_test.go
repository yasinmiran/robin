package gui

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pion/stun/v4"

	"github.com/yasinmiran/robin/internal/relay"
	"github.com/yasinmiran/robin/internal/sshmux"
	"github.com/yasinmiran/robin/internal/tcpowner"
)

// fakeForward is a dialer standing in for the SSH forward's local end: a
// unix socket whose listener the test holds.
type fakeForward struct{ sock string }

func (f fakeForward) Dial() (net.Conn, error) { return net.Dial("unix", f.sock) }

// testTunnel serves a tunnel on loopback whose forward leads to a socket of
// the test's; what reaches that socket comes out of accepted.
func testTunnel(t *testing.T, ln net.Listener, firstCheck time.Duration) (*tunnel, <-chan net.Conn) {
	t.Helper()
	fwd, accepted := testForward(t)
	tun := &tunnel{ln: ln, log: slog.New(slog.DiscardHandler), firstCheck: firstCheck, fwd: fwd, selfOf: tcpowner.PeerIsSelf}
	go tun.serve()
	t.Cleanup(func() { ln.Close() })
	return tun, accepted
}

// testForward is a forward whose connections come out of accepted.
func testForward(t *testing.T) (fakeForward, <-chan net.Conn) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "fwd.sock")
	u, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 8)
	go func() {
		for {
			c, err := u.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	t.Cleanup(func() { u.Close() })
	return fakeForward{sock}, accepted
}

func loopback(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// iceAnswer is as much of an agent's answer as the tunnel reads.
func iceAnswer(ufrag, pwd string) string {
	return "v=0\r\na=fingerprint:sha-256 AB:CD\r\nm=video 9 UDP/TLS/RTP/SAVPF 102\r\na=mid:0\r\na=ice-ufrag:" + ufrag + "\r\na=ice-pwd:" + pwd + "\r\n"
}

// framedCheck is a connectivity check as the browser's ICE-TCP sends it:
// a STUN Binding request behind its RFC 4571 length.
func framedCheck(t *testing.T, username, pwd string) []byte {
	t.Helper()
	m, err := stun.Build(stun.TransactionID, stun.BindingRequest, stun.NewUsername(username),
		stun.RawAttribute{Type: stun.AttrICEControlling, Value: make([]byte, 8)},
		stun.NewShortTermIntegrity(pwd), stun.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	return framed(m.Raw)
}

func framed(msg []byte) []byte {
	return append(binary.BigEndian.AppendUint16(nil, uint16(len(msg))), msg...)
}

func dialTunnel(t *testing.T, tun *tunnel, first []byte) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp4", tun.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if first != nil {
		if _, err := c.Write(first); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// refused checks the tunnel closed c without passing anything back.
func refused(t *testing.T, name string, c net.Conn) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err := c.Read(make([]byte, 1))
	if errors.Is(err, os.ErrDeadlineExceeded) || err == nil {
		t.Errorf("%s: still open (%v)", name, err)
	}
}

func spliced(t *testing.T, accepted <-chan net.Conn, want []byte) net.Conn {
	t.Helper()
	select {
	case u := <-accepted:
		t.Cleanup(func() { u.Close() })
		got := make([]byte, len(want))
		u.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.ReadFull(u, got); err != nil || string(got) != string(want) {
			t.Fatalf("the forward got %q, %v; want the check intact", got, err)
		}
		return u
	case <-time.After(3 * time.Second):
		t.Fatal("the check never reached the forward")
		return nil
	}
}

// TestTunnelRefusesStrangers: any user of this machine can connect to the
// tunnel's loopback port, and each connection would be an SSH channel and a
// connection to the agent's ICE-TCP listener. Only a connection whose first
// message is a check signed with the agent's current ICE password, which
// only the page knows, is let through; one that says nothing is dropped
// once firstCheck is up.
func TestTunnelRefusesStrangers(t *testing.T) {
	tun, accepted := testTunnel(t, loopback(t), 300*time.Millisecond)
	const ufrag, pwd = "agentufrag", "agentpasswordagentpasswo"

	refused(t, "a check before any answer", dialTunnel(t, tun, framedCheck(t, ufrag+":brws", pwd)))
	tun.answered(iceAnswer(ufrag, pwd))
	refused(t, "silence", dialTunnel(t, tun, nil))
	refused(t, "half a length", dialTunnel(t, tun, []byte{0}))
	refused(t, "another ufrag", dialTunnel(t, tun, framedCheck(t, "stranger:brws", pwd)))
	refused(t, "a guessed password", dialTunnel(t, tun, framedCheck(t, ufrag+":brws", "guessedpasswordguessedpa")))
	refused(t, "not STUN", dialTunnel(t, tun, framed([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))))
	refused(t, "a frame over the agent's limit", dialTunnel(t, tun, append([]byte{0xff, 0xff}, make([]byte, 600)...)))
	refused(t, "a frame cut short", dialTunnel(t, tun, framedCheck(t, ufrag+":brws", pwd)[:40]))
	select {
	case <-accepted:
		t.Fatal("a stranger reached the forward")
	default:
	}

	check := framedCheck(t, ufrag+":brws", pwd)
	c := dialTunnel(t, tun, check)
	u := spliced(t, accepted, check)
	u.Write([]byte("answer"))
	got := make([]byte, len("answer"))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "answer" {
		t.Fatalf("the browser side got %q, %v", got, err)
	}

	// An ICE restart or a reload brings new credentials; a check with the
	// old ones no longer gets through, and the spliced connection stays.
	tun.answered(iceAnswer("agentufrag2", "agentpassword2agentpassw"))
	refused(t, "the last answer's credentials", dialTunnel(t, tun, framedCheck(t, ufrag+":brws", pwd)))
	fresh := framedCheck(t, "agentufrag2:brws", "agentpassword2agentpassw")
	dialTunnel(t, tun, fresh)
	spliced(t, accepted, fresh)
	c.Write([]byte("still"))
	got = make([]byte, len("still"))
	u.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(u, got); err != nil || string(got) != "still" {
		t.Fatalf("the first spliced connection got %q, %v after new credentials", got, err)
	}

	// An answer the tunnel cannot read leaves the credentials as they were.
	tun.answered("v=0\r\n")
	again := framedCheck(t, "agentufrag2:brws", "agentpassword2agentpassw")
	dialTunnel(t, tun, again)
	spliced(t, accepted, again)
}

// TestTunnelAdmitsTheBrowserDuringAFlood: another user of this machine
// who keeps opening silent connections to the tunnel's port would push
// the browser's out of the connections waiting for their check, and so
// keep the session off the tunnel. Where the kernel says who opened a
// connection, another user's is closed before it counts.
func TestTunnelAdmitsTheBrowserDuringAFlood(t *testing.T) {
	ln := loopback(t)
	fwd, accepted := testForward(t)
	var mu sync.Mutex
	mine := map[string]bool{} // dialling ends of this user's
	tun := &tunnel{ln: ln, log: slog.New(slog.DiscardHandler), firstCheck: 5 * time.Second, fwd: fwd,
		selfOf: func(c net.Conn) (bool, error) {
			mu.Lock()
			defer mu.Unlock()
			return mine[c.RemoteAddr().String()], nil
		}}
	go tun.serve()
	t.Cleanup(func() { ln.Close() })
	tun.answered(iceAnswer("agentufrag", "agentpasswordagentpasswo"))

	stop := make(chan struct{})
	flooded := make(chan int)
	go func() {
		var held []net.Conn
		n := 0
		defer func() {
			for _, c := range held {
				c.Close()
			}
			flooded <- n
		}()
		for {
			select {
			case <-stop:
				return
			default:
			}
			c, err := net.Dial("tcp4", ln.Addr().String())
			if err != nil {
				continue
			}
			n++
			if held = append(held, c); len(held) > 4*maxPending {
				held[0].Close()
				held = held[1:]
			}
		}
	}()
	defer func() {
		close(stop)
		t.Logf("%d connections from another user during the test", <-flooded)
	}()

	// The browser's connection comes from a port put on record first, so
	// its owner is known before the tunnel accepts it.
	probe := loopback(t)
	from := probe.Addr().(*net.TCPAddr)
	probe.Close()
	mu.Lock()
	mine[from.String()] = true
	mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	c, err := (&net.Dialer{LocalAddr: from}).Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(50 * time.Millisecond)
	check := framedCheck(t, "agentufrag:brws", "agentpasswordagentpasswo")
	c.Write(check)
	spliced(t, accepted, check)
}

// TestTunnelTellsUsersApart: a connection the kernel names as this
// user's waits for its check, and one it names as another user's, or
// cannot place although it was asked, is closed at once. Where it cannot
// be asked at all, every connection waits for its check as before.
func TestTunnelTellsUsersApart(t *testing.T) {
	for _, tc := range []struct {
		name   string
		self   bool
		err    error
		admits bool
	}{
		{"this user's", true, nil, true},
		{"another user's", false, nil, false},
		{"not found", false, errors.New("the other end is not a socket of this host"), false},
		{"root's, once its dialer closed it", false, errors.New("the other end is no longer held open (state 7)"), false},
		{"the kernel cannot be asked", false, fmt.Errorf("%w: sock_diag: permission denied", tcpowner.ErrUnavailable), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fwd, accepted := testForward(t)
			ln := loopback(t)
			tun := &tunnel{ln: ln, log: slog.New(slog.DiscardHandler), firstCheck: 5 * time.Second, fwd: fwd,
				selfOf: func(net.Conn) (bool, error) { return tc.self, tc.err }}
			go tun.serve()
			t.Cleanup(func() { ln.Close() })
			tun.answered(iceAnswer("agentufrag", "agentpasswordagentpasswo"))
			c := dialTunnel(t, tun, nil)
			if !tc.admits {
				refused(t, tc.name, c)
				return
			}
			check := framedCheck(t, "agentufrag:brws", "agentpasswordagentpasswo")
			c.Write(check)
			spliced(t, accepted, check)
		})
	}
}

// flakyListener fails its first Accept the way a process out of file
// descriptors does.
type flakyListener struct {
	net.Listener
	failed atomic.Bool
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.failed.CompareAndSwap(false, true) {
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept4", syscall.EMFILE)}
	}
	return l.Listener.Accept()
}

// TestTunnelOutlivesAFailedAccept: running out of file descriptors for a
// moment costs the tunnel that one connection, not the rest of the session.
func TestTunnelOutlivesAFailedAccept(t *testing.T) {
	tun, accepted := testTunnel(t, &flakyListener{Listener: loopback(t)}, 300*time.Millisecond)
	tun.answered(iceAnswer("agentufrag", "agentpasswordagentpasswo"))
	check := framedCheck(t, "agentufrag:brws", "agentpasswordagentpasswo")
	dialTunnel(t, tun, check)
	spliced(t, accepted, check)
}

// TestTunnelBoundsPendingConnections: each connection still to send its
// check holds a descriptor until firstCheck, and any user of this machine
// can open them until the CLI runs out. Past maxPending the oldest goes at
// once. The browser sends its check a loopback round trip after it
// connects, so the oldest is not the browser's, and a good check that
// comes after a crowd of silent connections is still spliced.
func TestTunnelBoundsPendingConnections(t *testing.T) {
	tun, accepted := testTunnel(t, loopback(t), 5*time.Second)
	tun.answered(iceAnswer("agentufrag", "agentpasswordagentpasswo"))
	var silent []net.Conn
	for range 64 {
		silent = append(silent, dialTunnel(t, tun, nil))
	}
	for i, c := range silent {
		kept := i >= len(silent)-maxPending
		wait := time.Second
		if kept {
			wait = 20 * time.Millisecond
		}
		c.SetReadDeadline(time.Now().Add(wait))
		_, err := c.Read(make([]byte, 1))
		if gone := errors.Is(err, io.EOF); gone == kept {
			t.Fatalf("silent connection %d of %d: %v; want only the last %d left open", i+1, len(silent), err, maxPending)
		}
	}
	check := framedCheck(t, "agentufrag:brws", "agentpasswordagentpasswo")
	dialTunnel(t, tun, check)
	spliced(t, accepted, check)
}

// TestTunnelTrustsALookupThatKnowsThisUser: a kernel without the TCP
// sock_diag handler answers ENOENT for every socket, the same error as for
// a socket it does not have, and the tunnel would close the browser's
// connection as a stranger's. The lookup is asked first about a connection
// the CLI makes itself; one that does not call it this user's is taken
// as unavailable, so the browser waits for its check as it would where
// the kernel cannot be asked at all.
func TestTunnelTrustsALookupThatKnowsThisUser(t *testing.T) {
	for _, tc := range []struct {
		name    string
		self    bool
		err     error
		trusted bool
	}{
		{"this user's", true, nil, true},
		{"no handler for TCP", false, errors.New("the other end is not a socket of this host"), false},
		{"another user's", false, nil, false},
		{"the kernel cannot be asked", false, fmt.Errorf("%w: sock_diag: permission denied", tcpowner.ErrUnavailable), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln := loopback(t)
			t.Cleanup(func() { ln.Close() })
			var asked atomic.Int32
			lookup := trustedLookup(ln, func(net.Conn) (bool, error) {
				asked.Add(1)
				return tc.self, tc.err
			}, slog.New(slog.DiscardHandler))
			if asked.Load() != 1 {
				t.Fatalf("the lookup was asked %d times while it was tried, want once", asked.Load())
			}
			_, err := lookup(nil)
			if unavailable := errors.Is(err, tcpowner.ErrUnavailable); unavailable == tc.trusted {
				t.Fatalf("lookup after the probe: %v; trusted %v", err, tc.trusted)
			}
			fwd, accepted := testForward(t)
			tun := &tunnel{ln: ln, log: slog.New(slog.DiscardHandler), firstCheck: 5 * time.Second, fwd: fwd, selfOf: lookup}
			go tun.serve()
			tun.answered(iceAnswer("agentufrag", "agentpasswordagentpasswo"))
			c := dialTunnel(t, tun, nil)
			if !tc.self && tc.trusted {
				refused(t, tc.name, c)
				return
			}
			check := framedCheck(t, "agentufrag:brws", "agentpasswordagentpasswo")
			c.Write(check)
			spliced(t, accepted, check)
		})
	}
}

// TestTunnelRefusesWithoutAWorkingForward: a check that passes is dropped
// like any other while there is no forward to take it: none yet, after a
// first forward that failed under Auto, whose listener the browser still
// knows, or one whose socket is gone, as between two SSH connections.
func TestTunnelRefusesWithoutAWorkingForward(t *testing.T) {
	for _, tc := range []struct {
		name string
		fwd  dialer
	}{
		{"no forward", nil},
		{"forward socket gone", fakeForward{filepath.Join(t.TempDir(), "gone.sock")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tun := &tunnel{ln: loopback(t), log: slog.New(slog.DiscardHandler), firstCheck: time.Second, fwd: tc.fwd, selfOf: tcpowner.PeerIsSelf}
			go tun.serve()
			defer tun.Close()
			tun.answered(iceAnswer("agentufrag", "agentpasswordagentpasswo"))
			refused(t, "a check with no forward to take it", dialTunnel(t, tun, framedCheck(t, "agentufrag:brws", "agentpasswordagentpasswo")))
		})
	}
}

// TestOpenTunnel: Direct sets up no tunnel, and a forward that fails is
// fatal only when the tunnel is the only transport allowed.
func TestOpenTunnel(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	tripwire := filepath.Join(dir, "ssh")
	if err := os.WriteFile(tripwire, []byte("#!/bin/sh\ntouch "+ran+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := sshmux.Dial(t.Context(), "host", sshmux.Options{SSHPath: tripwire, NoMux: true, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	tun, err := openTunnel(t.Context(), m, "127.0.0.1:4000", relay.Direct, log)
	if err != nil || tun.Port() != 0 || tun.Close() != nil || tun.forward(t.Context(), m, "127.0.0.1:4000") != nil {
		t.Fatalf("Direct: %v, port %d; want no tunnel", err, tun.Port())
	}

	// A malformed target fails the forward before any ssh runs, and so does
	// one ssh would expand from this machine's environment.
	for _, remote := range []string{"a:b:c", "/tmp/${HOME}"} {
		_, err := openTunnel(t.Context(), m, remote, relay.Tunnel, log)
		if err == nil || !strings.Contains(err.Error(), "forward ICE-TCP over SSH") || !strings.Contains(err.Error(), "malformed forward target") {
			t.Fatalf("Tunnel to %q: %v, want the target refused", remote, err)
		}
	}
	if _, err := os.Stat(ran); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ssh ran for a refused target: %v", err)
	}
	tun, err = openTunnel(t.Context(), m, "a:b:c", relay.Auto, log)
	if err != nil || tun.Port() == 0 {
		t.Fatalf("Auto with a forward that fails: %v, port %d; want the listener, direct UDP beside it", err, tun.Port())
	}
	if err := tun.Close(); err != nil {
		t.Fatal(err)
	}
}
