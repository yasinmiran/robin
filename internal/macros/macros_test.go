package macros

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasinmiran/robin/internal/protocol"
)

func chord(keys ...string) protocol.MacroStep {
	var st protocol.MacroStep
	for i := 0; i+1 < len(keys); i += 2 {
		st.Keys = append(st.Keys, protocol.MacroKey{Key: keys[i], Code: keys[i+1]})
	}
	return st
}

func macro(id, name string, steps ...protocol.MacroStep) protocol.Macro {
	if len(steps) == 0 {
		steps = []protocol.MacroStep{chord("Control", "ControlLeft", "l", "KeyL")}
	}
	return protocol.Macro{ID: id, Name: name, Steps: steps}
}

func put(m protocol.Macro) *protocol.MacroChange { return &protocol.MacroChange{Put: &m} }

func ids(m *protocol.Macros) string {
	var out []string
	for _, x := range m.List {
		out = append(out, x.ID)
	}
	return strings.Join(out, ",")
}

func mustChange(t *testing.T, s *Store, c *protocol.MacroChange) *protocol.Macros {
	t.Helper()
	got := s.Change(c)
	if got.Error != "" {
		t.Fatalf("change %+v: %s", c, got.Error)
	}
	return got
}

func TestPathIsInTheUserConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	p, err := Path()
	if err != nil || p != filepath.Join(dir, "robin", "macros.json") {
		t.Fatalf("Path() = %q, %v", p, err)
	}
	if got := New("box").Load(); got.Error != "" || got.Host != "box" || len(got.List) != 0 {
		t.Errorf("no file yet: %+v", got)
	}
}

func TestWithoutAConfigDirNothingIsKept(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	s := New("box")
	if got := s.Load(); !strings.HasPrefix(got.Error, "macros are not kept: ") {
		t.Errorf("Load: %+v", got)
	}
	if got := s.Change(put(macro("a", "A"))); !strings.HasPrefix(got.Error, "macros are not kept: ") || len(got.List) != 0 {
		t.Errorf("Change: %+v", got)
	}
}

func TestAChangeIsWrittenByRenameAndPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "robin")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "macros.json")
	s := Open(path, "alice@box")
	m := macro("m1", "Lock screen", chord("Meta", "MetaLeft", "l", "KeyL"),
		protocol.MacroStep{Pause: 500}, protocol.MacroStep{Text: "hunter2\n"})
	got := mustChange(t, s, put(m))
	if got.Host != "alice@box" || ids(got) != "m1" {
		t.Fatalf("after put: %+v", got)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := st.Mode(); mode != 0o600 {
		t.Errorf("file mode %v, want 0600", mode)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v, want 0700", st.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("dir holds %v, want macros.json alone", entries)
	}

	b, _ := os.ReadFile(path)
	var f struct {
		Version int              `json:"version"`
		Macros  []protocol.Macro `json:"macros"`
	}
	if err := json.Unmarshal(b, &f); err != nil || f.Version != 1 || len(f.Macros) != 1 || f.Macros[0].Steps[2].Text != "hunter2\n" {
		t.Errorf("file %s: %+v, %v", b, f, err)
	}

	// The rename replaced the file, so a reader that held the old one still
	// reads it whole.
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	m.Name = "Lock"
	mustChange(t, s, put(m))
	was, _ := os.ReadFile(old.Name())
	held := new(bytes.Buffer)
	held.ReadFrom(old)
	if !bytes.Equal(held.Bytes(), b) || bytes.Equal(was, b) {
		t.Errorf("the open file changed under its reader, or the path still holds the old macros")
	}
	if got := Open(path, "alice@box").Load(); got.Error != "" || got.List[0].Name != "Lock" {
		t.Errorf("a new session reads %+v", got)
	}
}

func TestDirectoryIsMadePrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "robin", "macros.json")
	mustChange(t, Open(path, "box"), put(macro("a", "A")))
	if st, err := os.Stat(filepath.Dir(path)); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("made dir: %v, %v", st.Mode(), err)
	}
}

