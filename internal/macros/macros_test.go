package macros

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func ids(list []protocol.Macro) string {
	var out []string
	for _, x := range list {
		out = append(out, x.ID)
	}
	return strings.Join(out, ",")
}

func mustChange(t *testing.T, s *Store, c *protocol.MacroChange) []protocol.Macro {
	t.Helper()
	got, err := s.Change(c)
	if err != nil {
		t.Fatalf("change %+v: %v", c, err)
	}
	return got
}

func mustLoad(t *testing.T, s *Store) []protocol.Macro {
	t.Helper()
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return got
}

// entries names what dir holds besides the lock file.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		if e.Name() != "macros.json.lock" {
			names = append(names, e.Name())
		}
	}
	return names
}

func TestPathIsInTheUserConfigDir(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"HOME", "XDG_CONFIG_HOME", "AppData"} {
		t.Setenv(v, dir)
	}
	config, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Path()
	if err != nil || p != filepath.Join(config, "robin", "macros.json") || !strings.HasPrefix(p, dir) {
		t.Fatalf("Path() = %q, %v; want robin/macros.json in %s", p, err, config)
	}
	s := New(p, "box")
	if got, err := s.Load(); err != nil || len(got) != 0 || got == nil || s.Host() != "box" {
		t.Errorf("no file yet: %+v, %v", got, err)
	}
}

func TestAStoreWithoutAPathKeepsNothing(t *testing.T) {
	wd, _ := os.Getwd()
	s := New("", "box")
	if _, err := s.Load(); !errors.Is(err, errNoPath) {
		t.Errorf("Load: %v", err)
	}
	if got, err := s.Change(put(macro("a", "A"))); !errors.Is(err, errNoPath) || len(got) != 0 {
		t.Errorf("Change: %+v, %v", got, err)
	}
	if err := s.write(nil); !errors.Is(err, errNoPath) {
		t.Errorf("write: %v", err)
	}
	if left, _ := os.ReadDir(wd); slicesHasPrefix(left, ".macros-") {
		t.Errorf("a temp file in the working directory")
	}
}

func slicesHasPrefix(es []os.DirEntry, prefix string) bool {
	for _, e := range es {
		if strings.HasPrefix(e.Name(), prefix) {
			return true
		}
	}
	return false
}

func TestAChangeIsWrittenByRenameAndPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "robin")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "macros.json")
	s := New(path, "alice@box")
	m := macro("m1", "Lock screen", chord("Meta", "MetaLeft", "l", "KeyL"),
		protocol.MacroStep{Pause: 500}, protocol.MacroStep{Text: "hunter2\n"})
	if got := mustChange(t, s, put(m)); ids(got) != "m1" {
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
	if names := entries(t, dir); len(names) != 1 {
		t.Errorf("dir holds %v, want macros.json alone", names)
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
	if got := mustLoad(t, New(path, "alice@box")); got[0].Name != "Lock" {
		t.Errorf("a new session reads %+v", got)
	}
}

func TestDirectoryIsMadePrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "robin", "macros.json")
	mustChange(t, New(path, "box"), put(macro("a", "A")))
	if st, err := os.Stat(filepath.Dir(path)); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("made dir: %v, %v", st.Mode(), err)
	}
}

func TestPutReplacesAndDeleteRemoves(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "macros.json"), "box")
	mustChange(t, s, put(macro("a", "A")))
	mustChange(t, s, put(macro("b", "B")))
	got := mustChange(t, s, put(macro("a", "A again")))
	if ids(got) != "a,b" || got[0].Name != "A again" {
		t.Errorf("replace kept the order and took the new name: %+v", got)
	}
	if got := mustChange(t, s, &protocol.MacroChange{Delete: "a"}); ids(got) != "b" {
		t.Errorf("after delete: %+v", got)
	}
	if got, err := s.Change(&protocol.MacroChange{Delete: "a"}); err == nil || !strings.Contains(err.Error(), `no macro "a"`) || ids(got) != "b" {
		t.Errorf("delete twice: %+v, %v", got, err)
	}
}

func TestAChangeIsOneOfPutAndDelete(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "macros.json"), "box")
	m := macro("a", "A")
	for _, c := range []*protocol.MacroChange{nil, {}, {Put: &m, Delete: "a"}} {
		if got, err := s.Change(c); err == nil || err.Error() != "a change puts or deletes one macro" || got == nil {
			t.Errorf("%+v: %+v, %v", c, got, err)
		}
	}
	if _, err := os.Stat(s.path); !os.IsNotExist(err) {
		t.Errorf("a refused change wrote the file: %v", err)
	}
}

func TestAMacroPinnedToAHostShowsOnlyThere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "macros.json")
	box, lab := New(path, "alice@box"), New(path, "lab")
	pinned := macro("p", "Pinned")
	pinned.Host = "alice@box"
	mustChange(t, box, put(pinned))
	mustChange(t, lab, put(macro("u", "Everywhere")))

	if got := mustLoad(t, box); ids(got) != "p,u" {
		t.Errorf("on the host it was made on: %+v", got)
	}
	if got := mustLoad(t, lab); ids(got) != "u" {
		t.Errorf("on another host: %+v", got)
	}
	// box spelled another way is another host.
	if got := mustLoad(t, New(path, "box")); ids(got) != "u" {
		t.Errorf("the host as typed: %+v", got)
	}

	if _, err := lab.Change(&protocol.MacroChange{Delete: "p"}); err == nil {
		t.Errorf("lab deleted box's macro")
	}
	stolen := macro("p", "Mine now")
	if _, err := lab.Change(put(stolen)); err == nil || !strings.Contains(err.Error(), "taken by a macro of another host") {
		t.Errorf("lab replaced box's macro: %v", err)
	}
	elsewhere := macro("q", "Q")
	elsewhere.Host = "alice@box"
	if got, err := lab.Change(put(elsewhere)); err == nil || !strings.Contains(err.Error(), `pinned to "lab" only`) || ids(got) != "u" {
		t.Errorf("lab pinned a macro to box: %+v, %v", got, err)
	}
	if got := mustLoad(t, box); ids(got) != "p,u" || got[0].Name != "Pinned" {
		t.Errorf("box's macros after lab's tries: %+v", got)
	}
}

