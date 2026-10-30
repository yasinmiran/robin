package gui

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/yasinmiran/robin/internal/relay"
	"github.com/yasinmiran/robin/internal/sshmux"
	"github.com/yasinmiran/robin/internal/tcpowner"
)

// firstCheckWait is how long a connection to the tunnel has to send its
// first connectivity check before it is dropped. The browser sends one as
// soon as its ICE-TCP connection is up, a round trip on loopback; the wait
// bounds how long a connection that says nothing holds a descriptor.
const firstCheckWait = 5 * time.Second

// maxCheckBytes bounds the first message read from a tunnel connection. The
// agent's ICE-TCP listener reads its first message into 512 bytes and drops
// a connection whose first message is longer, so a longer one could never
// be a check that works; a browser's is about 150.
const maxCheckBytes = 512

// maxPending bounds the tunnel connections still to send their first
// check, each holding a descriptor for up to firstCheckWait. Past it the
// oldest is closed. The browser sends its check a loopback round trip
// after it connects, so the oldest is not the browser's unless someone
// keeps opening connections faster than that, which only another user's
// can do unchecked where the kernel cannot say whose they are. A spliced
// connection no longer counts.
const maxPending = 16

// maxSplices bounds the tunnel connections spliced at once. Each is an SSH
// channel, and on Windows an ssh of its own that logs in, so a check
// captured off the network and sent again on many connections must not
// start a login for each. The browser keeps one connection per pair of
// candidates, and a few across an ICE restart.
const maxSplices = 8

// maxSeenChecks bounds the transaction IDs the tunnel remembers for the
// agent's current credentials, the oldest forgotten first. A browser sends
// each check once over TCP, so an ID that comes again is a copy.
const maxSeenChecks = 4096

// A listener out of file descriptors is tried again after acceptRetryFirst,
// doubling up to acceptRetryMax, so a moment without descriptors costs one
// connection, not the tunnel.
const (
	acceptRetryFirst = 5 * time.Millisecond
	acceptRetryMax   = time.Second
)

// dialer opens a connection through the SSH forward: a *sshmux.Forward.
type dialer interface{ Dial() (net.Conn, error) }

// tunnel is the client end of the ICE-TCP fallback: a loopback TCP listener
// whose connections are spliced into an SSH forward to the agent's listener.
// The listener lives as long as the session; the forward is remade on each
// SSH connection, so the port the browser was told never changes.
//
// Any user of this machine can connect to a loopback port, so a connection
// is spliced only once its first message is a check the agent's ICE
// credentials sign, which only the page learns: anything else would get an
// SSH channel and a connection to the agent's listener for the asking.
// Where the kernel says who opened a connection, one that is not this
// user's is closed before it waits for that check. A check passes once
// for the credentials it was signed with, and at most maxSplices
// connections are spliced at once.
type tunnel struct {
	ln         net.Listener
	log        *slog.Logger
	firstCheck time.Duration
	selfOf     func(net.Conn) (bool, error) // tcpowner.PeerIsSelf

	mu       sync.Mutex
	fwd      dialer            // nil while there is none
	creds    relay.Credentials // of the agent's last answer
	pending  []net.Conn        // still to send their check, oldest first
	splices  int               // spliced now
	seen     map[[12]byte]bool // transaction IDs of checks passed with creds
	seenFIFO [][12]byte        // the same, oldest first
}

