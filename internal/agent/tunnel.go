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
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"golang.org/x/sys/unix"
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
	return ice.NewTCPMuxDefault(tunnelMuxParams(newOwnConns(ln, log, connUID, guest))), ln.Addr().String(), nil
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

// connUID is the uid of the process that opened the other end of c, a TCP
// connection over IPv4 loopback, as the kernel has it. The other end is a
// socket of this host whose local address is c's remote one. It counts
// only while that socket is established and a process still holds it:
// once its owner closes it, the kernel reports it as root's. sshd connects
// to an IPv4 address as an IPv4 socket; a connection from an IPv6 socket
// is not found and so not taken.
func connUID(c net.Conn) (uint32, error) {
	local, ok1 := c.LocalAddr().(*net.TCPAddr)
	remote, ok2 := c.RemoteAddr().(*net.TCPAddr)
	if !ok1 || !ok2 || local.IP.To4() == nil || remote.IP.To4() == nil {
		return 0, errors.New("not a TCP connection over IPv4")
	}
	s, err := lookupTCP4(remote, local)
	if err != nil {
		return 0, err
	}
	if s.state != tcpEstablished || s.inode == 0 {
		return 0, fmt.Errorf("the other end is no longer held open (state %d)", s.state)
	}
	return s.uid, nil
}

// tcpSocket is what the kernel reports of one TCP socket.
type tcpSocket struct {
	state      uint8
	uid, inode uint32
}

// From the kernel's linux/tcp_states.h and linux/inet_diag.h.
const (
	tcpEstablished      = 1
	sizeofInetDiagReqV2 = 56
	sizeofInetDiagMsg   = 72
)

// lookupTCP4 asks the kernel for the TCP socket from local to remote, with
// one exact NETLINK_SOCK_DIAG request: it needs no privilege, and costs
// the same however many sockets the host holds.
func lookupTCP4(local, remote *net.TCPAddr) (tcpSocket, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return tcpSocket{}, fmt.Errorf("sock_diag: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Sendto(fd, diagRequest(local, remote), 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return tcpSocket{}, fmt.Errorf("sock_diag: %w", err)
	}
	buf := make([]byte, 4096)
	n, _, err := unix.Recvfrom(fd, buf, 0)
	if err != nil {
		return tcpSocket{}, fmt.Errorf("sock_diag: %w", err)
	}
	return parseDiagReply(buf[:n], local, remote)
}

// diagRequest is a struct nlmsghdr and a struct inet_diag_req_v2 asking
// for one TCP socket over IPv4. The socket's ports and addresses go in
// network byte order, the rest in the host's.
func diagRequest(local, remote *net.TCPAddr) []byte {
	b := make([]byte, unix.NLMSG_HDRLEN+sizeofInetDiagReqV2)
	ne := binary.NativeEndian
	ne.PutUint32(b[0:], uint32(len(b)))
	ne.PutUint16(b[4:], unix.SOCK_DIAG_BY_FAMILY)
	ne.PutUint16(b[6:], unix.NLM_F_REQUEST)
	req := b[unix.NLMSG_HDRLEN:]
	req[0], req[1] = unix.AF_INET, unix.IPPROTO_TCP
	ne.PutUint32(req[4:], 1<<tcpEstablished)
	putSockID(req[8:], local, remote)
	// INET_DIAG_NOCOOKIE: any socket at this address, not one already seen.
	ne.PutUint32(req[48:], ^uint32(0))
	ne.PutUint32(req[52:], ^uint32(0))
	return b
}

// putSockID writes the ports and addresses of a struct inet_diag_sockid.
func putSockID(id []byte, local, remote *net.TCPAddr) {
	binary.BigEndian.PutUint16(id[0:], uint16(local.Port))
	binary.BigEndian.PutUint16(id[2:], uint16(remote.Port))
	copy(id[4:8], local.IP.To4())
	copy(id[20:24], remote.IP.To4())
}

// parseDiagReply reads the kernel's answer to diagRequest: one struct
// inet_diag_msg, which must be about the socket asked for, or an error.
func parseDiagReply(b []byte, local, remote *net.TCPAddr) (tcpSocket, error) {
	ne := binary.NativeEndian
	if len(b) < unix.NLMSG_HDRLEN {
		return tcpSocket{}, errors.New("sock_diag: short reply")
	}
	switch ne.Uint16(b[4:]) {
	case unix.NLMSG_ERROR:
		if len(b) < unix.NLMSG_HDRLEN+4 {
			return tcpSocket{}, errors.New("sock_diag: short error")
		}
		errno := unix.Errno(-int32(ne.Uint32(b[unix.NLMSG_HDRLEN:])))
		if errno == unix.ENOENT {
			return tcpSocket{}, errors.New("the other end is not a socket of this host")
		}
		return tcpSocket{}, fmt.Errorf("sock_diag: %w", errno)
	case unix.SOCK_DIAG_BY_FAMILY:
	default:
		return tcpSocket{}, fmt.Errorf("sock_diag: reply of type %d", ne.Uint16(b[4:]))
	}
	msg := b[unix.NLMSG_HDRLEN:]
	if len(msg) < sizeofInetDiagMsg {
		return tcpSocket{}, errors.New("sock_diag: short reply")
	}
	want := make([]byte, 40)
	putSockID(want, local, remote)
	if msg[0] != unix.AF_INET || string(msg[4:8]) != string(want[0:4]) ||
		string(msg[8:12]) != string(want[4:8]) || string(msg[24:28]) != string(want[20:24]) {
		return tcpSocket{}, errors.New("sock_diag: the reply is about another socket")
	}
	return tcpSocket{state: msg[1], uid: ne.Uint32(msg[64:]), inode: ne.Uint32(msg[68:])}, nil
}
