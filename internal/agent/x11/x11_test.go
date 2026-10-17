//go:build linux

package x11

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/xproto"

	"github.com/yasinmiran/remotejump/internal/agent/display"
)

const maxW, maxH = 1920, 1080

// testX is a spawned display, our Conn to it, and a second independent
// connection that observes what the Conn does.
type testX struct {
	d      *display.Display
	c      *Conn
	peer   *xgb.Conn
	root   xproto.Window
	events chan xgb.Event // the peer's events
}

func startX(t testing.TB, session string) *testX {
	t.Helper()
	return startXWith(t, display.Options{Session: session})
}

func startXWith(t testing.TB, o display.Options) *testX {
	t.Helper()
	if _, err := exec.LookPath("Xvfb"); err != nil {
		t.Skip("Xvfb not on PATH; run internal/agent/x11/testdata/xtest.sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	o.RunDir, o.MaxWidth, o.MaxHeight = t.TempDir(), maxW, maxH
	d, err := display.Spawn(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	c, err := Open(d.Name, d.XAuthority)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	peer, err := dial(d.Name, d.XAuthority)
	if err != nil {
		t.Fatal(err)
	}
	tx := &testX{d: d, c: c, peer: peer, root: xproto.Setup(peer).DefaultScreen(peer).Root,
		events: make(chan xgb.Event, 1000)}
	// Pump the peer's events until it closes. Waiting for that in cleanup
	// orders its reader before the next test's extension setup, which
	// writes xgb's global event tables.
	pumped := make(chan struct{})
	go func() {
		defer close(pumped)
		for {
			ev, xerr := peer.WaitForEvent()
			if ev == nil && xerr == nil {
				return
			}
			if ev != nil {
				select {
				case tx.events <- ev:
				default:
				}
			}
		}
	}()
	t.Cleanup(func() { peer.Close(); <-pumped })
	if err := randr.Init(peer); err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestOpen(t *testing.T) {
	tx := startX(t, "sleep 1000")
	if w, h := tx.c.Size(); w != maxW || h != maxH {
		t.Fatalf("Size = %dx%d, want %dx%d", w, h, maxW, maxH)
	}
	if _, err := Open(tx.d.Name, "/dev/null"); err == nil {
		t.Error("Open with an empty Xauthority succeeded")
	}
}
