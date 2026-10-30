package xkb

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var testDirs = []string{"testdata/symbols-not-here", "testdata"}

// missing lists the printable ASCII characters l does not type.
func missing(l Layout) string {
	var b strings.Builder
	for r := rune(0x20); r < 0x7f; r++ {
		if !l.Has(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// level is the level l types sym on, or -1 for a keysym it lacks.
func (l Layout) level(sym string) int {
	if n, ok := l.levels[sym]; ok {
		return n
	}
	return -1
}

func load(t *testing.T, id string) Layout {
	t.Helper()
	layout, variant := ParseSource(id)
	l, err := Load(testDirs, layout, variant)
	if err != nil {
		t.Fatalf("Load %s: %v", id, err)
	}
	return l
}

func TestLatinLayoutTypesAllOfASCII(t *testing.T) {
	l := load(t, "us")
	if l.Name != "English (US)" || !l.Latin() || missing(l) != "" {
		t.Errorf("us: %q, Latin %v, missing %q", l.Name, l.Latin(), missing(l))
	}
	if !l.Has('\n') || !l.Has('\t') || l.Has('é') || l.Has(0x7f) {
		t.Error("us: Has gets the shared keys or a non-ASCII character wrong")
	}
	// Dead keys take the plain characters' place on the first two levels.
	// AltGr brings some back on the third, and mutter types those, but
	// not the ones on the fourth, Shift and AltGr.
	if l := load(t, "us+intl"); l.Name != "English (US, intl., with dead keys)" || missing(l) != `"^~` {
		t.Errorf("us+intl: %q missing %q", l.Name, missing(l))
	}
}

func TestCyrillicLayoutHasNoLatin(t *testing.T) {
	l := load(t, "ru")
	if l.Name != "Russian" || l.Latin() {
		t.Errorf("ru: %q, Latin %v", l.Name, l.Latin())
	}
	if !l.Has('1') || !l.Has('!') || !l.Has('.') || l.Has('a') || l.Has('#') {
		t.Errorf("ru: missing %q; want digits, ! and . but neither a nor # (its 3 types numerosign)", missing(l))
	}
	// A section's own name wins over the one its include gives.
	if l := load(t, "ru(common)"); l.Name != "Russian (common)" {
		t.Errorf("ru(common) is named %q", l.Name)
	}
}

func TestMerging(t *testing.T) {
	// Augmenting fills only keys without symbols, and so does a | in an
	// include.
	for _, id := range []string{"ru+under", "ru+piped"} {
		under := load(t, id)
		if under.Latin() || !under.Has('w') || under.Has('q') || under.Has('a') || under.Has('v') {
			t.Errorf("%s: missing %q", id, missing(under))
		}
	}
	// NoSymbol and any leave a level as it was.
	if kept := load(t, "ru+kept"); !kept.Latin() || kept.level("Cyrillic_ef") != 2 {
		t.Errorf("ru(kept): missing %q, Cyrillic_ef on %d", missing(kept), kept.level("Cyrillic_ef"))
	}
	// A level-three key over its include keeps its letters.
	if l := load(t, "latin(type3)"); !l.Latin() || l.level("Greek_OMEGA") != 3 || l.level("backslash") != 2 {
		t.Errorf("latin(type3): missing %q", missing(l))
	}
}

func TestKeyForms(t *testing.T) {
	l := load(t, "us+forms")
	if l.Name != "Forms" {
		t.Errorf("named %q, want the first group's name", l.Name)
	}
	var got []string
	for r := 'a'; r <= 'z'; r++ {
		if l.Has(r) {
			got = append(got, string(r))
		}
	}
	// a by U0061, s by its Unicode keysym, f from a level of several, g
	// beside other fields, h kept over an augment, j replaced. d is only
	// in the second group, and the second group's include is not the
	// first group's.
	if want := []string{"a", "f", "g", "h", "j", "s"}; !slices.Equal(got, want) {
		t.Errorf("letters %v, want %v", got, want)
	}
	if !l.Has('A') || !l.Has('S') || l.level("Cyrillic_er") >= 0 || l.level("Cyrillic_ve") != 0 || l.level("uFFFF") != 1 {
		t.Errorf("forms keysyms: %v", l.levels)
	}
}

// TestMutterPicks: a keysym comes from the key of the lowest code that has
// it, on the lowest level there, as mutter looks it up, though that level
// is one mutter does not type.
func TestMutterPicks(t *testing.T) {
	l := load(t, "us+order")
	if l.Has('q') || !l.Has('w') || !l.Has('e') || l.level("e") != 2 || !l.Has('1') {
		t.Errorf("us(order): missing %q, e on %d", missing(l), l.level("e"))
	}
	// A key that has no code is on no keyboard, and an alias has its
	// key's code.
	if l.level("Greek_alpha") >= 0 || l.level("Greek_beta") != 0 {
		t.Errorf("us(order): Greek_alpha on %d, Greek_beta on %d", l.level("Greek_alpha"), l.level("Greek_beta"))
	}
}

func TestIncludeFromADirectory(t *testing.T) {
	if l := load(t, "us+vendor"); !l.Has('z') || l.Has('a') {
		t.Errorf("us(vendor): missing %q, want only z", missing(l))
	}
}

// TestSystemLayouts reads every layout of the xkeyboard-config installed
// here, if there is one, as a desktop could name any of them.
func TestSystemLayouts(t *testing.T) {
	root := "/usr/share/X11/xkb"
	ents, err := os.ReadDir(filepath.Join(root, "symbols"))
	if err != nil {
		t.Skipf("no xkeyboard-config here: %v", err)
	}
	n := 0
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(root, "symbols", e.Name()))
		if e.IsDir() || err != nil || !strings.Contains(string(b), "xkb_symbols") {
			continue
		}
		secs, err := parse(string(b))
		if err != nil {
			t.Errorf("symbols/%s: %v", e.Name(), err)
			continue
		}
		for _, s := range secs {
			if _, err := Load([]string{root}, e.Name(), s.name); err != nil {
				t.Errorf("Load %s(%s): %v", e.Name(), s.name, err)
			}
			n++
		}
	}
	t.Logf("%d layouts", n)
	for id, want := range map[string]struct {
		latin   bool
		missing string
	}{
		"us": {true, ""}, "us+dvorak": {true, ""},
		// German's ^ and ` are dead keys, which type nothing alone.
		"de": {true, "^`"},
		// French has $, < and > on the fourth level of keys with lower
		// codes than the keys it has them on first.
		"fr":      {true, "$<>"},
		"us+intl": {true, `"^~`},
		"ru":      {false, "#$&'<>@ABCDEFGHIJKLMNOPQRSTUVWXYZ[]^`abcdefghijklmnopqrstuvwxyz{}~"},
		"gr":      {false, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"},
	} {
		layout, variant := ParseSource(id)
		l, err := Load([]string{root}, layout, variant)
		if err != nil || l.Latin() != want.latin || strings.TrimSpace(missing(l)) != want.missing {
			t.Errorf("%s: Latin %v, missing %q, %v; want %v, %q", id, l.Latin(), missing(l), err, want.latin, want.missing)
		}
	}
}

func TestUserLayoutsComeFirst(t *testing.T) {
	l, err := Load([]string{"testdata/user", "testdata"}, "us", "")
	if err != nil || l.Name != "Mine" || !l.Has('a') || l.Has('b') {
		t.Errorf("Load = %q (%v), missing %q; want the user's own layout", l.Name, err, missing(l))
	}
}

func TestLoadErrors(t *testing.T) {
	for _, tc := range []struct{ layout, variant, want string }{
		{"nosuchlayout", "", "no symbols/nosuchlayout"},
		{"us", "nosuchvariant", `no variant "nosuchvariant"`},
		{"broken", "loop", "nest deeper"},
		{"broken", "missing", "no symbols/nosuchfile"},
		{"broken", "badname", "bad symbols file name"},
		{"broken", "novariant", `no variant "nosuchvariant"`},
		{"", "", "bad symbols file name"},
	} {
		if _, err := Load(testDirs, tc.layout, tc.variant); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Load(%q, %q) = %v, want %q", tc.layout, tc.variant, err, tc.want)
		}
	}
	// A directory where a file should be is not a layout either.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "symbols", "us"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load([]string{dir}, "us", ""); err == nil {
		t.Error("Load read a directory as a layout")
	}
}