// openTunnel sets the tunnel up for transport: none for Direct, and for
// the others the listener plus the forward over m, where a forward that
// fails is an error only when the tunnel is all the user allowed.
func openTunnel(ctx context.Context, m *sshmux.Master, remote string, transport relay.Transport, log *slog.Logger) (*tunnel, error) {
	t := &tunnel{log: log, firstCheck: firstCheckWait}
	if remote == "" || transport == relay.Direct {
		return t, nil
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	t.ln = ln
	t.selfOf = trustedLookup(ln, tcpowner.PeerIsSelf, log)
	go t.serve()
	if err := t.forward(ctx, m, remote); err != nil {
		if transport == relay.Tunnel {
			ln.Close()
			return nil, err
		}
		log.Warn("SSH tunnel unavailable; direct UDP only", "err", err)
	}
	return t, nil
}

// trustedLookup is lookup, once it has said that a connection this process
// makes to ln is this user's. A kernel that cannot place sockets at all,
// such as one built without the TCP sock_diag handler that answers ENOENT
// for every socket, would otherwise have the tunnel close the browser's
// connection too; there the lookup is taken as unavailable, and the cap
// on pending connections is all there is, as where it cannot be asked.
// It must run before anything else accepts on ln.
func trustedLookup(ln net.Listener, lookup func(net.Conn) (bool, error), log *slog.Logger) func(net.Conn) (bool, error) {
	unavailable := func(net.Conn) (bool, error) { return false, tcpowner.ErrUnavailable }
	self, err := probeLookup(ln, lookup)
	if errors.Is(err, tcpowner.ErrUnavailable) {
		return unavailable
	}
	if err != nil || !self {
		log.Warn("tunnel cannot tell this user's connections from others'; the cap on waiting ones is all that guards it", "self", self, "err", err)
		return unavailable
	}
	return lookup
}

// probeLookup dials ln and asks lookup about the connection it accepts
// from that dial. Another one that comes first is closed.
func probeLookup(ln net.Listener, lookup func(net.Conn) (bool, error)) (bool, error) {
	if tl, ok := ln.(*net.TCPListener); ok {
		tl.SetDeadline(time.Now().Add(firstCheckWait))
		defer tl.SetDeadline(time.Time{})
	}
	mine, err := net.DialTimeout("tcp4", ln.Addr().String(), firstCheckWait)
	if err != nil {
		return false, err
	}
	defer mine.Close()
	for range maxPending {
		c, err := ln.Accept()
		if err != nil {
			return false, err
		}
		if c.RemoteAddr().String() != mine.LocalAddr().String() {
			c.Close()
			continue
		}
		defer c.Close()
		return lookup(c)
	}
	return false, errors.New("the probe's own connection never came")
}

// serve splices the listener's connections until it is closed.
func (t *tunnel) serve() {
	retry := acceptRetryFirst
	for {
		c, err := t.ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			t.log.Warn("tunnel accept", "err", err)
			time.Sleep(retry)
			retry = min(retry*2, acceptRetryMax)
			continue
		}
		retry = acceptRetryFirst
		if t.stranger(c) {
			c.Close()
			continue
		}
		t.waitForCheck(c)
		go t.splice(c)
	}
}

// stranger reports whether c is another user's, or comes from a socket
// that no process holds open any more, which the kernel says of every
// connection where it can be asked: on Linux, outside a sandbox that
// blocks netlink. Elsewhere, Android among them, nobody is, and the cap on
// pending connections is all there is.
func (t *tunnel) stranger(c net.Conn) bool {
	self, err := t.selfOf(c)
	if errors.Is(err, tcpowner.ErrUnavailable) {
		return false
	}
	if err != nil || !self {
		t.log.Debug("tunnel refused another user's connection", "from", c.RemoteAddr(), "err", err)
		return true
	}
	return false
}

// waitForCheck counts c among the connections still to send their check,
// closing the oldest of them to make room.
func (t *tunnel) waitForCheck(c net.Conn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.pending) == maxPending {
		t.pending[0].Close()
		t.pending = slices.Delete(t.pending, 0, 1)
	}
	t.pending = append(t.pending, c)
}

// answered takes the ICE credentials of an agent's answer, which the bridge
// hands over before the page sees it; checks signed with any earlier ones
// no longer get through, as the agent no longer takes them either.
func (t *tunnel) answered(sdp string) {
	creds, err := relay.AnswerCredentials(sdp)
	if err != nil {
		t.log.Warn("tunnel keeps the last credentials", "err", err)
		return
	}
	t.mu.Lock()
	t.creds = creds
	t.seen, t.seenFIFO = nil, nil
	t.mu.Unlock()
}

