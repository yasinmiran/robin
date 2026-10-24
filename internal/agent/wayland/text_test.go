//go:build linux

package wayland

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/godbus/dbus/v5"
)

// recorder stands in for the portal, recording what is asked of it. Its
// SelectionRead fails, as with nothing on the clipboard.
type recorder struct {
	dbus.BusObject // methods besides Call are not used
	mu             sync.Mutex
	calls          []string
}

func (r *recorder) Call(method string, _ dbus.Flags, args ...any) *dbus.Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch method {
	case ifaceRemote + ".NotifyKeyboardKeysym":
		dir := "up"
		if args[3].(uint32) == 1 {
			dir = "down"
		}
		r.calls = append(r.calls, "key "+symName(args[2].(int32))+" "+dir)
	case ifaceClipboard + ".SetSelection":
		r.calls = append(r.calls, "offer")
	case ifaceClipboard + ".SelectionRead":
		r.calls = append(r.calls, "read")
		return &dbus.Call{Err: errors.New("no selection owner")}
	default:
		r.calls = append(r.calls, method)
	}
	return &dbus.Call{}
}

// symName shows a keysym as its character where it is printable Latin-1,
// and by its name or number otherwise.
func symName(sym int32) string {
	switch {
	case sym == 0xffe3:
		return "Control_L"
	case 0x20 < sym && sym < 0x7f, 0xa0 < sym && sym <= 0xff, 0x1000000 < sym && unicode.IsPrint(rune(sym-0x1000000)):
		return string(rune(sym & 0xffffff))
	}
	return fmt.Sprintf("%#x", sym)
}

func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.calls
	r.calls = nil
	return out
}

// recorded is a desktop whose portal is a recorder, with the clipboard
// holding held as the user's text, or with none the agent knows of.
func recorded(withClipboard bool, held string) (*Desktop, *recorder) {
	r := &recorder{}
	s := &portalSession{p: &portal{obj: r}, handle: "/session"}
	d := &Desktop{in: &input{s: s, held: map[string]uint32{}, buttons: map[int32]bool{}}}
	if withClipboard {
		d.clip = &clipboard{
			s: s, updates: make(chan string, 1), log: slog.New(slog.DiscardHandler),
			held: held, heldOK: held != "", known: held != "",
		}
	}
	return d, r
}

func (c *clipboard) offered() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text
}

// restoreNow runs the put-back the last paste armed, as restoreAfter
// passing would, and returns what that asked of the portal.
func (d *Desktop) restoreNow(t *testing.T, r *recorder) []string {
	t.Helper()
	p := &d.clip.paste
	p.mu.Lock()
	p.timer.Reset(0)
	p.mu.Unlock()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if got := r.take(); len(got) > 0 {
			return got
		}
	}
	t.Fatal("the put-back asked nothing of the portal")
	return nil
}

var ctrlV = []string{"key Control_L down", "key v down", "key v up", "key Control_L up"}

func TestTypeASCII(t *testing.T) {
	d, r := recorded(true, "kept")
	if err := d.Type(t.Context(), "Hi!\x1b"); err != nil {
		t.Fatal(err)
	}
	want := []string{"key H down", "key H up", "key i down", "key i up", "key ! down", "key ! up"}
	if got := r.take(); !slices.Equal(got, want) {
		t.Errorf("portal got %q, want %q", got, want)
	}
}

func TestTypePastes(t *testing.T) {
	d, r := recorded(true, "kept")
	start := time.Now()
	if err := d.Type(t.Context(), "a日本\x00b"); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < pasteSettle {
		t.Errorf("Type returned %v after Ctrl+V, before the paste settled", took)
	}
	if got, want := r.take(), append([]string{"offer"}, ctrlV...); !slices.Equal(got, want) {
		t.Errorf("portal got %q, want %q", got, want)
	}
	if got := d.clip.offered(); got != "a日本b" {
		t.Errorf("offered %q, want the whole text", got)
	}

	// A second paste before the clipboard is put back keeps what the first
	// one replaced.
	if err := d.Type(t.Context(), "€"); err != nil {
		t.Fatal(err)
	}
	r.take()
	if got := d.restoreNow(t, r); !slices.Equal(got, []string{"offer"}) {
		t.Errorf("portal got %q, want the clipboard put back once", got)
	}
	if got := d.clip.offered(); got != "kept" {
		t.Errorf("clipboard put back to %q, want kept", got)
	}
}

