//go:build linux

package display

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"

	"github.com/yasinmiran/robin/internal/agent/peercred"
)

// waitForWindow returns once some top-level window on display num is
// viewable, if the Xvfb with pid xvfbPID serves it, or when ctx ends.
func waitForWindow(ctx context.Context, num string, cookie []byte, xvfbPID int) error {
	nc, err := dialServer(num, xvfbPID)
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
// or else the abstract one, if the Xvfb with pid xvfbPID listens there.
// Another user can put a socket at either name before Xvfb binds it or
// after, and whoever answers gets the display's cookie.
func dialServer(num string, xvfbPID int) (net.Conn, error) {
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
		cred, err := peercred.Of(nc)
		if err == nil && int(cred.Pid) == xvfbPID {
			return nc, nil
		}
		nc.Close()
		if err != nil {
			ownerErr = fmt.Errorf("display: %s: cannot tell who serves it: %w", addr, err)
		} else {
			ownerErr = &notServerError{addr: addr, pid: int(cred.Pid), xvfbPID: xvfbPID}
		}
	}
	if ownerErr != nil {
		return nil, ownerErr
	}
	return nil, dialErr
}

// checkSockets makes sure the Xvfb with pid xvfbPID holds both socket names
// of display num. libxcb dials the abstract name first, and a name Xvfb
// left free, as one built or started without the abstract transport does,
// any local user can take and so receive the cookie of every client in the
// session.
func checkSockets(num string, xvfbPID int) error {
	sock := "/tmp/.X11-unix/X" + num
	for _, addr := range []string{sock, "@" + sock} {
		nc, err := net.Dial("unix", addr)
		if err != nil {
			return fmt.Errorf("display: Xvfb does not listen on %s, which any local user could take to receive the session's cookie: %w", addr, err)
		}
		cred, err := peercred.Of(nc)
		nc.Close()
		if err != nil {
			return fmt.Errorf("display: %s: cannot tell who serves it: %w", addr, err)
		}
		if int(cred.Pid) != xvfbPID {
			return &notServerError{addr: addr, pid: int(cred.Pid), xvfbPID: xvfbPID}
		}
	}
	return nil
}

// notServerError means a process other than the spawned Xvfb listens at
// one of its display's socket names.
type notServerError struct {
	addr         string
	pid, xvfbPID int
}

func (e *notServerError) Error() string {
	return fmt.Sprintf("display: %s is served by pid %d, not by Xvfb (pid %d)", e.addr, e.pid, e.xvfbPID)
}
