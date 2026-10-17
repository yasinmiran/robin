//go:build linux

package display

import (
	"context"
	"encoding/hex"
	"net"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// waitForWM returns once a window manager announces itself, by owning WM_S0
// (ICCCM) or setting _NET_SUPPORTING_WM_CHECK on the root (EWMH), or when
// ctx ends. A terminal mapped before that moment can stay unmanaged and
// invisible.
func waitForWM(ctx context.Context, num string, cookie []byte) error {
	sock := "/tmp/.X11-unix/X" + num
	nc, err := net.Dial("unix", sock)
	if err != nil {
		var aerr error
		if nc, aerr = net.Dial("unix", "@"+sock); aerr != nil {
			return err
		}
	}
	x, err := xgb.NewConnNetWithCookieHex(nc, hex.EncodeToString(cookie))
	if err != nil {
		nc.Close()
		return err
	}
	defer x.Close()
	atom, err := xproto.InternAtom(x, false, 5, "WM_S0").Reply()
	if err != nil {
		return err
	}
	check, err := xproto.InternAtom(x, false, 24, "_NET_SUPPORTING_WM_CHECK").Reply()
	if err != nil {
		return err
	}
	root := xproto.Setup(x).DefaultScreen(x).Root
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		owner, err := xproto.GetSelectionOwner(x, atom.Atom).Reply()
		if err != nil {
			return err
		}
		if owner.Owner != xproto.WindowNone {
			return nil
		}
		prop, err := xproto.GetProperty(x, false, root, check.Atom, xproto.AtomWindow, 0, 1).Reply()
		if err == nil && prop.ValueLen > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
