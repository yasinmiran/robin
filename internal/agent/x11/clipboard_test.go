//go:build linux

package x11

import (
	"bytes"
	"math/rand/v2"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jezek/xgb/xproto"

	"github.com/yasinmiran/robin/internal/protocol"
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
	// property and anything longer as INCR, so 1 MiB and the cap take the
	// INCR path (if an xclip upgrade changes that threshold, this stops
	// covering INCR). Outbound, all of them are over one request and go by
	// INCR.
	for _, n := range []int{700_000, 1 << 20, MaxClipboard} {
		big := strings.Repeat("0123456789abcdeø", n/17) + strings.Repeat("x", n%17)
		start := time.Now()
		xclip(t, tx, big, "-i")
		expectClipboard(t, c, big)
		in := time.Since(start)
		if err := c.SetClipboard(big); err != nil {
			t.Fatal(err)
		}
		start = time.Now()
		if got := xclip(t, tx, "", "-o"); got != big {
			t.Errorf("xclip -o of %d bytes returned %d bytes", len(big), len(got))
		}
		t.Logf("%d bytes: in from xclip %v, out to xclip %v", n, in.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
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

// xclipCmd is xclip on the test display, for binary data and for runs the
// test watches itself.
func xclipCmd(tx *testX, args ...string) *exec.Cmd {
	cmd := exec.Command("xclip", append([]string{"-selection", "clipboard"}, args...)...)
	cmd.Env = append(os.Environ(), "DISPLAY="+tx.d.Name, "XAUTHORITY="+tx.d.XAuthority)
	return cmd
}

func expectContent(t *testing.T, c *Conn, want protocol.Clip) {
	t.Helper()
	select {
	case got := <-c.ClipboardContent():
		if got.Text != want.Text || got.HTML != want.HTML || !bytes.Equal(got.PNG, want.PNG) {
			t.Errorf("ClipboardContent() = %d/%d/%d bytes of text/HTML/PNG, want %d/%d/%d",
				len(got.Text), len(got.HTML), len(got.PNG), len(want.Text), len(want.HTML), len(want.PNG))
		}
	case <-time.After(10 * time.Second):
		t.Errorf("ClipboardContent() delivered nothing")
	}
}

// drain empties both clipboard channels of what earlier steps left.
func drain(c *Conn) {
	for {
		select {
		case <-c.Clipboard():
		case <-c.ClipboardContent():
		default:
			return
		}
	}
}

// pngLike is n bytes that start like a PNG; X does not look further.
func pngLike(n int) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewPCG(uint64(n), 1))
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	copy(b, "\x89PNG\r\n\x1a\n")
	return b
}

func TestClipboardRichContent(t *testing.T) {
	if _, err := exec.LookPath("xclip"); err != nil {
		t.Skip("no xclip")
	}
	tx := startX(t, "sleep 1000")
	c := tx.c

	// Browser to X: every format offered, the image big enough for INCR.
	clip := protocol.Clip{Text: "plain ✓", HTML: "<b>bold ✓</b>", PNG: pngLike(3 << 20)}
	// The session sets the clipboard on a data channel's read loop, so this
	// is how long a paste from the browser holds that loop.
	for i := range 3 {
		start := time.Now()
		if err := c.SetClipboardContent(clip); err != nil {
			t.Fatal(err)
		}
		t.Logf("SetClipboardContent %d took %v", i+1, time.Since(start))
	}
	targets := xclip(t, tx, "", "-o", "-t", "TARGETS")
	for _, want := range []string{"TARGETS", "UTF8_STRING", "STRING", "text/plain;charset=utf-8", "text/html", "image/png"} {
		if !strings.Contains(targets, want+"\n") {
			t.Errorf("TARGETS %q lacks %s", targets, want)
		}
	}
	if got := xclip(t, tx, "", "-o", "-t", "text/html"); got != clip.HTML {
		t.Errorf("text/html = %q, want %q", got, clip.HTML)
	}
	if got := xclip(t, tx, "", "-o", "-t", "image/png"); got != string(clip.PNG) {
		t.Errorf("image/png = %d bytes, want %d", len(got), len(clip.PNG))
	}
	if got := xclip(t, tx, "", "-o"); got != clip.Text {
		t.Errorf("text = %q, want %q", got, clip.Text)
	}
	// Two pastes at once, each by INCR: neither may hold up the other.
	results := make(chan []byte, 2)
	for range 2 {
		go func() {
			out, _ := xclipCmd(tx, "-o", "-t", "image/png").Output()
			results <- out
		}()
	}
	for range 2 {
		if got := <-results; !bytes.Equal(got, clip.PNG) {
			t.Errorf("concurrent image/png = %d bytes, want %d", len(got), len(clip.PNG))
		}
	}

	// An image alone offers no text, so a text paste gets nothing.
	if err := c.SetClipboardContent(protocol.Clip{PNG: pngLike(2000)}); err != nil {
		t.Fatal(err)
	}
	targets = xclip(t, tx, "", "-o", "-t", "TARGETS")
	if strings.Contains(targets, "UTF8_STRING") || !strings.Contains(targets, "image/png") {
		t.Errorf("TARGETS of an image alone: %q", targets)
	}
	if out, err := xclipCmd(tx, "-o", "-t", "UTF8_STRING").Output(); err == nil && len(out) > 0 {
		t.Errorf("an image alone pasted as text: %q", out)
	}
	if err := c.SetClipboardContent(protocol.Clip{PNG: make([]byte, MaxClipboard+1)}); err == nil {
		t.Error("SetClipboardContent over the cap succeeded")
	}

	// X to browser, each format the way a program offers it alone.
	drain(c)
	big := pngLike(5 << 20) // over xclip's single-property limit: INCR
	cmd := xclipCmd(tx, "-i", "-t", "image/png")
	cmd.Stdin = bytes.NewReader(big)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	expectContent(t, c, protocol.Clip{PNG: big})
	expectNoClipboard(t, c, 300*time.Millisecond) // no text, nothing for the text channel

	xclip(t, tx, "<i>from xclip</i>", "-i", "-t", "text/html")
	expectContent(t, c, protocol.Clip{HTML: "<i>from xclip</i>"})

	xclip(t, tx, "just text", "-i")
	expectContent(t, c, protocol.Clip{Text: "just text"})
	expectClipboard(t, c, "just text")

	// Over the cap: dropped, and what comes next still arrives.
	drain(c)
	cmd = xclipCmd(tx, "-i", "-t", "image/png")
	cmd.Stdin = bytes.NewReader(make([]byte, MaxClipboard+1))
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-c.ClipboardContent():
		t.Errorf("an image over the cap arrived with %d bytes", len(got.PNG))
	case <-time.After(3 * time.Second):
	}
	xclip(t, tx, "after the big one", "-i")
	expectContent(t, c, protocol.Clip{Text: "after the big one"})
}

