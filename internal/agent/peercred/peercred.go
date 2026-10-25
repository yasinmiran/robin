//go:build linux

// Package peercred reads who is at the other end of a unix connection, as
// the kernel recorded it: the agent's own sockets take only this user's
// processes, and the X sockets it dials only a server it trusts.
package peercred

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// Of returns the credentials of the process at the other end of c, a unix
// connection: the one that connected, for an accepted connection, or the
// one that listens, for a dialled one.
func Of(c net.Conn) (*unix.Ucred, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return nil, fmt.Errorf("peercred: not a unix connection: %T", c)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("peercred: %w", err)
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return nil, fmt.Errorf("peercred: %w", err)
	}
	if credErr != nil {
		return nil, fmt.Errorf("peercred: %w", credErr)
	}
	return cred, nil
}
