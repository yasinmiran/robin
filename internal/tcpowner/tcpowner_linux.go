package tcpowner

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// PeerUID is the uid of the process that opened the other end of c, a TCP
// connection over IPv4 loopback, as the kernel has it. The other end is a
// socket of this host whose local address is c's remote one. It counts
// only while that socket is established and a process still holds it:
// once its owner closes it, the kernel reports it as root's. A connection
// from an IPv6 socket is not found and so not answered for.
func PeerUID(c net.Conn) (uint32, error) {
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

// PeerIsSelf reports whether the other end of c, as PeerUID finds it, was
// opened by this process's user.
func PeerIsSelf(c net.Conn) (bool, error) {
	uid, err := PeerUID(c)
	if err != nil {
		return false, err
	}
	return uid == uint32(os.Getuid()), nil
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
		return tcpSocket{}, fmt.Errorf("%w: sock_diag: %w", ErrUnavailable, err)
	}
	defer unix.Close(fd)
	if err := unix.Sendto(fd, diagRequest(local, remote), 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return tcpSocket{}, fmt.Errorf("%w: sock_diag: %w", ErrUnavailable, err)
	}
	buf := make([]byte, 4096)
	n, _, err := unix.Recvfrom(fd, buf, 0)
	if err != nil {
		return tcpSocket{}, fmt.Errorf("%w: sock_diag: %w", ErrUnavailable, err)
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