func TestPutReplacesAndDeleteRemoves(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "macros.json"), "box")
	mustChange(t, s, put(macro("a", "A")))
	mustChange(t, s, put(macro("b", "B")))
	got := mustChange(t, s, put(macro("a", "A again")))
	if ids(got) != "a,b" || got.List[0].Name != "A again" {
		t.Errorf("replace kept the order and took the new name: %+v", got)
	}
	if got := mustChange(t, s, &protocol.MacroChange{Delete: "a"}); ids(got) != "b" {
		t.Errorf("after delete: %+v", got)
	}
	if got := s.Change(&protocol.MacroChange{Delete: "a"}); !strings.Contains(got.Error, `no macro "a"`) || ids(got) != "b" {
		t.Errorf("delete twice: %+v", got)
	}
}

func TestAChangeIsOneOfPutAndDelete(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "macros.json"), "box")
	m := macro("a", "A")
	for _, c := range []*protocol.MacroChange{nil, {}, {Put: &m, Delete: "a"}} {
		if got := s.Change(c); got.Error != "a change puts or deletes one macro" {
			t.Errorf("%+v: %q", c, got.Error)
		}
	}
	if _, err := os.Stat(s.path); !os.IsNotExist(err) {
		t.Errorf("a refused change wrote the file: %v", err)
	}
}

func TestAMacroPinnedToAHostShowsOnlyThere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "macros.json")
	box, lab := Open(path, "alice@box"), Open(path, "lab")
	pinned := macro("p", "Pinned")
	pinned.Host = "alice@box"
	mustChange(t, box, put(pinned))
	mustChange(t, lab, put(macro("u", "Everywhere")))

	if got := box.Load(); ids(got) != "p,u" {
		t.Errorf("on the host it was made on: %+v", got)
	}
	if got := lab.Load(); ids(got) != "u" {
		t.Errorf("on another host: %+v", got)
	}
	// box spelled another way is another host.
	if got := Open(path, "box").Load(); ids(got) != "u" {
		t.Errorf("the host as typed: %+v", got)
	}

	if got := lab.Change(&protocol.MacroChange{Delete: "p"}); got.Error == "" {
		t.Errorf("lab deleted box's macro")
	}
	stolen := macro("p", "Mine now")
	if got := lab.Change(put(stolen)); !strings.Contains(got.Error, "taken by a macro of another host") {
		t.Errorf("lab replaced box's macro: %+v", got)
	}
	elsewhere := macro("q", "Q")
	elsewhere.Host = "alice@box"
	if got := lab.Change(put(elsewhere)); !strings.Contains(got.Error, `pinned to "lab" only`) {
		t.Errorf("lab pinned a macro to box: %+v", got)
	}
	if got := box.Load(); ids(got) != "p,u" || got.List[0].Name != "Pinned" {
		t.Errorf("box's macros after lab's tries: %+v", got)
	}
}

func TestAChangeKeepsWhatAnotherSessionWrote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "macros.json")
	one, two := Open(path, "box"), Open(path, "lab")
	one.Load()
	two.Load()
	mustChange(t, one, put(macro("a", "A")))
	if got := mustChange(t, two, put(macro("b", "B"))); ids(got) != "a,b" {
		t.Errorf("the second session lost the first one's macro: %+v", got)
	}
}

