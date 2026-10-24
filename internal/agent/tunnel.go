//go:build linux

package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/pion/ice/v4"
)

// listenTunnel opens the ICE-TCP listener the CLI's SSH forward leads to,
// and the mux on it, and returns the mux and the listener's address. The
// listener is loopback-only, so the forward is the one way in from
// outside the host, and it hangs up on any connection another user's
// process opened: TCP carries no credentials, but the kernel knows who
// made each socket.
func listenTunnel(log *slog.Logger) (ice.TCPMux, string, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("listen for ICE-TCP: %w", err)
	}
	own := &ownConns{Listener: ln, log: log, uidOf: connUID}
	return ice.NewTCPMuxDefault(ice.TCPMuxParams{Listener: own, ReadBufferSize: 64}), ln.Addr().String(), nil
}

// ownConns passes on the connections that this user, or root, opened, and
// closes the rest before anything reads from them. sshd's forward runs as
// the user who logged in; a server that forwards as root is trusted as
// root is.
type ownConns struct {
	net.Listener
	log     *slog.Logger
	uidOf   func(net.Conn) (uint32, error)
	refused atomic.Bool // a refusal was logged at Warn; the rest go at Debug
}

func (l *ownConns) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uid, err := l.uidOf(c)
		if err == nil && (uid == uint32(os.Getuid()) || uid == 0) {
			return c, nil
		}
		c.Close()
		level := slog.LevelDebug
		if l.refused.CompareAndSwap(false, true) {
			level = slog.LevelWarn
		}
		if err != nil {
			l.log.Log(context.Background(), level, "ICE-TCP connection refused: cannot tell whose it is", "from", c.RemoteAddr(), "err", err)
		} else {
			l.log.Log(context.Background(), level, "ICE-TCP connection refused: another user's", "from", c.RemoteAddr(), "uid", uid)
		}
	}
}

// connUID is the uid of the process that opened the other end of c, a TCP
// connection over IPv4 loopback, from the kernel's table of sockets. The
// other end is a socket of this host whose local address is c's remote
// one. sshd connects to an IPv4 address as an IPv4 socket; a connection
// from an IPv6 socket is not in the table and so not taken.
func connUID(c net.Conn) (uint32, error) {
	local, ok1 := c.LocalAddr().(*net.TCPAddr)
	remote, ok2 := c.RemoteAddr().(*net.TCPAddr)
	if !ok1 || !ok2 || local.IP.To4() == nil || remote.IP.To4() == nil {
		return 0, errors.New("not a TCP connection over IPv4")
	}
	b, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return 0, err
	}
	// Each row: sl, local_address, rem_address, st, tx:rx, tr:when,
	// retrnsmt, uid, ... The kernel prints an address as the word it
	// stores, in host byte order, and the port as a number.
	want, peer := procAddr(remote), procAddr(local)
	for _, line := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) > 7 && f[1] == want && f[2] == peer {
			uid, err := strconv.ParseUint(f[7], 10, 32)
			return uint32(uid), err
		}
	}
	return 0, errors.New("the other end is not in /proc/net/tcp")
}

func procAddr(a *net.TCPAddr) string {
	ip := a.IP.To4()
	return fmt.Sprintf("%08X:%04X", binary.NativeEndian.Uint32(ip), a.Port)
}
