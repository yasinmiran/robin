//go:build linux

package wayland

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode"

	"github.com/godbus/dbus/v5"

	"github.com/yasinmiran/robin/internal/agent/xkb"
	"github.com/yasinmiran/robin/internal/protocol"
)

// recorder stands in for the portal, recording what is asked of it. It
// plays the desktop too: managers clipboard managers that read every
// offer, and a focused program that pastes when the keys in pastesOn come,
// Ctrl+V for most programs or Ctrl+Shift+V for a terminal, matching V by
// its key, as programs match shortcuts on layouts without Latin letters.
// SelectionRead returns what clip holds as the type asked for, which is
// nothing for a type it lacks, or fails as with no owner while clip is nil.
type recorder struct {
	dbus.BusObject // methods besides Call and CallWithContext are not used
	mu             sync.Mutex
	calls          []string
	held           map[int32]bool
	managers       int
	pastesOn       string // "ctrl+v", "ctrl+shift+v" or ""
	clip           map[string][]byte
	read           func() // counts a read of the agent's offer
}

// keyV is the evdev code of the V key.
const keyV = 47

func (r *recorder) CallWithContext(_ context.Context, method string, flags dbus.Flags, args ...any) *dbus.Call {
	return r.Call(method, flags, args...)
}

func (r *recorder) Call(method string, _ dbus.Flags, args ...any) *dbus.Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch method {
	case ifaceRemote + ".NotifyKeyboardKeysym":
		sym, down := args[2].(int32), args[3].(uint32) == 1
		dir := "up"
		if down {
			dir = "down"
		}
		r.calls = append(r.calls, "key "+symName(sym)+" "+dir)
		if r.held == nil {
			r.held = map[int32]bool{}
		}
		r.held[sym] = down
		if down && sym == 'v' {
			r.pressedV()
		}
	case ifaceRemote + ".NotifyKeyboardKeycode":
		code, down := args[2].(int32), args[3].(uint32) == 1
		dir := "up"
		if down {
			dir = "down"
		}
		r.calls = append(r.calls, fmt.Sprintf("code %d %s", code, dir))
		if down && code == keyV {
			r.pressedV()
		}
	case ifaceClipboard + ".SetSelection":
		r.calls = append(r.calls, "offer")
		for range r.managers {
			r.read()
		}
	case ifaceClipboard + ".SelectionRead":
		r.calls = append(r.calls, "read")
		if r.clip == nil {
			return &dbus.Call{Err: errors.New("no selection owner")}
		}
		var fds [2]int
		if err := syscall.Pipe2(fds[:], syscall.O_CLOEXEC); err != nil {
			return &dbus.Call{Err: err}
		}
		// More than a pipe holds, so it is written while the agent reads.
		go func(data []byte) {
			w := os.NewFile(uintptr(fds[1]), "clip")
			w.Write(data)
			w.Close()
		}(r.clip[args[1].(string)])
		return &dbus.Call{Body: []any{dbus.UnixFD(fds[0])}}
	default:
		r.calls = append(r.calls, method)
	}
	return &dbus.Call{}
}

// pressedV pastes if the modifiers held make the chord the focused program
// pastes with. r.mu must be held.
func (r *recorder) pressedV() {
	chord := "ctrl+v"
	switch {
	case !r.held[control]:
		return
	case r.held[shift]:
		chord = "ctrl+shift+v"
	}
	if chord == r.pastesOn {
		r.calls = append(r.calls, "pasted")
		r.read()
	}
}