func TestAFileThatDoesNotReadIsLeftAsItIs(t *testing.T) {
	valid, _ := encode([]protocol.Macro{macro("a", "A")})
	twice, _ := encode([]protocol.Macro{macro("a", "A"), macro("a", "B")})
	tooMany := make([]protocol.Macro, protocol.MaxMacros+1)
	for i := range tooMany {
		tooMany[i] = macro("a", "A")
	}
	many, _ := json.Marshal(file{Version: 1, Macros: tooMany})
	for _, c := range []struct {
		name, body, want string
	}{
		{"not JSON", "{macros", "invalid character"},
		{"unknown field", `{"version":1,"macros":[],"extra":true}`, `unknown field "extra"`},
		{"unknown step field", `{"version":1,"macros":[{"id":"a","name":"A","steps":[{"wait":5}]}]}`, `unknown field "wait"`},
		{"newer version", `{"version":2,"macros":[]}`, "version 2, where this Robin reads 1"},
		{"no version", `{"macros":[]}`, "version 0"},
		{"two values", string(valid) + "{}", "more than one JSON value"},
		{"same ID twice", string(twice), `macro ID "a" twice`},
		{"a bad macro", `{"version":1,"macros":[{"id":"a","name":"","steps":[{"pause":5}]}]}`, "a macro's name is 1 to 64 bytes"},
		{"too many macros", string(many), "101 macros, over the 100"},
		{"too big", `{"version":1,"macros":[]}` + strings.Repeat(" ", protocol.MaxMacrosBytes), "is over 256 KiB"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "macros.json")
			if err := os.WriteFile(path, []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			s := Open(path, "box")
			got := s.Load()
			if !strings.Contains(got.Error, c.want) || !strings.Contains(got.Error, path) || !strings.Contains(got.Error, "leaves it as it is") || len(got.List) != 0 {
				t.Errorf("Load: %+v, want an error with %q and the path", got, c.want)
			}
			if got := s.Change(put(macro("z", "Z"))); got.Error == "" {
				t.Errorf("a change went through")
			}
			if b, _ := os.ReadFile(path); string(b) != c.body {
				t.Errorf("the file changed")
			}
			if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
				t.Errorf("dir holds %v", entries)
			}
		})
	}
}

func TestTheFileCapHoldsForWrites(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "macros.json"), "box")
	big := protocol.MacroStep{Text: strings.Repeat("x", protocol.MaxMacroTextBytes)}
	steps := make([]protocol.MacroStep, 15)
	for i := range steps {
		steps[i] = big
	}
	mustChange(t, s, put(macro("a", "A", steps...)))
	got := s.Change(put(macro("b", "B", steps...)))
	if !strings.Contains(got.Error, "over the 256 KiB the file holds") || ids(got) != "a" {
		t.Errorf("a second big macro: %+v", got)
	}

	s = Open(filepath.Join(t.TempDir(), "macros.json"), "box")
	for i := range protocol.MaxMacros {
		mustChange(t, s, put(macro(strings.Repeat("m", 1+i/26)+string(rune('a'+i%26)), "M")))
	}
	if got := s.Change(put(macro("one-more", "M"))); !strings.Contains(got.Error, "100 macros at most") {
		t.Errorf("macro 101: %q", got.Error)
	}
	if got := mustChange(t, s, put(macro("ma", "Replaced"))); len(got.List) != protocol.MaxMacros {
		t.Errorf("replacing one at the cap: %d", len(got.List))
	}
}

