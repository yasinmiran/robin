//go:build linux

package display

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"syscall"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// waitForWindow returns once some top-level window on display num, served
// by process server, is viewable, or when ctx ends.
func waitForWindow(ctx context.Context, num string, cookie []byte, server int) error {
	nc, err := dialServer(num, server)
	if err != nil {
		return err
	}
	x, err := xgb.NewConnNetWithCookieHex(nc, hex.EncodeToString(cookie))
	if err != nil {
		nc.Close()
		return err
	}
	defer x.Close()
	root := xproto.Setup(x).DefaultScreen(x).Root
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		tree, err := xproto.QueryTree(x, root).Reply()
		if err != nil {
			return err
		}
		for _, w := range tree.Children {
			a, err := xproto.GetWindowAttributes(x, w).Reply()
			if err == nil && a.MapState == xproto.MapStateViewable && !a.OverrideRedirect {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// dialServer connects to the unix socket of display num, the filesystem one
// or else the abstract one, if process server listens there. Another user
// can put a socket at either name before Xvfb binds it or after, and
// whoever answers gets the display's cookie.
func dialServer(num string, server int) (net.Conn, error) {
	sock := "/tmp/.X11-unix/X" + num
	var dialErr, ownerErr error
	for _, addr := range []string{sock, "@" + sock} {
		nc, err := net.Dial("unix", addr)
		if err != nil {
			if dialErr == nil {
				dialErr = err
			}
			continue
		}
		pid, err := peerPID(nc)
		if err == nil && pid == server {
			return nc, nil
		}
		nc.Close()
		if err != nil {
			ownerErr = fmt.Errorf("display: %s: cannot tell who serves it: %w", addr, err)
		} else {
			ownerErr = fmt.Errorf("display: %s is served by pid %d, not by Xvfb (pid %d)", addr, pid, server)
		}
	}
	if ownerErr != nil {
		return nil, ownerErr
	}
	return nil, dialErr
}

// peerPID is the pid of the process that listens at the other end of nc.
func peerPID(nc net.Conn) (int, error) {
	uc, ok := nc.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("not a unix socket: %T", nc)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	return int(cred.Pid), nil
}
