//go:build linux

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"

	"github.com/yasinmiran/robin/internal/tcpowner"
)

// listenTunnel opens the ICE-TCP listener the CLI's SSH forward leads to,
// and the mux on it, and returns the mux and the listener's address. The
// listener is loopback-only, so the forward is the one way in from
// outside the host, and it hangs up on any connection whose other end is
// not a socket that a process of this user's, or root's, holds open: TCP
// carries no credentials, but the kernel knows who made each socket.
//
// A guest's forward runs as the guest, so a uid that guest admits counts
// too, for as long as a guest of that uid is connected.
func listenTunnel(log *slog.Logger, guest func(uint32) bool) (ice.TCPMux, string, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("listen for ICE-TCP: %w", err)
	}
	return ice.NewTCPMuxDefault(tunnelMuxParams(newOwnConns(ln, log, tcpowner.PeerUID, guest))), ln.Addr().String(), nil
}

// muxWait bounds what a connection that passed the owner check holds in
// the mux before ICE takes it: the wait for its first STUN request, and
// the life of a connection under a ufrag no agent gathered. Pion's 30 s
// would let a process of this user's, or a dialer's socket the kernel
// shows as root's, keep a goroutine and a buffer per ufrag it makes up.
// The CLI's forward sends its first request as it connects, and the
// agent gathers its ufrag before the browser hears of it.
const muxWait = 5 * time.Second

func tunnelMuxParams(ln net.Listener) ice.TCPMuxParams {
	return ice.TCPMuxParams{
		Listener: ln, ReadBufferSize: 64,
		FirstStunBindTimeout: muxWait, AliveDurationForConnFromStun: muxWait,
	}
}

// maxOwnerChecks bounds the connections ownConns checks at once. A check
// is one netlink round trip; a connection that comes while every checker
// is busy is closed.
const maxOwnerChecks = 8

// ownConns passes on the connections that this user, or root, opened, and
// closes the rest before anything reads from them. sshd's forward runs as
// the user who logged in; a server that forwards as root is trusted as
// root is. It accepts on a goroutine of its own and checks each connection
// on another, so a slow check holds up no other connection.
type ownConns struct {
	net.Listener
	log   *slog.Logger
	uidOf func(net.Conn) (uint32, error)
	// guest admits the uid of a guest connected now; nil admits none.
	guest func(uint32) bool

	checkers chan struct{} // a slot per check under way
	checked  chan net.Conn
	done     chan struct{} // closed once the listener fails
	err      error         // why it failed, once done is closed
	refused  atomic.Bool   // a refusal was logged at Warn; the rest go at Debug
}

func newOwnConns(ln net.Listener, log *slog.Logger, uidOf func(net.Conn) (uint32, error), guest func(uint32) bool) *ownConns {
	l := &ownConns{
		Listener: ln, log: log, uidOf: uidOf, guest: guest,
		checkers: make(chan struct{}, maxOwnerChecks), checked: make(chan net.Conn), done: make(chan struct{}),
	}
	go l.accept()
	return l
}

// Accept returns the next connection that passed the check.
func (l *ownConns) Accept() (net.Conn, error) {
	select {
	case c := <-l.checked:
		return c, nil
	case <-l.done:
		return nil, l.err
	}
}

func (l *ownConns) accept() {
	defer close(l.done)
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			l.err = err
			return
		}
		select {
		case l.checkers <- struct{}{}:
			go l.check(c)
		default:
			c.Close()
			l.refuse("ICE-TCP connection refused: too many being checked", "from", c.RemoteAddr())
		}
	}
}

func (l *ownConns) check(c net.Conn) {
	defer func() { <-l.checkers }()
	uid, err := l.uidOf(c)
	switch {
	case err != nil:
		c.Close()
		l.refuse("ICE-TCP connection refused: cannot tell whose it is", "from", c.RemoteAddr(), "err", err)
	case uid != uint32(os.Getuid()) && uid != 0 && (l.guest == nil || !l.guest(uid)):
		c.Close()
		l.refuse("ICE-TCP connection refused: another user's", "from", c.RemoteAddr(), "uid", uid)
	default:
		select {
		case l.checked <- c:
		case <-l.done:
			c.Close()
		}
	}
}

func (l *ownConns) refuse(msg string, args ...any) {
	level := slog.LevelDebug
	if l.refused.CompareAndSwap(false, true) {
		level = slog.LevelWarn
	}
	l.log.Log(context.Background(), level, msg, args...)
}
