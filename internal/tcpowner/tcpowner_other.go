//go:build !linux

package tcpowner

import "net"

// PeerUID is ErrUnavailable: only Linux tells who opened a socket.
func PeerUID(net.Conn) (uint32, error) {
	return 0, ErrUnavailable
}
