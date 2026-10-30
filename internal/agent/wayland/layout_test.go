//go:build linux

package wayland

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/yasinmiran/robin/internal/agent/xkb"
)

func TestParseSources(t *testing.T) {
	for in, want := range map[string][][2]string{
		"[('xkb', 'us'), ('xkb', 'ru')]\n":          {{"xkb", "us"}, {"xkb", "ru"}},
		"@a(ss) []\n":                               nil,
		`[('ibus', 'mozc-jp'), ("xkb", "us+intl")]`: {{"ibus", "mozc-jp"}, {"xkb", "us+intl"}},
		"garbage": nil,
	} {
		if got := parseSources(in); !slices.Equal(got, want) {
			t.Errorf("parseSources(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCurrentSource(t *testing.T) {
	us, ru, mozc := [2]string{"xkb", "us"}, [2]string{"xkb", "ru"}, [2]string{"ibus", "mozc-jp"}
	for _, tc := range []struct {
		sources, mru [][2]string
		want         string
	}{
		{nil, [][2]string{ru}, ""},
		{[][2]string{us, ru}, nil, "us"},
		{[][2]string{us, ru}, [][2]string{ru, us}, "ru"},
		// A recently used source since removed does not count.
		{[][2]string{us}, [][2]string{ru, us}, "us"},
		{[][2]string{mozc, us}, nil, ""},
	} {
		if got := currentSource(tc.sources, tc.mru); got != tc.want {
			t.Errorf("currentSource(%v, %v) = %q, want %q", tc.sources, tc.mru, got, tc.want)
		}
	}
}

func TestTouchesInputSources(t *testing.T) {
	for _, tc := range []struct {
		body []any
		want bool
	}{
		{[]any{"/org/gnome/desktop/input-sources/mru-sources", []string{""}, "tag"}, true},
		{[]any{"/org/gnome/desktop/", []string{"input-sources/sources", "interface/font-name"}, "tag"}, true},
		{[]any{"/", []string{}, "tag"}, true},
		{[]any{"/org/gnome/desktop/interface/", []string{"font-name"}, "tag"}, false},
		{[]any{"/org/gnome/desktop/input-sources/sources"}, false},
	} {
		if got := touchesInputSources(&dbus.Signal{Body: tc.body}); got != tc.want {
			t.Errorf("touchesInputSources(%v) = %v", tc.body, got)
		}
	}
}

// fakeGsettings puts a gsettings first on PATH that prints the input
// sources from the file sources and the most recent ones from the file mru
// in dir, which a test may rewrite, or fails as gsettings does without the
// schema.
func fakeGsettings(t *testing.T, sources, mru string) (dir string) {
	t.Helper()
	dir = t.TempDir()
	script := "#!/bin/sh\n" +
		"[ \"$1 $2\" = \"get " + inputSources + "\" ] || { echo \"No such schema\" >&2; exit 1; }\n" +
		"case $3 in sources) cat " + dir + "/sources ;; mru-sources) cat " + dir + "/mru ;; *) exit 1 ;; esac\n"
	if err := os.WriteFile(filepath.Join(dir, "gsettings"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	setSources(t, dir, sources, mru)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func setSources(t *testing.T, dir, sources, mru string) {
	t.Helper()
	for name, v := range map[string]string{"sources": sources, "mru": mru} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGnomeSource(t *testing.T) {
	fakeGsettings(t, "[('xkb', 'us'), ('xkb', 'ru')]", "[('xkb', 'ru'), ('xkb', 'us')]")
	if got, err := gnomeSource(); err != nil || got != "ru" {
		t.Errorf("gnomeSource = %q, %v; want ru", got, err)
	}
	if _, err := gsettings("org.example.nothing", "key"); err == nil || !strings.Contains(err.Error(), "No such schema") {
		t.Errorf("gsettings of a missing schema: %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := gnomeSource(); err == nil || !strings.Contains(err.Error(), "gsettings get") {
		t.Errorf("gnomeSource without gsettings: %v", err)
	}
}

func TestGnomeSourceWithoutRecentOnes(t *testing.T) {
	fakeGsettings(t, "[('xkb', 'de')]", "@a(ss) []")
	if got, err := gnomeSource(); err != nil || got != "de" {
		t.Errorf("gnomeSource = %q, %v; want de", got, err)
	}
}

// TestGnomeSourceWithoutRecentOnesToRead: a gsettings that knows the
// sources but fails on the recent ones gives no source.
func TestGnomeSourceWithoutRecentOnesToRead(t *testing.T) {
	dir := fakeGsettings(t, "[('xkb', 'de')]", "")
	os.Remove(filepath.Join(dir, "mru"))
	if got, err := gnomeSource(); err == nil {
		t.Errorf("gnomeSource = %q, want an error", got)
	}
}

// testKeyboard is a keyboard whose source is whatever *source holds, read
// from the xkb package's test layouts.
func testKeyboard(source *string, err *error) *keyboard {
	return &keyboard{
		source: func() (string, error) { return *source, *err },
		load: func(l, v string) (xkb.Layout, error) {
			return xkb.Load([]string{"../xkb/testdata"}, l, v)
		},
		log: slog.New(slog.DiscardHandler),
	}
}

func TestKeyboardFollowsTheSource(t *testing.T) {
	source, serr := "ru", error(nil)
	k := testKeyboard(&source, &serr)
	k.update()
	if !k.lacks('o') || !k.lacks('A') || k.lacks('1') || k.lacks('!') || k.lacks(' ') {
		t.Error("ru: lacks gets the ASCII the layout lacks or has wrong")
	}
	if k.lacks('日') || k.lacks('\n') {
		t.Error("ru: lacks claims to know of a character outside printable ASCII")
	}

	notes := make(chan *dbus.Signal, 2)
	done := make(chan struct{})
	go func() { k.follow(notes); close(done) }()
	source = "us"
	notes <- &dbus.Signal{Body: []any{"/org/gnome/desktop/interface/font-name", []string{""}, ""}}
	notes <- &dbus.Signal{Body: []any{"/org/gnome/desktop/input-sources/mru-sources", []string{""}, ""}}
	close(notes)
	<-done
	if k.lacks('o') || k.lacks('~') {
		t.Error("us: lacks a character after the source changed")
	}

	// A layout that cannot be read, or none named, is not known, and then
	// every ASCII character is taken to type.
	for _, tc := range []struct {
		source string
		err    error
	}{{"ru", nil}, {"nosuchlayout", nil}, {"ru", nil}, {"", nil}, {"ru", nil}, {"", errors.New("no gsettings")}} {
		source, serr = tc.source, tc.err
		k.update()
		if got, want := k.lacks('o'), tc.source == "ru"; got != want {
			t.Errorf("%q, %v: lacks o %v, want %v", tc.source, tc.err, got, want)
		}
	}
	k.close()
	k.stop = func() { source = "stopped" }
	k.close()
	if source != "stopped" {
		t.Error("close did not stop following dconf")
	}
}

func TestTypable(t *testing.T) {
	source, serr := "ru", error(nil)
	k := testKeyboard(&source, &serr)
	k.update()
	for _, tc := range []struct {
		d    *Desktop
		text string
		want bool
	}{
		{&Desktop{in: &input{}}, "ok!\n", true},
		{&Desktop{in: &input{}}, "é", false},
		{&Desktop{in: &input{lacks: k.lacks}}, "1!.\n", true},
		{&Desktop{in: &input{lacks: k.lacks}}, "ok", false},
		{&Desktop{in: &input{lacks: k.lacks}, anyKeysym: true}, "ok日本", true},
	} {
		if got := tc.d.typable(tc.text); got != tc.want {
			t.Errorf("typable(%q) with any keysym %v, a layout %v = %v", tc.text, tc.d.anyKeysym, tc.d.in.lacks != nil, got)
		}
	}
}

// TestFollowKeyboard reads the layout through gsettings, and again when
// dconf announces a change to the input sources on the session bus.
func TestFollowKeyboard(t *testing.T) {
	conn := privateBus(t)
	dir := fakeGsettings(t, "[('xkb', 'us'), ('xkb', 'ru')]", "@a(ss) []")
	t.Setenv("XKB_CONFIG_ROOT", "../xkb/testdata")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	k := followKeyboard(newSignals(conn), slog.New(slog.DiscardHandler))
	defer k.close()
	if k.lacks('o') {
		t.Fatal("us: lacks o")
	}
	setSources(t, dir, "[('xkb', 'us'), ('xkb', 'ru')]", "[('xkb', 'ru'), ('xkb', 'us')]")
	other, err := sessionBus()
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.Emit(dconfPath, dconfWriter+".Notify", "/org/gnome/desktop/input-sources/mru-sources", []string{""}, "tag"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); !k.lacks('o'); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the keyboard did not follow the switch to ru")
		}
	}
}

// TestFollowKeyboardWithoutABus: a keyboard whose dconf changes cannot be
// watched still reads the layout once.
func TestFollowKeyboardWithoutABus(t *testing.T) {
	conn := privateBus(t)
	fakeGsettings(t, "[('xkb', 'ru')]", "@a(ss) []")
	t.Setenv("XKB_CONFIG_ROOT", "../xkb/testdata")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	sigs := newSignals(conn)
	conn.Close()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		sigs.mu.Lock()
		closed := sigs.closed
		sigs.mu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the signals never saw the bus close")
		}
	}
	k := followKeyboard(sigs, slog.New(slog.DiscardHandler))
	defer k.close()
	if !k.lacks('o') || k.stop != nil {
		t.Errorf("ru without a bus: lacks o %v, following %v", k.lacks('o'), k.stop != nil)
	}
}

func TestTypesAnyKeysym(t *testing.T) {
	conn := privateBus(t)
	if typesAnyKeysym(conn) {
		t.Error("a bus without KWin types any keysym")
	}
	if reply, err := conn.RequestName("org.kde.KWin", dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("RequestName: %v, %v", reply, err)
	}
	if !typesAnyKeysym(conn) {
		t.Error("KWin's bus does not type any keysym")
	}
}
