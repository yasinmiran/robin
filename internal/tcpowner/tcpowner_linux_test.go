package tcpowner

import (
	"encoding/binary"
	"errors"
	"net"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// TestPeerUIDIsTheDialer looks a loopback connection's other end up in the
// kernel's socket table: this process dialled it, so it is this user's.
func TestPeerUIDIsTheDialer(t *testing.T) {
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
	uid, err := PeerUID(accepted)
	if err != nil || uid != uint32(os.Getuid()) {
		t.Errorf("PeerUID = %d, %v; want %d", uid, err, os.Getuid())
	}
	if _, err := PeerUID(&net.UnixConn{}); err == nil {
		t.Error("PeerUID of a unix connection succeeded")
	}
	gone := addrConn{
		local:  &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: ln.Addr().(*net.TCPAddr).Port},
		remote: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1},
	}
	// The kernel answered, so this is no ErrUnavailable: a caller that
	// lets through what it cannot ask about must not let this through.
	if _, err := PeerUID(gone); err == nil || errors.Is(err, ErrUnavailable) {
		t.Errorf("PeerUID of a connection that does not exist: %v, want the kernel's no", err)
	}
	v6 := addrConn{local: &net.TCPAddr{IP: net.IPv6loopback, Port: 1}, remote: &net.TCPAddr{IP: net.IPv6loopback, Port: 2}}
	if _, err := PeerUID(v6); err == nil {
		t.Error("PeerUID took a connection over IPv6")
	}
}

// addrConn is a connection that is nothing but its two addresses.
type addrConn struct {
	net.Conn
	local, remote net.Addr
}

func (c addrConn) LocalAddr() net.Addr  { return c.local }
func (c addrConn) RemoteAddr() net.Addr { return c.remote }

// TestParseDiagReplyTakesOnlyWhatItAskedFor feeds the reply parser what
// the kernel does not send for an exact lookup, and a socket other than
// the one asked for: each is an error, never a uid.
func TestParseDiagReplyTakesOnlyWhatItAskedFor(t *testing.T) {
	local := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000}
	remote := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 50000}
	reply := func(typ uint16, body []byte) []byte {
		b := make([]byte, unix.NLMSG_HDRLEN, unix.NLMSG_HDRLEN+len(body))
		binary.NativeEndian.PutUint32(b[0:], uint32(cap(b)))
		binary.NativeEndian.PutUint16(b[4:], typ)
		return append(b, body...)
	}
	errno := func(e unix.Errno) []byte { return binary.NativeEndian.AppendUint32(nil, uint32(-int32(e))) }
	msg := func(from, to *net.TCPAddr) []byte {
		m := make([]byte, sizeofInetDiagMsg)
		m[0], m[1] = unix.AF_INET, tcpEstablished
		putSockID(m[4:], from, to)
		binary.NativeEndian.PutUint32(m[64:], 1000)
		binary.NativeEndian.PutUint32(m[68:], 77)
		return m
	}
	got, err := parseDiagReply(reply(unix.SOCK_DIAG_BY_FAMILY, msg(local, remote)), local, remote)
	if err != nil || got != (tcpSocket{state: tcpEstablished, uid: 1000, inode: 77}) {
		t.Fatalf("the socket asked for: %+v, %v", got, err)
	}
	other := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 50000}
	for name, b := range map[string][]byte{
		"short":            {1, 2, 3},
		"short error":      reply(unix.NLMSG_ERROR, nil),
		"not found":        reply(unix.NLMSG_ERROR, errno(unix.ENOENT)),
		"other error":      reply(unix.NLMSG_ERROR, errno(unix.EPERM)),
		"another type":     reply(unix.NLMSG_DONE, nil),
		"short message":    reply(unix.SOCK_DIAG_BY_FAMILY, msg(local, remote)[:40]),
		"another socket":   reply(unix.SOCK_DIAG_BY_FAMILY, msg(local, other)),
		"another port":     reply(unix.SOCK_DIAG_BY_FAMILY, msg(remote, local)),
		"another listener": reply(unix.SOCK_DIAG_BY_FAMILY, msg(local, &net.TCPAddr{IP: net.IPv4zero})),
	} {
		if got, err := parseDiagReply(b, local, remote); err == nil {
			t.Errorf("%s: %+v, want an error", name, got)
		}
	}
}
