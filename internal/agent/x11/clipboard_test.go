//go:build linux

package x11

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func xclip(t *testing.T, tx *testX, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("xclip", append([]string{"-selection", "clipboard"}, args...)...)
	cmd.Env = append(os.Environ(), "DISPLAY="+tx.d.Name, "XAUTHORITY="+tx.d.XAuthority)
	if stdin != "" {
		// xclip -i forks a server that would hold a stdout pipe open.
		cmd.Stdin = strings.NewReader(stdin)
		if err := cmd.Run(); err != nil {
			t.Fatalf("xclip %v: %v", args, err)
		}
		return ""
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("xclip %v: %v", args, err)
	}
	return string(out)
}

func expectClipboard(t *testing.T, c *Conn, want string) {
	t.Helper()
	select {
	case got := <-c.Clipboard():
		if got != want {
			t.Errorf("Clipboard() = %.60q (%d bytes), want %.60q (%d bytes)", got, len(got), want, len(want))
		}
	case <-time.After(5 * time.Second):
		t.Errorf("Clipboard() delivered nothing, want %.60q", want)
	}
}

func expectNoClipboard(t *testing.T, c *Conn, wait time.Duration) {
	t.Helper()
	select {
	case got := <-c.Clipboard():
		t.Errorf("Clipboard() = %.60q, want nothing", got)
	case <-time.After(wait):
	}
}

func TestClipboard(t *testing.T) {
	if _, err := exec.LookPath("xclip"); err != nil {
		t.Skip("no xclip")
	}
	tx := startX(t, "sleep 1000")
	c := tx.c

	// Browser to X.
	text := "héllo wörld € ✓"
	if err := c.SetClipboard(text); err != nil {
		t.Fatal(err)
	}
	if got := xclip(t, tx, "", "-o"); got != text {
		t.Errorf("xclip -o = %q, want %q", got, text)
	}
	if got := xclip(t, tx, "", "-o", "-t", "STRING"); got != "h\xe9llo w\xf6rld ? ?" {
		t.Errorf("STRING target = %q, want Latin-1", got)
	}
	targets := xclip(t, tx, "", "-o", "-t", "TARGETS")
	for _, want := range []string{"TARGETS", "UTF8_STRING", "STRING", "TEXT", "text/plain;charset=utf-8"} {
		if !strings.Contains(targets, want+"\n") {
			t.Errorf("TARGETS %q lacks %s", targets, want)
		}
	}
	expectNoClipboard(t, c, 300*time.Millisecond) // our own selection is not echoed

	// X to browser, then back again to check ownership flips cleanly.
	xclip(t, tx, "from xclip ✓", "-i")
	expectClipboard(t, c, "from xclip ✓")
	if err := c.SetClipboard("again"); err != nil {
		t.Fatal(err)
	}
	if got := xclip(t, tx, "", "-o"); got != "again" {
		t.Errorf("xclip -o = %q after retaking the selection", got)
	}

	// Large texts. With BIG-REQUESTS, xclip sends up to 1 MiB - 1 in one
	// property and anything longer as INCR, so exactly the cap takes the INCR
	// path (if an xclip upgrade changes that threshold, this stops covering
	// INCR). Outbound, both need more than one ChangeProperty.
	for _, n := range []int{700_000, MaxClipboard} {
		big := strings.Repeat("0123456789abcdeø", n/17) + strings.Repeat("x", n%17)
		xclip(t, tx, big, "-i")
		expectClipboard(t, c, big)
		if err := c.SetClipboard(big); err != nil {
			t.Fatal(err)
		}
		if got := xclip(t, tx, "", "-o"); got != big {
			t.Errorf("xclip -o of %d bytes returned %d bytes", len(big), len(got))
		}
	}

	// Over the cap: refused outbound, dropped inbound.
	huge := strings.Repeat("x", MaxClipboard+1)
	if err := c.SetClipboard(huge); err == nil {
		t.Error("SetClipboard over the cap succeeded")
	}
	xclip(t, tx, huge, "-i")
	expectNoClipboard(t, c, 2*time.Second)
	xclip(t, tx, "small after huge", "-i")
	expectClipboard(t, c, "small after huge")
}

// TestCloseAfterServerDeath: the display dying first must not wedge Close,
// Capture or the clipboard channel.
func TestCloseAfterServerDeath(t *testing.T) {
	tx := startX(t, "sleep 1000")
	tx.d.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, _, err := tx.c.Capture(true); err == nil {
			t.Error("Capture on a dead display succeeded")
		}
		if err := tx.c.SetClipboard("x"); err == nil {
			t.Error("SetClipboard on a dead display succeeded")
		}
		for range tx.c.Clipboard() {
		}
		tx.c.Close()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("hung after the server died")
	}
}