// peerAtom interns name on the test's own connection.
func peerAtom(t *testing.T, tx *testX, name string) xproto.Atom {
	t.Helper()
	r, err := xproto.InternAtom(tx.peer, false, uint16(len(name)), name).Reply()
	if err != nil {
		t.Fatal(err)
	}
	return r.Atom
}

// askClipboard asks for CLIPBOARD as target, at time at, from a window of
// the test's own, and returns the property the owner answered in: 0 for a
// refusal.
func askClipboard(t *testing.T, tx *testX, target xproto.Atom, at xproto.Timestamp) xproto.Atom {
	t.Helper()
	win, err := xproto.NewWindowId(tx.peer)
	if err != nil {
		t.Fatal(err)
	}
	if err := xproto.CreateWindowChecked(tx.peer, 0, win, tx.root, 0, 0, 1, 1, 0, xproto.WindowClassInputOnly, 0, 0, nil).Check(); err != nil {
		t.Fatal(err)
	}
	defer xproto.DestroyWindow(tx.peer, win)
	xproto.ConvertSelection(tx.peer, win, peerAtom(t, tx, "CLIPBOARD"), target, peerAtom(t, tx, "_ROBIN_TEST_PASTE"), at)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-tx.events:
			if n, ok := ev.(xproto.SelectionNotifyEvent); ok && n.Requestor == win {
				return n.Property
			}
		case <-deadline:
			t.Fatal("the clipboard owner never answered")
			return 0
		}
	}
}

// ICCCM asks an owner to refuse a request older than its ownership. And a
// request from the agent's own window, as a read of a host copy makes when
// the page's paste lands first, must not start an INCR transfer to that
// window: nothing moves it on, and once it expires the window hears no
// property changes, so every later paste from the page fails.
func TestClipboardRefusesStaleAndOwnRequests(t *testing.T) {
	if _, err := exec.LookPath("xclip"); err != nil {
		t.Skip("no xclip")
	}
	// Set before the event loop starts, put back after it ends.
	incrTimeout = 300 * time.Millisecond
	t.Cleanup(func() { incrTimeout = 30 * time.Second })
	tx := startX(t, "sleep 1000")
	c := tx.c
	clip := protocol.Clip{Text: "served", PNG: pngLike(3 << 20)} // INCR, as in TestClipboardRichContent
	if err := c.SetClipboardContent(clip); err != nil {
		t.Fatal(err)
	}
	text := peerAtom(t, tx, "UTF8_STRING")
	c.clip.mu.Lock()
	since := c.clip.since
	c.clip.mu.Unlock()
	if prop := askClipboard(t, tx, text, since-1000); prop != 0 {
		t.Error("a request from before the agent took the clipboard was answered")
	}
	if prop := askClipboard(t, tx, text, xproto.TimeCurrentTime); prop == 0 {
		t.Error("a request at CurrentTime was refused")
	}

	xproto.ConvertSelection(tx.peer, c.clip.win, peerAtom(t, tx, "CLIPBOARD"), peerAtom(t, tx, protocol.MIMEPNG),
		peerAtom(t, tx, "_ROBIN_TEST_SELF"), xproto.TimeCurrentTime)
	time.Sleep(3 * incrTimeout)
	// A big paste elsewhere drops the transfers that expired meanwhile.
	if out, err := xclipCmd(tx, "-o", "-t", "image/png").Output(); err != nil || !bytes.Equal(out, clip.PNG) {
		t.Errorf("image/png = %d bytes (%v), want %d", len(out), err, len(clip.PNG))
	}
	if err := c.SetClipboardContent(protocol.Clip{Text: "after"}); err != nil {
		t.Fatalf("setting the clipboard once the agent asked itself: %v", err)
	}
	if got := xclip(t, tx, "", "-o"); got != "after" {
		t.Errorf("xclip -o = %q, want %q", got, "after")
	}
}
