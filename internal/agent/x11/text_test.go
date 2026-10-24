//go:build linux

package x11

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jezek/xgb/xproto"

	"github.com/yasinmiran/robin/internal/agent/keysym"
	"github.com/yasinmiran/robin/internal/protocol"
)

// cjk returns n distinct characters, none of which a keymap has, from the
// from-th of a fixed series.
func cjk(from, n int) string {
	r := make([]rune, n)
	for i := range r {
		r[i] = rune(0x4e00 + (from+i)*7)
	}
	return string(r)
}

func TestTypeText(t *testing.T) {
	tx := startX(t, "sleep 1000")
	o := newObserver(t, tx)

	// Within the spare keycodes, so the observer, which reads the keymap
	// after the fact, still decodes every one.
	text := "Hi, 日本語\tæ€😀\n\x1b\r"
	if err := tx.c.Type(t.Context(), text); err != nil {
		t.Fatal(err)
	}
	evs := o.keys(t)
	var want []xproto.Keysym
	for _, r := range text {
		if s, ok := keysym.Text(r); ok {
			want = append(want, xproto.Keysym(s))
		}
	}
	if got := typedSyms(evs); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("typed\n got %x\nwant %x", got, want)
	}
	for _, e := range evs {
		if e.state&^xproto.ModMaskShift != 0 {
			t.Errorf("unexpected modifiers on %v", e)
		}
	}
	tx.assertNothingHeld(t)
}

// TestTypeWaitsForSpares types more characters missing from the keymap than
// there are spare keycodes, and checks the ones that reuse a keycode wait
// for it to settle first.
func TestTypeWaitsForSpares(t *testing.T) {
	tx := startX(t, "sleep 1000")
	start := time.Now()
	if err := tx.c.Type(t.Context(), cjk(0, maxSpareKeys)); err != nil {
		t.Fatal(err)
	}
	// Every spare keycode was pressed just now, so the next one waits, and
	// the wait ends with the context.
	ctx, cancel := context.WithTimeout(t.Context(), spareSettle/10)
	defer cancel()
	if err := tx.c.Type(ctx, cjk(maxSpareKeys, 1)); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Type past its deadline = %v", err)
	}
	if err := tx.c.Type(t.Context(), cjk(maxSpareKeys, 1)); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < spareSettle {
		t.Errorf("rebinding a spare keycode came %v after its last press, want at least %v", d, spareSettle)
	}

	// A cancelled context stops between characters.
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	if err := tx.c.Type(ctx, "ab"); !errors.Is(err, context.Canceled) {
		t.Errorf("Type with a cancelled context = %v", err)
	}
	tx.assertNothingHeld(t)
}

func TestTypeOnClosedConnection(t *testing.T) {
	tx := startX(t, "sleep 1000")
	c, err := Open(tx.d.Name, tx.d.XAuthority)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if err := c.Type(t.Context(), "a"); err == nil {
		t.Error("Type on a closed connection succeeded")
	}
}

// TestTypeIntoXterm types a line with several times as many characters
// missing from the keymap as there are spare keycodes, so each keycode is
// bound to many characters in turn, and checks xterm reads every one right.
func TestTypeIntoXterm(t *testing.T) {
	if _, err := exec.LookPath("xterm"); err != nil {
		t.Skip("no xterm")
	}
	tx := startX(t, "sleep 1000")
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	xt := exec.Command("xterm", "-geometry", "80x10+0+0", "-e", "sh", "-c",
		`stty -echo; touch "$0.ready"; IFS= read -r line; printf %s "$line" > "$0"`, out)
	xt.Env = append(os.Environ(), "DISPLAY="+tx.d.Name, "XAUTHORITY="+tx.d.XAuthority, "LANG=C.UTF-8")
	if err := xt.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { xt.Process.Kill(); xt.Wait() })
	waitFile(t, out+".ready")
	waitMapped(t, tx)
	tx.input(t, protocol.Control{T: protocol.ControlMove, X: 100, Y: 50}) // no WM: focus follows the pointer

	text := "ab " + cjk(0, 4*maxSpareKeys) + " éж😀 {~}"
	start := time.Now()
	if err := tx.c.Type(t.Context(), text+"\n"); err != nil {
		t.Fatal(err)
	}
	t.Logf("typed %d characters in %v", len([]rune(text)), time.Since(start))
	waitFile(t, out)
	b, _ := os.ReadFile(out)
	if string(b) != text {
		t.Errorf("xterm read\n %q\nwant\n %q", b, text)
	}
	tx.assertNothingHeld(t)
}