// forward points the tunnel at remote through m.
func (t *tunnel) forward(ctx context.Context, m *sshmux.Master, remote string) error {
	if t.ln == nil {
		return nil
	}
	fwd, err := m.Forward(ctx, remote)
	if err != nil {
		return fmt.Errorf("forward ICE-TCP over SSH: %w", err)
	}
	t.mu.Lock()
	t.fwd = fwd
	t.mu.Unlock()
	return nil
}

// Port is the loopback port the browser's tunnel candidate names, or 0
// without a tunnel.
func (t *tunnel) Port() int {
	if t.ln == nil {
		return 0
	}
	return t.ln.Addr().(*net.TCPAddr).Port
}

func (t *tunnel) Close() error {
	if t.ln == nil {
		return nil
	}
	return t.ln.Close()
}

// splice joins c to the forward once its first message is a check for the
// agent, and passes that message on first.
func (t *tunnel) splice(c net.Conn) {
	defer c.Close()
	first, err := t.firstMessage(c)
	t.mu.Lock()
	t.pending = slices.DeleteFunc(t.pending, func(p net.Conn) bool { return p == c })
	fwd, creds := t.fwd, t.creds
	t.mu.Unlock()
	if err != nil || fwd == nil || !creds.Admits(first[2:]) {
		t.log.Debug("tunnel refused a connection", "from", c.RemoteAddr(), "err", err)
		return
	}
	if why := t.admit(creds, first[2:]); why != "" {
		t.log.Debug("tunnel refused a connection", "from", c.RemoteAddr(), "why", why)
		return
	}
	defer t.release()
	u, err := fwd.Dial()
	if err != nil {
		t.log.Warn("tunnel dial", "err", err)
		return
	}
	defer u.Close()
	if _, err := u.Write(first); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(u, c); done <- struct{}{} }()
	go func() { io.Copy(c, u); done <- struct{}{} }()
	<-done
}

// admit takes a splice for check, a STUN message that creds admit, or says
// why there is none: its transaction ID passed before, or maxSplices are
// under way. A check of credentials replaced meanwhile is not counted.
func (t *tunnel) admit(creds relay.Credentials, check []byte) string {
	var id [12]byte
	copy(id[:], check[8:20])
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.creds != creds {
		return "credentials replaced"
	}
	if t.seen[id] {
		return "a check seen before"
	}
	if t.splices >= maxSplices {
		return "too many spliced at once"
	}
	if t.seen == nil {
		t.seen = make(map[[12]byte]bool)
	}
	if len(t.seenFIFO) == maxSeenChecks {
		delete(t.seen, t.seenFIFO[0])
		t.seenFIFO = t.seenFIFO[1:]
	}
	t.seen[id] = true
	t.seenFIFO = append(t.seenFIFO, id)
	t.splices++
	return ""
}

// release gives back the splice admit took.
func (t *tunnel) release() {
	t.mu.Lock()
	t.splices--
	t.mu.Unlock()
}

// firstMessage reads c's first RFC 4571 frame, length included, within
// firstCheck.
func (t *tunnel) firstMessage(c net.Conn) ([]byte, error) {
	c.SetReadDeadline(time.Now().Add(t.firstCheck))
	defer c.SetReadDeadline(time.Time{})
	var length [2]byte
	if _, err := io.ReadFull(c, length[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(length[:]))
	if n > maxCheckBytes {
		return nil, fmt.Errorf("first message of %d bytes", n)
	}
	frame := make([]byte, 2+n)
	copy(frame, length[:])
	if _, err := io.ReadFull(c, frame[2:]); err != nil {
		return nil, err
	}
	return frame, nil
}
