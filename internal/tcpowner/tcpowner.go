// Package tcpowner tells which user opened the other end of a loopback TCP
// connection. TCP carries no credentials, but the kernel records who made
// each socket, and Linux answers anyone who asks.
package tcpowner

import "errors"

// ErrUnavailable is the error PeerUID wraps where the kernel cannot be
// asked: off Linux, or where a sandbox blocks the netlink socket the
// question goes over, as Android does for apps.
var ErrUnavailable = errors.New("tcpowner: the kernel cannot be asked who opened a connection")
