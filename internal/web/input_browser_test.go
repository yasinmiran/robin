package web

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/protocol"
)

// TestBrowserInput runs testdata/browser/input.mjs: an input method composing
// in desktop Chrome, then Chrome as a phone, with taps, a long press, a
// two-finger scroll, a pinch and the soft keyboard. It checks what the page
// sends the agent stand-in. Like TestBrowserSmoke it needs `npm ci` in
// testdata/browser.
func TestBrowserInput(t *testing.T) {
	dir := filepath.Join("testdata", "browser")
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "playwright-core")); err != nil {
		t.Skip("browser input test needs `npm ci` in internal/web/testdata/browser")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("browser input test needs node on PATH")
	}
	frames := filepath.Join(t.TempDir(), "frames.json")
	s, _ := start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)

	var (
		mu       sync.Mutex
		controls []protocol.Control
		agents   sync.WaitGroup
	)
	agents.Add(1)
	go func() {
		defer agents.Done()
		for {
			b, err := s.Accept(ctx)
			if err != nil {
				return
			}
			agents.Add(1)
			go func() {
				defer agents.Done()
				err := playAgent(ctx, b, frames, func(string) {},
					func(c protocol.Control) { mu.Lock(); controls = append(controls, c); mu.Unlock() })
				if err != nil {
					t.Errorf("agent stand-in: %v", err)
				}
			}()
		}
	}()

	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, node, "input.mjs", s.URL(), frames)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	cancel()
	agents.Wait()
	t.Logf("input.mjs:\n%s", out.String())
	if err != nil {
		t.Fatalf("input.mjs: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var got []protocol.Control
	for _, c := range controls {
		if c.T != protocol.ControlResize && c.T != protocol.ControlRelease {
			got = append(got, c)
		}
	}
	key := func(k string, down bool) protocol.Control {
		return protocol.Control{T: protocol.ControlKey, Key: k, Code: "Key" + string(k[0]-'a'+'A'), Down: down}
	}
	button := func(x, y, b int, down bool) protocol.Control {
		return protocol.Control{T: protocol.ControlButton, X: x, Y: y, Button: b, Down: down}
	}
	move := func(x, y int) protocol.Control { return protocol.Control{T: protocol.ControlMove, X: x, Y: y} }
	text := func(s string) protocol.Control { return protocol.Control{T: protocol.ControlText, Text: s} }

	// In order, with what may come between them left out.
	want := []protocol.Control{
		text("日本"), key("z", true), key("z", false),
		move(100, 200), button(100, 200, 0, true), button(100, 200, 0, false),
		move(300, 300), button(300, 300, 2, true), button(300, 300, 2, false),
		move(550, 390),
		// Zoomed 2x about the centre, the page's corner is remote 320,180.
		move(320, 180), button(320, 180, 0, true), button(320, 180, 0, false),
		text("robin"), text("!"),
	}
	at := 0
	for _, c := range got {
		if at < len(want) && c == want[at] {
			at++
		}
	}
	if at < len(want) {
		t.Errorf("page sent %+v\nmissing %+v and what follows it", got, want[at])
	}

	var scrolled float64
	for _, c := range got {
		switch {
		case c.T == protocol.ControlWheel:
			scrolled += c.DY
			if c.DX != 0 {
				t.Errorf("a vertical two-finger drag scrolled sideways: %+v", c)
			}
		case c.T == protocol.ControlKey && (c.Key == "Process" || c.Key == "Unidentified"):
			t.Errorf("a key an input method owned was sent: %+v", c)
		}
	}
	if scrolled < 90 || scrolled > 110 {
		t.Errorf("fingers moving up 100 px scrolled %v, want about 100 down", scrolled)
	}
	if n := slices.IndexFunc(got, func(c protocol.Control) bool { return c.T == protocol.ControlText && c.Text == "robi" }); n >= 0 {
		t.Errorf("a composition still in progress was sent: %+v", got[n])
	}
}
