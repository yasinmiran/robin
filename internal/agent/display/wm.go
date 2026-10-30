//go:build linux

package display

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"golang.org/x/sys/unix"

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
		if err == nil {
			if err = servedBy(addr, cred, xvfbPID); err == nil {
				return nc, nil
			}
			ownerErr = err
		} else {
			ownerErr = fmt.Errorf("display: %s: cannot tell who serves it: %w", addr, err)
		}
		nc.Close()
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
		if err := servedBy(addr, cred, xvfbPID); err != nil {
			return err
		}
	}
	return nil
}

// servedBy says whether cred, the peer credentials of a listener at addr,
// are those of the spawned Xvfb with pid xvfbPID. SO_PEERCRED gives the
// pid that called listen, not who holds the socket now: another user could
// listen from a process that hands the socket on and exits, and Xvfb could
// be given that pid later. Xvfb runs as this user, so the uid must match
// too, and a socket of another user's never passes whatever its pid.
func servedBy(addr string, cred *unix.Ucred, xvfbPID int) error {
	if int(cred.Pid) != xvfbPID || cred.Uid != uint32(os.Getuid()) {
		return &notServerError{addr: addr, pid: int(cred.Pid), uid: cred.Uid, xvfbPID: xvfbPID}
	}
	return nil
}

// notServerError means a process other than the spawned Xvfb listens at
// one of its display's socket names.
type notServerError struct {
	addr         string
	pid, xvfbPID int
	uid          uint32
}

func (e *notServerError) Error() string {
	return fmt.Sprintf("display: %s is served by pid %d of uid %d, not by Xvfb (pid %d)", e.addr, e.pid, e.uid, e.xvfbPID)
}
