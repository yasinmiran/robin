//go:build linux

package x11

import (
	"errors"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// trustedServerUIDs are the users whose X servers the agent talks to: this
// user, and root, which runs Xorg for display managers. A var so a test can
// make its own listener a stranger's.
var trustedServerUIDs = []uint32{uint32(os.Getuid()), 0}

// serverUID returns the uid of the process listening at the other end of nc.
func serverUID(nc net.Conn) (uint32, error) {
	uc, ok := nc.(*net.UnixConn)
	if !ok {
		return 0, errors.New("not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	return cred.Uid, nil
}