func TestCheck(t *testing.T) {
	long := func(n int) string { return strings.Repeat("k", n) }
	ok := chord("Control", "ControlLeft", "Alt", "AltLeft", "Delete", "Delete")
	for _, c := range []struct {
		name string
		m    protocol.Macro
		want string // empty: valid
	}{
		{"a chord", macro("a", "Ctrl+Alt+Del", ok), ""},
		{"every kind", macro("A-z_9", "Mix", ok, protocol.MacroStep{Text: "日本語\n\t"}, protocol.MacroStep{Pause: 1}), ""},
		{"at every cap", atTheCaps(), ""},
		{"a key without a name", macro("a", "A", protocol.MacroStep{Keys: []protocol.MacroKey{{Code: "KeyA"}}}), "a key is 1 to 32 bytes"},
		{"a key with no code", macro("a", "A", protocol.MacroStep{Keys: []protocol.MacroKey{{Key: "a"}}}), ""},
		{"a long key", macro("a", "A", chord(long(33), "KeyA")), "a key is 1 to 32 bytes"},
		{"a key at its cap", macro("a", "A", chord(long(32), long(32))), ""},
		{"a control character key", macro("a", "A", chord("\x1b", "Escape")), "a key is"},
		{"a long code", macro("a", "A", chord("a", long(33))), "a key's code is up to 32"},
		{"a code with punctuation", macro("a", "A", chord("a", "Key-A")), "a key's code is up to 32"},
		{"a code outside ASCII", macro("a", "A", chord("a", "Keyé")), "a key's code"},
		{"too many keys", macro("a", "A", protocol.MacroStep{Keys: make([]protocol.MacroKey, protocol.MaxMacroChordKeys+1)}), "a chord takes 8 keys at most"},
		{"no ID", macro("", "A"), `macro ID "" is not 1 to 32`},
		{"a long ID", macro(long(33), "A"), "macro ID"},
		{"an ID with a slash", macro("../x", "A"), "macro ID"},
		{"an ID outside ASCII", macro("é", "A"), "macro ID"},
		{"no name", macro("a", ""), "a macro's name is 1 to 64 bytes"},
		{"a blank name", macro("a", "  "), "a macro's name"},
		{"a long name", macro("a", long(65)), "a macro's name"},
		{"a name with a newline", macro("a", "A\nB"), "a macro's name"},
		{"a name that is not UTF-8", macro("a", "\xff"), "a macro's name"},
		{"a host with a control character", protocol.Macro{ID: "a", Name: "A", Host: "box\x07", Steps: []protocol.MacroStep{ok}}, "a host is up to 255"},
		{"a long host", protocol.Macro{ID: "a", Name: "A", Host: long(256), Steps: []protocol.MacroStep{ok}}, "a host"},
		{"no steps", protocol.Macro{ID: "a", Name: "A"}, "0 steps, where a macro takes 1 to 100"},
		{"too many steps", macro("a", "A", make([]protocol.MacroStep, 101)...), "101 steps"},
		{"an empty step", macro("a", "A", protocol.MacroStep{}), "step 1: a step is one chord, one run of text or one pause"},
		{"a step of two kinds", macro("a", "A", ok, protocol.MacroStep{Text: "x", Pause: 5}), "step 2: a step is one"},
		{"long text", macro("a", "A", protocol.MacroStep{Text: long(protocol.MaxMacroTextBytes + 1)}), "text is up to 16 KiB of UTF-8"},
		{"text that is not UTF-8", macro("a", "A", protocol.MacroStep{Text: "\xc3"}), "text is up to"},
		{"a negative pause", macro("a", "A", protocol.MacroStep{Pause: -1}), "a pause is 1 to 60000 ms"},
		{"a long pause", macro("a", "A", protocol.MacroStep{Pause: protocol.MaxMacroPauseMs + 1}), "a pause is"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := Check(c.m)
			switch {
			case c.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Errorf("Check = %v, want %q", err, c.want)
			}
		})
	}
}

// atTheCaps is a macro that reaches every bound and stays within it.
func atTheCaps() protocol.Macro {
	long := func(n int) string { return strings.Repeat("k", n) }
	keys := make([]protocol.MacroKey, protocol.MaxMacroChordKeys)
	for i := range keys {
		keys[i] = protocol.MacroKey{Key: long(protocol.MaxMacroKeyBytes), Code: long(protocol.MaxMacroKeyBytes)}
	}
	steps := []protocol.MacroStep{{Keys: keys}, {Text: long(protocol.MaxMacroTextBytes)}}
	for len(steps) < protocol.MaxMacroSteps {
		steps = append(steps, protocol.MacroStep{Pause: protocol.MaxMacroPauseMs})
	}
	return protocol.Macro{
		ID:    long(protocol.MaxMacroIDBytes),
		Name:  strings.Repeat("é", protocol.MaxMacroNameBytes/2),
		Host:  long(protocol.MaxMacroHostBytes),
		Steps: steps,
	}
}
