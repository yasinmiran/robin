//go:build !linux && !windows

package tcpowner

import "net"

// PeerUID is ErrUnavailable: only Linux tells who opened a socket.
func PeerUID(net.Conn) (uint32, error) {
	return 0, ErrUnavailable
}

// PeerIsSelf is ErrUnavailable: only Linux and Windows tell who opened a
// socket.
func PeerIsSelf(net.Conn) (bool, error) {
	return false, ErrUnavailable
}