// TestTypePasteLeavesNoText checks typed text does not stay on a clipboard
// that held none the agent knows of, and is never taken for the user's.
func TestTypePasteLeavesNoText(t *testing.T) {
	d, r := recorded(true, "")
	if err := d.Type(t.Context(), "日本"); err != nil {
		t.Fatal(err)
	}
	// What was there before the session is read once, before the first
	// paste; here there is nothing.
	if got, want := r.take(), slices.Concat([]string{"read", "offer"}, ctrlV); !slices.Equal(got, want) {
		t.Errorf("portal got %q, want %q", got, want)
	}
	d.restoreNow(t, r)
	if got := d.clip.offered(); got != "" {
		t.Errorf("clipboard holds %q after the paste, want empty text", got)
	}
	if err := d.Type(t.Context(), "€"); err != nil {
		t.Fatal(err)
	}
	if got, want := r.take(), append([]string{"offer"}, ctrlV...); !slices.Equal(got, want) {
		t.Errorf("portal got %q, want %q", got, want)
	}
	d.restoreNow(t, r)
	if got := d.clip.offered(); got != "" {
		t.Errorf("clipboard holds %q after a second paste, want empty text", got)
	}
}

func TestTypePasteYieldsToCopies(t *testing.T) {
	// The browser sets the clipboard: nothing goes back over it, and the
	// next paste puts the browser's text back.
	d, r := recorded(true, "kept")
	if err := d.Type(t.Context(), "日本"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetClipboard("newer"); err != nil {
		t.Fatal(err)
	}
	r.take()
	if err := d.clip.putBack(); err != nil || len(r.take()) != 0 || d.clip.offered() != "newer" {
		t.Errorf("put-back over the browser's text: %v, clipboard %q", err, d.clip.offered())
	}
	if err := d.Type(t.Context(), "日本"); err != nil {
		t.Fatal(err)
	}
	r.take()
	d.restoreNow(t, r)
	if got := d.clip.offered(); got != "newer" {
		t.Errorf("clipboard put back to %q, want the browser's text", got)
	}

	// The session ends: what the paste replaced goes back at once, and
	// only once.
	d, r = recorded(true, "kept")
	if err := d.Type(t.Context(), "日本"); err != nil {
		t.Fatal(err)
	}
	r.take()
	if err := d.clip.putBack(); err != nil {
		t.Fatal(err)
	}
	if got := r.take(); !slices.Equal(got, []string{"offer"}) || d.clip.offered() != "kept" {
		t.Errorf("portal got %q, clipboard %q; want kept put back", got, d.clip.offered())
	}
	if err := d.clip.putBack(); err != nil || len(r.take()) != 0 {
		t.Errorf("a second put-back: %v", err)
	}
}

func TestTypeWithoutClipboard(t *testing.T) {
	d, r := recorded(false, "")
	if err := d.Type(t.Context(), "é"); err != nil {
		t.Fatal(err)
	}
	if got := r.take(); !slices.Equal(got, []string{"key é down", "key é up"}) {
		t.Errorf("portal got %q, want the keysym", got)
	}
}

func TestTypeCancelled(t *testing.T) {
	d, r := recorded(true, "kept")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := d.Type(ctx, "abc"); !errors.Is(err, context.Canceled) {
		t.Errorf("Type = %v, want cancelled", err)
	}
	if err := d.Type(ctx, "日本"); !errors.Is(err, context.Canceled) {
		t.Errorf("paste = %v, want cancelled", err)
	}
	if got := r.take(); len(got) != 0 {
		t.Errorf("a cancelled Type asked the portal for %q", got)
	}

	// A context that ends while the paste settles ends the wait.
	ctx, cancel = context.WithTimeout(t.Context(), pasteSettle/10)
	defer cancel()
	if err := d.Type(ctx, "日本"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("paste past its deadline = %v", err)
	}
	if got, want := r.take(), append([]string{"offer"}, ctrlV...); !slices.Equal(got, want) {
		t.Errorf("portal got %q, want %q", got, want)
	}
}

func TestTypeViewOnly(t *testing.T) {
	d := &Desktop{in: &input{viewOnly: true}}
	if err := d.Type(t.Context(), "hi"); !errors.Is(err, errViewOnly) {
		t.Errorf("Type on a view-only session: %v", err)
	}
}