// symName shows a keysym as its character where it is printable Latin-1,
// and by its name or number otherwise.
func symName(sym int32) string {
	switch {
	case sym == control:
		return "Control_L"
	case sym == shift:
		return "Shift_L"
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
// holding held as the user's text, or with none the agent knows of. Its
// clipboard manager reads every offer, and its focused program pastes with
// Ctrl+V.
func recorded(withClipboard bool, held string) (*Desktop, *recorder) {
	r := &recorder{managers: 1, pastesOn: "ctrl+v"}
	s := &portalSession{p: &portal{obj: r}, handle: "/session"}
	d := &Desktop{in: &input{s: s, held: map[string]uint32{}, buttons: map[int32]bool{}}}
	r.read = func() {}
	if withClipboard {
		d.clip = &clipboard{
			s: s, updates: make(chan string, 1), log: slog.New(slog.DiscardHandler),
			held: protocol.Clip{Text: held}, known: held != "",
		}
		r.read = d.clip.countRead
	}
	return d, r
}

func (c *clipboard) offered() protocol.Clip {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.content
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

var (
	ctrlV      = []string{"key Control_L down", "key v down", "key v up", "key Control_L up"}
	ctrlShiftV = []string{"key Control_L down", "key Shift_L down", "key v down", "key v up", "key Shift_L up", "key Control_L up"}
	// pasteV is what a paste asks of the portal where the program pastes
	// with Ctrl+V: Ctrl+Shift+V goes first and nothing reads the offer.
	pasteV = slices.Concat([]string{"offer"}, ctrlShiftV, ctrlV[:2], []string{"pasted"}, ctrlV[2:])
)

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
	if took := time.Since(start); took < managerQuiet+pasteRead+pasteSettle {
		t.Errorf("Type returned %v after it began, before Ctrl+V's paste settled", took)
	}
	if got := r.take(); !slices.Equal(got, pasteV) {
		t.Errorf("portal got %q, want %q", got, pasteV)
	}
	if got := d.clip.offered().Text; got != "a日本b" {
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
	if got := d.clip.offered().Text; got != "kept" {
		t.Errorf("clipboard put back to %q, want kept", got)
	}

	// Text that types nothing pastes nothing.
	if err := d.Type(t.Context(), "\x00\x01"); err != nil || len(r.take()) != 0 {
		t.Errorf("a paste of nothing: %v", err)
	}
}

// TestTypePastesIntoATerminal: a program that pastes with Ctrl+Shift+V
// reads the offer, so Ctrl+V, which a terminal takes as a control
// character, never goes.
func TestTypePastesIntoATerminal(t *testing.T) {
	d, r := recorded(true, "kept")
	r.pastesOn = "ctrl+shift+v"
	start := time.Now()
	if err := d.Type(t.Context(), "日本"); err != nil {
		t.Fatal(err)
	}
	want := slices.Concat([]string{"offer"}, ctrlShiftV[:3], []string{"pasted"}, ctrlShiftV[3:])
	if got := r.take(); !slices.Equal(got, want) {
		t.Errorf("portal got %q, want %q", got, want)
	}
	if took := time.Since(start); took >= pasteRead {
		t.Errorf("Type took %v with the offer read at once", took)
	}
}

// TestTypePasteWithoutAClipboardManager: with no manager to read the
// offer first, the paste waits managerRead for one, then goes as before.
func TestTypePasteWithoutAClipboardManager(t *testing.T) {
	d, r := recorded(true, "kept")
	r.managers = 0
	start := time.Now()
	if err := d.Type(t.Context(), "日本"); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < managerRead+pasteRead+pasteSettle {
		t.Errorf("Type took only %v", took)
	}
	if got := r.take(); !slices.Equal(got, pasteV) {
		t.Errorf("portal got %q, want %q", got, pasteV)
	}
}

// TestTypePasteAfterManyManagers: the paste keys wait until every manager
// has read the offer, so that none of their reads counts as the focused
// program's, and a program that pastes with Ctrl+V still gets Ctrl+V.
func TestTypePasteAfterManyManagers(t *testing.T) {
	d, r := recorded(true, "kept")
	r.managers = 3
	// The last manager reads after the others, though within managerQuiet.
	go func() {
		time.Sleep(managerQuiet / 2)
		r.read()
	}()
	if err := d.Type(t.Context(), "日本"); err != nil {
		t.Fatal(err)
	}
	if got := r.take(); !slices.Equal(got, pasteV) {
		t.Errorf("portal got %q, want %q", got, pasteV)
	}
}

// TestTypePasteLeavesNoText checks typed text does not stay on a clipboard
// that held none the agent knows of, and is never taken for the user's.
func TestTypePasteLeavesNoText(t *testing.T) {
	d, r := recorded(true, "")
	if err := d.Type(t.Context(), "日本"); err != nil {
		t.Fatal(err)
	}
	// What was there before the session is read once, in each format,
	// before the first paste; here there is nothing.
	if got, want := r.take(), slices.Concat([]string{"read", "read", "read"}, pasteV); !slices.Equal(got, want) {
		t.Errorf("portal got %q, want %q", got, want)
	}
	d.restoreNow(t, r)
	if got := d.clip.offered(); got.Size() != 0 {
		t.Errorf("clipboard holds %+v after the paste, want empty text", got)
	}
	if err := d.Type(t.Context(), "€"); err != nil {
		t.Fatal(err)
	}
	if got := r.take(); !slices.Equal(got, pasteV) {
		t.Errorf("portal got %q, want %q", got, pasteV)
	}
	d.restoreNow(t, r)
	if got := d.clip.offered(); got.Size() != 0 {
		t.Errorf("clipboard holds %+v after a second paste, want empty text", got)
	}
}

// TestTypePasteRestoresEveryFormat: a clipboard from before the session is
// read in each format the rich clipboard knows, and put back in each.
func TestTypePasteRestoresEveryFormat(t *testing.T) {
	d, r := recorded(true, "")
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 200<<10)...)
	r.clip = map[string][]byte{textMimes[0]: []byte("before"), protocol.MIMEHTML: []byte("<b>before</b>"), protocol.MIMEPNG: png}
	if err := d.Type(t.Context(), "日本"); err != nil {
		t.Fatal(err)
	}
	r.take()
	if got := d.restoreNow(t, r); !slices.Equal(got, []string{"offer"}) {
		t.Errorf("portal got %q, want the clipboard put back once", got)
	}
	if got := d.clip.offered(); got.Text != "before" || got.HTML != "<b>before</b>" || !bytes.Equal(got.PNG, png) {
		t.Errorf("put back %q, %q and %d bytes of PNG", got.Text, got.HTML, len(got.PNG))
	}

	// Formats that do not fit maxClipboard together are left out, the
	// later ones first.
	d, r = recorded(true, "")
	r.clip = map[string][]byte{textMimes[0]: []byte("before"), protocol.MIMEPNG: make([]byte, maxClipboard)}
	if err := d.Type(t.Context(), "日本"); err != nil {
		t.Fatal(err)
	}
	r.take()
	d.restoreNow(t, r)
	if got := d.clip.offered(); got.Text != "before" || got.PNG != nil {
		t.Errorf("put back %q and %d bytes of PNG, want the text alone", got.Text, len(got.PNG))
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
	if err := d.clip.putBack(); err != nil || len(r.take()) != 0 || d.clip.offered().Text != "newer" {
		t.Errorf("put-back over the browser's text: %v, clipboard %q", err, d.clip.offered().Text)
	}
	if err := d.Type(t.Context(), "日本"); err != nil {
		t.Fatal(err)
	}
	r.take()
	d.restoreNow(t, r)
	if got := d.clip.offered().Text; got != "newer" {
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
	if got := r.take(); !slices.Equal(got, []string{"offer"}) || d.clip.offered().Text != "kept" {
		t.Errorf("portal got %q, clipboard %q; want kept put back", got, d.clip.offered().Text)
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

// TestTypeAnyKeysym: where the compositor types any keysym, as KWin does,
// text never touches the clipboard.
func TestTypeAnyKeysym(t *testing.T) {
	d, r := recorded(true, "kept")
	d.anyKeysym = true
	if err := d.Type(t.Context(), "a日\n"); err != nil {
		t.Fatal(err)
	}
	want := []string{"key a down", "key a up", "key 日 down", "key 日 up", "key 0xff0d down", "key 0xff0d up"}
	if got := r.take(); !slices.Equal(got, want) {
		t.Errorf("portal got %q, want %q", got, want)
	}
}

// cyrillic is a keyboard whose layout has digits and ! but no Latin
// letters, as Russian does, until source names its variant with them.
func cyrillic(t *testing.T, source *string) *keyboard {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "symbols"), 0o755); err != nil {
		t.Fatal(err)
	}
	symbols := `default xkb_symbols "basic" { name[Group1] = "Cyrillic"; key <AC01> { [ Cyrillic_ef, 1, exclam ] }; };
xkb_symbols "latin" { name[Group1] = "Latin"; key <AC01> { [ o, k ] }; };`
	if err := os.WriteFile(filepath.Join(dir, "symbols", "cy"), []byte(symbols), 0o644); err != nil {
		t.Fatal(err)
	}
	k := &keyboard{
		source: func() (string, error) { return *source, nil },
		load:   func(l, v string) (xkb.Layout, error) { return xkb.Load([]string{dir}, l, v) },
		log:    slog.New(slog.DiscardHandler),
	}
	k.update()
	return k
}

// TestTypeFollowsTheLayout: on a layout without Latin letters even ASCII
// is pasted, with V pressed by its key since the layout has no v, and on
// one with them what it lacks is.
func TestTypeFollowsTheLayout(t *testing.T) {
	d, r := recorded(true, "kept")
	source := "cy"
	d.kb = cyrillic(t, &source)
	d.in.lacks = d.kb.lacks
	if err := d.Type(t.Context(), "1!"); err != nil {
		t.Fatal(err)
	}
	if got := r.take(); !slices.Equal(got, []string{"key 1 down", "key 1 up", "key ! down", "key ! up"}) {
		t.Errorf("text the layout has: portal got %q", got)
	}
	if err := d.Type(t.Context(), "ok"); err != nil {
		t.Fatal(err)
	}
	byKey := func(keys []string) []string {
		return slices.Concat(keys[:2], []string{"code 47 down", "code 47 up"}, keys[4:])
	}
	want := slices.Concat([]string{"offer"}, byKey(ctrlShiftV),
		[]string{"key Control_L down", "code 47 down", "pasted", "code 47 up", "key Control_L up"})
	if got := r.take(); !slices.Equal(got, want) {
		t.Errorf("ASCII the layout lacks: portal got %q, want %q", got, want)
	}

	source = "cy+latin"
	d.kb.update()
	if err := d.Type(t.Context(), "ok"); err != nil {
		t.Fatal(err)
	}
	if got := r.take(); !slices.Equal(got, []string{"key o down", "key o up", "key k down", "key k up"}) {
		t.Errorf("text the new layout has: portal got %q", got)
	}
}

// TestKeysTheLayoutLacks: a key whose character the layout lacks goes by
// the key a US layout has it on, with Shift where that takes it, so
// shortcuts reach programs; one the layout has goes as its keysym.
func TestKeysTheLayoutLacks(t *testing.T) {
	d, r := recorded(false, "")
	source := "cy"
	d.in.lacks = cyrillic(t, &source).lacks
	press := func(key, code string) {
		t.Helper()
		if err := d.Input(protocol.Control{T: protocol.ControlKey, Key: key, Code: code, Down: true}); err != nil {
			t.Fatal(err)
		}
		if err := d.Input(protocol.Control{T: protocol.ControlKey, Key: key, Code: code}); err != nil {
			t.Fatal(err)
		}
	}
	hold := func(key, code string, down bool) {
		t.Helper()
		if err := d.Input(protocol.Control{T: protocol.ControlKey, Key: key, Code: code, Down: down}); err != nil {
			t.Fatal(err)
		}
	}
	press("a", "KeyA")
	press("A", "KeyA") // Caps Lock: a capital without Shift held
	press("!", "Digit1")
	hold("Shift", "ShiftLeft", true)
	press("{", "BracketLeft")
	hold("Shift", "ShiftLeft", false)
	hold("Control", "ControlLeft", true)
	press("c", "KeyC")
	hold("Control", "ControlLeft", false)
	press("é", "Quote")
	want := []string{
		"code 30 down", "code 30 up",
		"key Shift_L down", "code 30 down", "code 30 up", "key Shift_L up",
		"key ! down", "key ! up",
		"key Shift_L down", "code 26 down", "code 26 up", "key Shift_L up",
		"key Control_L down", "code 46 down", "code 46 up", "key Control_L up",
		"key é down", "key é up",
	}
	if got := r.take(); !slices.Equal(got, want) {
		t.Errorf("portal got %q, want %q", got, want)
	}
}

func TestUSKey(t *testing.T) {
	seen := map[[2]uint32]rune{}
	for r := rune('!'); r < 0x7f; r++ {
		code, shift, ok := usKey(r)
		if !ok {
			t.Errorf("%q: no US key", r)
			continue
		}
		k := [2]uint32{code, 0}
		if shift {
			k[1] = 1
		}
		if other, dup := seen[k]; dup {
			t.Errorf("%q and %q on the same key and level", other, r)
		}
		seen[k] = r
	}
	for r, want := range map[rune][2]uint32{'a': {30, 0}, 'Q': {16, 1}, '`': {41, 0}, '|': {43, 1}, '?': {53, 1}, '=': {13, 0}, 'v': {47, 0}} {
		code, shift, _ := usKey(r)
		if code != want[0] || shift != (want[1] == 1) {
			t.Errorf("%q: key %d shift %v, want %v", r, code, shift, want)
		}
	}
	if _, _, ok := usKey('é'); ok {
		t.Error("é has a US key")
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

	// A context that ends while the paste waits for a read ends the wait,
	// and so does one that ends while it settles.
	for _, tc := range []struct {
		name     string
		pastesOn string
		managers int
		want     []string
	}{
		{"waiting for the manager", "", 0, []string{"offer"}},
		{"waiting for a read", "", 1, slices.Concat([]string{"offer"}, ctrlShiftV)},
		{"settling", "ctrl+shift+v", 1, slices.Concat([]string{"offer"}, ctrlShiftV[:3], []string{"pasted"}, ctrlShiftV[3:])},
	} {
		d, r := recorded(true, "kept")
		r.pastesOn, r.managers = tc.pastesOn, tc.managers
		wait := managerQuiet + pasteSettle/2
		if tc.managers == 0 {
			wait = managerRead / 2
		}
		ctx, cancel := context.WithTimeout(t.Context(), wait)
		if err := d.Type(ctx, "日本"); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s: paste past its deadline = %v", tc.name, err)
		}
		cancel()
		if got := r.take(); !slices.Equal(got, tc.want) {
			t.Errorf("%s: portal got %q, want %q", tc.name, got, tc.want)
		}
		// The text goes back off the clipboard all the same.
		if got := d.restoreNow(t, r); !slices.Equal(got, []string{"offer"}) || d.clip.offered().Text != "kept" {
			t.Errorf("%s: put back with %q to %q, want kept", tc.name, got, d.clip.offered().Text)
		}
	}
}

func TestTypeViewOnly(t *testing.T) {
	d := &Desktop{in: &input{viewOnly: true}}
	if err := d.Type(t.Context(), "hi"); !errors.Is(err, errViewOnly) {
		t.Errorf("Type on a view-only session: %v", err)
	}
}
