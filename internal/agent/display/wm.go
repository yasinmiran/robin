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

// waitForWindow returns once some top-level window on display num is
// viewable, or when ctx ends.
func waitForWindow(ctx context.Context, num string, cookie []byte) error {
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