func TestParseErrors(t *testing.T) {
	for src, want := range map[string]string{
		`/* open`:                                                       "unterminated comment",
		`xkb_symbols "a`:                                                "unterminated string",
		`xkb_symbols "a" { key <AC01 { [ a ] }`:                         "unterminated key name",
		`xkb_symbols { };`:                                              "without a name",
		`xkb_symbols "a" ( };`:                                          `want "{"`,
		`xkb_symbols "a" { key <AC01> { [ a ] };`:                       "no closing brace",
		`xkb_symbols "a" { key AC01 { [ a ] }; };`:                      "key without a name",
		`xkb_symbols "a" { key <AC01> [ a ] };`:                         `want "{"`,
		`xkb_symbols "a" { key <AC01> { [ a `:                           "no closing bracket",
		`xkb_symbols "a" { key <AC01> { [ a ]`:                          "no closing brace",
		`xkb_symbols "a" { key <AC01> { [ S( ] }; };`:                   `no ")"`,
		`xkb_symbols "a" { key <AC01> { t[G1 = "x" }; };`:               `want "]"`,
		`xkb_symbols "a" { name[Group1 = "x"; };`:                       `want "]"`,
		`xkb_symbols "a" { key <AC01> { actions[Group1] = [ S(x) }; };`: "no closing bracket",
	} {
		if _, err := parse(src); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parse(%q) = %v, want %q", src, err, want)
		}
	}
	// Text after the last section, and a file with none, are no layouts
	// but no errors.
	for _, src := range []string{``, `// nothing`, `xkb_symbols "a" { }; default partial`} {
		if _, err := parse(src); err != nil {
			t.Errorf("parse(%q): %v", src, err)
		}
	}
}

