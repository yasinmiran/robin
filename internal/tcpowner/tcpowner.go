// Package tcpowner tells which user opened the other end of a loopback TCP
// connection. TCP carries no credentials, but the kernel records who made
// each socket: Linux answers anyone who asks with the socket's uid, and
// Windows names the process that holds it, whose account a process of the
// same account can read.
package tcpowner

import "errors"

// ErrUnavailable is the error PeerUID and PeerIsSelf wrap where the kernel
// cannot be asked: PeerUID off Linux, PeerIsSelf off Linux and Windows, or
// where a sandbox blocks the netlink socket the question goes over, as
// Android does for apps.
var ErrUnavailable = errors.New("tcpowner: the kernel cannot be asked who opened a connection")
