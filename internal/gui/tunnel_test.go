package gui

import (
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pion/stun/v4"
)

// fakeForward is the SSH forward's local end, a unix socket whose listener
// the test holds.
type fakeForward struct{ sock string }

func (f fakeForward) Dial() (net.Conn, error) { return net.Dial("unix", f.sock) }

// testTunnel serves a tunnel on loopback whose forward leads to a socket of
// the test's; what reaches that socket comes out of accepted.
func testTunnel(t *testing.T, ln net.Listener) (*tunnel, <-chan net.Conn) {
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
	tun := &tunnel{ln: ln, log: slog.New(slog.DiscardHandler), firstCheck: 300 * time.Millisecond, fwd: fakeForward{sock}}
	go tun.serve()
	t.Cleanup(func() {
		ln.Close()
		u.Close()
	})
	return tun, accepted
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
	tun, accepted := testTunnel(t, loopback(t))
	const ufrag, pwd = "agentufrag", "agentpasswordagentpasswo"

	refused(t, "a check before any answer", dialTunnel(t, tun, framedCheck(t, ufrag+":brws", pwd)))
	tun.answered(iceAnswer(ufrag, pwd))
	refused(t, "silence", dialTunnel(t, tun, nil))
	refused(t, "half a length", dialTunnel(t, tun, []byte{0}))
	refused(t, "another ufrag", dialTunnel(t, tun, framedCheck(t, "stranger:brws", pwd)))
	refused(t, "a guessed password", dialTunnel(t, tun, framedCheck(t, ufrag+":brws", "guessedpasswordguessedpa")))
	refused(t, "not STUN", dialTunnel(t, tun, framed([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))))
	refused(t, "a frame over the agent's limit", dialTunnel(t, tun, append([]byte{0xff, 0xff}, make([]byte, 600)...)))
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
	tun, accepted := testTunnel(t, &flakyListener{Listener: loopback(t)})
	tun.answered(iceAnswer("agentufrag", "agentpasswordagentpasswo"))
	check := framedCheck(t, "agentufrag:brws", "agentpasswordagentpasswo")
	dialTunnel(t, tun, check)
	spliced(t, accepted, check)
}