func TestParseKeycodes(t *testing.T) {
	codes, err := parseKeycodes(`xkb_keycodes "a" { <A> = 1; };
default xkb_keycodes "b" { minimum = 8; ; <AC01> = 38; <BAD> = x; alias <AL> = <AC01>; alias <GONE> = <NOPE>; alias <ODD> <AC01>; };`)
	want := map[string]int{"<AC01>": 38, "<AL>": 38}
	if err != nil || !maps.Equal(codes, want) {
		t.Errorf("default section: %v, %v; want %v", codes, err, want)
	}
	if codes, err := parseKeycodes(`xkb_keycodes "a" { <A> = 1; }; xkb_keycodes "b" { <B> = 2; };`); err != nil || codes["<A>"] != 1 || len(codes) != 1 {
		t.Errorf("no default: %v, %v; want the first section", codes, err)
	}
	for src, want := range map[string]string{
		``:                         "no xkb_keycodes",
		`xkb_symbols "a" { };`:     "no xkb_keycodes",
		`xkb_keycodes "a" { <A> `:  "no closing brace",
		`xkb_keycodes { };`:        "without a name",
		`xkb_keycodes "a" ( };`:    `want "{"`,
		`xkb_keycodes "a" { "x" ;`: "no closing brace",
		`/* open`:                  "unterminated comment",
	} {
		if _, err := parseKeycodes(src); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseKeycodes(%q) = %v, want %q", src, err, want)
		}
	}
}

func TestLoadWithoutKeycodes(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ evdev, want string }{
		{"", "no keycodes/evdev"},
		{"xkb_keycodes", "keycodes/evdev: xkb_keycodes without a name"},
		{"dir", "is a directory"},
	} {
		os.RemoveAll(filepath.Join(dir, "keycodes"))
		if err := os.MkdirAll(filepath.Join(dir, "keycodes"), 0o755); err != nil {
			t.Fatal(err)
		}
		var err error
		switch tc.evdev {
		case "":
		case "dir":
			err = os.Mkdir(filepath.Join(dir, "keycodes", "evdev"), 0o755)
		default:
			err = os.WriteFile(filepath.Join(dir, "keycodes", "evdev"), []byte(tc.evdev), 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Load([]string{dir, "testdata/symbols-not-here"}, "us", ""); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("keycodes %q: Load = %v, want %q", tc.evdev, err, tc.want)
		}
	}
}

func TestParseSource(t *testing.T) {
	for id, want := range map[string][2]string{
		"us":           {"us", ""},
		"us+intl":      {"us", "intl"},
		"latin(type3)": {"latin", "type3"},
		"ru+phonetic":  {"ru", "phonetic"},
	} {
		if l, v := ParseSource(id); l != want[0] || v != want[1] {
			t.Errorf("ParseSource(%q) = %q, %q", id, l, v)
		}
	}
}

func TestSplitInclude(t *testing.T) {
	got := splitInclude("pc+us(basic):2|inet(evdev)+ru:x|latin", modeAugment)
	want := []includePart{
		{"pc", 1, modeAugment}, {"us(basic)", 2, modeOverride}, {"inet(evdev)", 1, modeAugment},
		{"latin", 1, modeAugment},
	}
	if !slices.Equal(got, want) {
		t.Errorf("splitInclude = %v, want %v", got, want)
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"U0061": "a", "u007e": "asciitilde", "0x61": "a", "0x100005b": "bracketleft",
		"U0444": "U0444", "0x20": "0x20", "U00zz": "U00zz", "0xzz": "0xzz", "U": "U", "a": "a",
	} {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDirs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/config")
	t.Setenv("HOME", "/home/u")
	t.Setenv("XKB_CONFIG_ROOT", "/opt/xkb")
	if got, want := Dirs(), []string{"/config/xkb", "/home/u/.xkb", "/etc/xkb", "/opt/xkb"}; !slices.Equal(got, want) {
		t.Errorf("Dirs = %v, want %v", got, want)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XKB_CONFIG_ROOT", "")
	if got, want := Dirs(), []string{"/home/u/.config/xkb", "/home/u/.xkb", "/etc/xkb", "/usr/share/X11/xkb"}; !slices.Equal(got, want) {
		t.Errorf("Dirs = %v, want %v", got, want)
	}
}
