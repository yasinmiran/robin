//go:build linux

package wayland

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// recorder stands in for the portal, recording what is asked of it.
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
		r.calls = append(r.calls, "key "+string(rune(args[2].(int32)&0xffff))+" "+dir)
	case ifaceClipboard + ".SetSelection":
		r.calls = append(r.calls, "offer")
	default:
		r.calls = append(r.calls, method)
	}
	return &dbus.Call{}
}

func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.calls
	r.calls = nil
	return out
}

// recorded is a desktop whose portal is a recorder, with the clipboard
// holding held.
func recorded(withClipboard bool, held string) (*Desktop, *recorder) {
	r := &recorder{}
	s := &portalSession{p: &portal{obj: r}, handle: "/session"}
	d := &Desktop{in: &input{s: s, held: map[string]uint32{}, buttons: map[int32]bool{}}}
	if withClipboard {
		d.clip = &clipboard{s: s, updates: make(chan string, 1), held: held, heldOK: held != ""}
	}
	return d, r
}

func (c *clipboard) offered() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text
}

var ctrlV = []string{"key ￣ down", "key v down", "key v up", "key ￣ up"}

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
	time.Sleep(restoreAfter + 200*time.Millisecond)
	if got := r.take(); !slices.Equal(got, []string{"offer"}) {
		t.Errorf("portal got %q, want the clipboard put back once", got)
	}
	if got := d.clip.offered(); got != "kept" {
		t.Errorf("clipboard put back to %q, want kept", got)
	}
}

func TestTypePasteYieldsToCopies(t *testing.T) {
	// The browser sets the clipboard: nothing goes back over it.
	d, r := recorded(true, "kept")
	if err := d.Type(t.Context(), "日本"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetClipboard("newer"); err != nil {
		t.Fatal(err)
	}
	d.clip.putBack()
	if got := d.clip.offered(); got != "newer" {
		t.Errorf("clipboard %q, want the browser's newer text", got)
	}

	// The clipboard held no text: there is nothing to put back.
	d, r = recorded(true, "")
	if err := d.Type(t.Context(), "日本"); err != nil {
		t.Fatal(err)
	}
	r.take()
	d.clip.putBack()
	if got := r.take(); len(got) != 0 {
		t.Errorf("portal got %q after the paste, want nothing put back", got)
	}

	// The session ends: what the paste replaced goes back at once.
	d, r = recorded(true, "kept")
	if err := d.Type(t.Context(), "日本"); err != nil {
		t.Fatal(err)
	}
	r.take()
	d.clip.putBack()
	if got := r.take(); !slices.Equal(got, []string{"offer"}) || d.clip.offered() != "kept" {
		t.Errorf("portal got %q, clipboard %q; want kept put back", got, d.clip.offered())
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
	start := time.Now()
	if err := d.Type(ctx, "日本"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) >= pasteSettle {
		t.Error("a cancelled paste waited to settle")
	}
	if got := strings.Join(r.take(), ","); !strings.HasPrefix(got, "offer") {
		t.Errorf("portal got %q", got)
	}
}

func TestTypeViewOnly(t *testing.T) {
	d := &Desktop{in: &input{viewOnly: true}}
	if err := d.Type(t.Context(), "hi"); !errors.Is(err, errViewOnly) {
		t.Errorf("Type on a view-only session: %v", err)
	}
}