func TestAChangeKeepsWhatAnotherSessionWrote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "macros.json")
	one, two := New(path, "box"), New(path, "lab")
	mustLoad(t, one)
	mustLoad(t, two)
	mustChange(t, one, put(macro("a", "A")))
	if got := mustChange(t, two, put(macro("b", "B"))); ids(got) != "a,b" {
		t.Errorf("the second session lost the first one's macro: %+v", got)
	}
}

// holding starts a's change and returns once it holds the file, with the
// function that lets it write and the channel its result comes on.
func holding(a *Store, c *protocol.MacroChange) (release func(), done <-chan error) {
	in, out := make(chan struct{}), make(chan struct{})
	a.beforeWrite = func() {
		close(in)
		<-out
	}
	result := make(chan error, 1)
	go func() {
		_, err := a.Change(c)
		result <- err
	}()
	<-in
	return func() { close(out) }, result
}

func TestADeleteIsNotUndoneByAnotherSessionsChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "macros.json")
	mustChange(t, New(path, "box"), put(macro("x", "Secret", protocol.MacroStep{Text: "hunter2"})))
	a, b := New(path, "box"), New(path, "lab")
	release, aDone := holding(a, &protocol.MacroChange{Delete: "x"})

	bDone := make(chan error, 1)
	go func() {
		_, err := b.Change(put(macro("y", "Y")))
		bDone <- err
	}()
	var bErr error
	bFinished := false
	select {
	case bErr = <-bDone:
		bFinished = true
		t.Error("another session changed the file while this one held it")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	if err := <-aDone; err != nil {
		t.Fatalf("the delete: %v", err)
	}
	if !bFinished {
		bErr = <-bDone
	}
	if bErr != nil {
		t.Fatalf("the put: %v", bErr)
	}
	if got := mustLoad(t, New(path, "box")); ids(got) != "y" {
		t.Errorf("the file holds %q, want the put and not the deleted macro", ids(got))
	}
}

func TestAChangeWaitsForAnotherSessionWithinABound(t *testing.T) {
	was := lockWait
	lockWait = 50 * time.Millisecond
	t.Cleanup(func() { lockWait = was })
	path := filepath.Join(t.TempDir(), "macros.json")
	mustChange(t, New(path, "box"), put(macro("x", "X")))
	release, aDone := holding(New(path, "box"), put(macro("a", "A")))
	defer func() {
		release()
		<-aDone
	}()
	start := time.Now()
	got, err := New(path, "lab").Change(put(macro("b", "B")))
	if err == nil || !strings.Contains(err.Error(), "another Robin session is changing the macros") || ids(got) != "x" {
		t.Errorf("a change while another holds the file: %+v, %v", ids(got), err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("waited %v", took)
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
		{"too big", `{"version":1,"macros":[]}` + strings.Repeat(" ", protocol.MaxMacrosBytes), "(over 256 KiB)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "macros.json")
			if err := os.WriteFile(path, []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			s := New(path, "box")
			got, err := s.Load()
			if !errors.Is(err, ErrUnreadable) || !strings.Contains(err.Error(), c.want) || len(got) != 0 {
				t.Errorf("Load: %+v, %v; want ErrUnreadable with %q", got, err, c.want)
			}
			if want := "Robin can't read " + path + " ("; err == nil || !strings.HasPrefix(err.Error(), want) ||
				!strings.HasSuffix(err.Error(), "). It leaves the file alone and saves no macros until you fix or remove it") {
				t.Errorf("the page reads %q", err)
			}
			if _, err := s.Change(put(macro("z", "Z"))); !errors.Is(err, ErrUnreadable) {
				t.Errorf("a change went through: %v", err)
			}
			if b, _ := os.ReadFile(path); string(b) != c.body {
				t.Errorf("the file changed")
			}
			if names := entries(t, filepath.Dir(path)); len(names) != 1 {
				t.Errorf("dir holds %v", names)
			}
		})
	}
}

func TestTheFileCapHoldsForWrites(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "macros.json"), "box")
	big := protocol.MacroStep{Text: strings.Repeat("x", protocol.MaxMacroTextBytes)}
	steps := make([]protocol.MacroStep, 15)
	for i := range steps {
		steps[i] = big
	}
	mustChange(t, s, put(macro("a", "A", steps...)))
	if got, err := s.Change(put(macro("b", "B", steps...))); err == nil || !strings.Contains(err.Error(), "over the 256 KiB the file holds") || ids(got) != "a" {
		t.Errorf("a second big macro: %+v, %v", ids(got), err)
	}

	s = New(filepath.Join(t.TempDir(), "macros.json"), "box")
	for i := range protocol.MaxMacros {
		mustChange(t, s, put(macro(strings.Repeat("m", 1+i/26)+string(rune('a'+i%26)), "M")))
	}
	if _, err := s.Change(put(macro("one-more", "M"))); err == nil || !strings.Contains(err.Error(), "100 macros at most") {
		t.Errorf("macro 101: %v", err)
	}
	if got := mustChange(t, s, put(macro("ma", "Replaced"))); len(got) != protocol.MaxMacros {
		t.Errorf("replacing one at the cap: %d", len(got))
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
