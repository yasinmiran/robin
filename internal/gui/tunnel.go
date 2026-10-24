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
// check, each holding a descriptor for up to firstCheckWait, which any user
// of this machine can open. Past it the oldest is closed. The browser sends
// its check a loopback round trip after it connects, so the oldest is never
// the browser's in practice, and a spliced connection no longer counts.
const maxPending = 16

// Accept backoff, as net/http's: a listener out of file descriptors is
// tried again after 5 ms, doubling up to a second.
const (
	acceptRetryFirst = 5 * time.Millisecond
	acceptRetryMax   = time.Second
)

// tunnel is the client end of the ICE-TCP fallback: a loopback TCP listener
// whose connections are spliced into an SSH forward to the agent's listener.
// The listener lives as long as the session; the forward is remade on each
// SSH connection, so the port the browser was told never changes.
//
// Any user of this machine can connect to a loopback port, so a connection
// is spliced only once its first message is a check the agent's ICE
// credentials sign, which only the page learns: anything else would get an
// SSH channel and a connection to the agent's listener for the asking.
type tunnel struct {
	ln         net.Listener
	log        *slog.Logger
	firstCheck time.Duration

	mu      sync.Mutex
	fwd     interface{ Dial() (net.Conn, error) } // nil while there is none
	creds   relay.Credentials                     // of the agent's last answer
	pending []net.Conn                            // still to send their check, oldest first
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
		t.waitForCheck(c)
		go t.splice(c)
	}
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
