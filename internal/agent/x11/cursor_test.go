//go:build linux

package x11

import (
	"bytes"
	"image/png"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/yasinmiran/remotejump/internal/protocol"
)

func nextCursor(t *testing.T, tx *testX, what string) protocol.Cursor {
	t.Helper()
	select {
	case c := <-tx.c.Cursor():
		return c
	case <-time.After(5 * time.Second):
		t.Fatalf("no cursor update: %s", what)
	}
	return protocol.Cursor{}
}

func TestCursorFollowsPointer(t *testing.T) {
	if _, err := exec.LookPath("xterm"); err != nil {
		t.Skip("no xterm")
	}
	tx := startX(t, "sleep 1000")
	first := nextCursor(t, tx, "initial cursor")
	img, err := png.Decode(bytes.NewReader(first.PNG))
	if err != nil {
		t.Fatalf("initial cursor PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() == 0 || first.HotX >= b.Dx() || first.HotY >= b.Dy() {
		t.Fatalf("cursor %dx%d with hotspot %d,%d", b.Dx(), b.Dy(), first.HotX, first.HotY)
	}

	xt := exec.Command("xterm", "-geometry", "80x10+0+0")
	xt.Env = append(os.Environ(), "DISPLAY="+tx.d.Name, "XAUTHORITY="+tx.d.XAuthority)
	if err := xt.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { xt.Process.Kill(); xt.Wait() })
	time.Sleep(500 * time.Millisecond)

	// Over the xterm the pointer becomes its text cursor: a new image.
	tx.input(t, protocol.Control{T: protocol.ControlMove, X: 60, Y: 40})
	over := nextCursor(t, tx, "pointer over xterm")
	if over.ID == first.ID || len(over.PNG) == 0 {
		t.Fatalf("over xterm: id %d (first %d), %d PNG bytes", over.ID, first.ID, len(over.PNG))
	}
	// Back on the root window the first image returns by ID alone.
	tx.input(t, protocol.Control{T: protocol.ControlMove, X: 900, Y: 600})
	back := nextCursor(t, tx, "pointer back on root")
	if back.ID != first.ID || len(back.PNG) != 0 {
		t.Fatalf("back on root: id %d (want %d), %d PNG bytes (want 0)", back.ID, first.ID, len(back.PNG))
	}
}
