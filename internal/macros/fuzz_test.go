package macros

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasinmiran/robin/internal/protocol"
)

// fileSeeds are macros files as Robin writes them and as a hand might
// leave them.
func fileSeeds(f *testing.F) [][]byte {
	f.Helper()
	b, err := encode([]protocol.Macro{
		macro("m1", "Lock"),
		{ID: "m2", Name: "Hi", Host: "box", Steps: []protocol.MacroStep{{Text: "hi\n"}, {Pause: 250}, chord("a", "KeyA")}},
	})
	if err != nil {
		f.Fatal(err)
	}
	full, err := encode([]protocol.Macro{atTheCaps()})
	if err != nil {
		f.Fatal(err)
	}
	return [][]byte{
		b, full,
		[]byte(`{"version":1,"macros":[]}`),
		[]byte(`{"version":2,"macros":[]}`),
		[]byte(`{"version":1,"macros":[],"extra":1}`),
		[]byte(`{"version":1,"macros":[]} {}`),
		[]byte(`{"version":1,"macros":[{"id":"a","name":"x","steps":[{"keys":[],"text":"t"}]}]}`),
		[]byte(`{"version":1,"macros":[{"id":"a","name":"x","steps":[{"pause":-1}]}]}`),
		[]byte(`{"version":1,"macros":[{"id":"a","name":"\u0007","steps":[{"text":"t"}]}]}`),
		[]byte(`{"version":1,"macros":[{"id":"a","name":"x","steps":[{"text":"t"}]},{"id":"a","name":"y","steps":[{"text":"t"}]}]}`),
		[]byte(`null`),
	}
}

// FuzzDecodeFile feeds the macros file's reader: a file it takes holds no
// more macros than Robin keeps, each one Check accepts under an ID of its
// own, and writing them back gives a file that reads the same.
func FuzzDecodeFile(f *testing.F) {
	for _, b := range fileSeeds(f) {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		all, err := decode(b)
		if err != nil {
			return
		}
		if len(all) > protocol.MaxMacros {
			t.Fatalf("took %d macros", len(all))
		}
		seen := map[string]bool{}
		for _, m := range all {
			if err := Check(m); err != nil {
				t.Fatalf("took a macro Check refuses: %v", err)
			}
			if seen[m.ID] {
				t.Fatalf("took the ID %q twice", m.ID)
			}
			seen[m.ID] = true
		}
		out, err := encode(all)
		if err != nil {
			t.Fatal(err)
		}
		again, err := decode(out)
		if err != nil {
			t.Fatalf("the file Robin writes does not read: %v\n%s", err, out)
		}
		if out2, _ := encode(again); !bytes.Equal(out, out2) {
			t.Fatalf("not a fixed point:\n%s\n%s", out, out2)
		}
	})
}

// FuzzCheck feeds one macro as the page puts it: one Check accepts stays
// within every bound protocol states, and reads back the same.
func FuzzCheck(f *testing.F) {
	for _, m := range []protocol.Macro{
		macro("m1", "Lock"),
		atTheCaps(),
		{ID: "x", Name: " ", Steps: []protocol.MacroStep{{Text: "t"}}},
		{ID: "x", Name: "n", Steps: []protocol.MacroStep{{Pause: protocol.MaxMacroPauseMs + 1}}},
		{ID: "x", Name: "n", Steps: []protocol.MacroStep{{Keys: []protocol.MacroKey{{Key: "a", Code: "Key-A"}}}}},
		{ID: "x/y", Name: "n", Steps: []protocol.MacroStep{{Text: "t"}}},
	} {
		b, err := json.Marshal(m)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var m protocol.Macro
		if json.Unmarshal(b, &m) != nil || Check(m) != nil {
			return
		}
		if !validID(m.ID) || len(m.Name) > protocol.MaxMacroNameBytes || len(m.Host) > protocol.MaxMacroHostBytes ||
			len(m.Steps) == 0 || len(m.Steps) > protocol.MaxMacroSteps {
			t.Fatalf("Check took %+v", m)
		}
		for _, st := range m.Steps {
			if len(st.Keys) > protocol.MaxMacroChordKeys || len(st.Text) > protocol.MaxMacroTextBytes ||
				st.Pause < 0 || st.Pause > protocol.MaxMacroPauseMs {
				t.Fatalf("Check took the step %+v", st)
			}
		}
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var again protocol.Macro
		if err := json.Unmarshal(out, &again); err != nil || Check(again) != nil {
			t.Fatalf("a macro Check took does not read back: %v", err)
		}
	})
}

// FuzzChange makes the page's change to a macros file of any contents. A
// change refused or a file that does not read leaves the file's bytes as
// they were; a change made leaves a file that reads, within its cap,
// holding the change and every macro pinned to another host as it was, in
// its order, and the session is shown what the file holds for its host
// alone.
func FuzzChange(f *testing.F) {
	changes := []string{
		`{"put":{"id":"m3","name":"New","steps":[{"text":"x"}]}}`,
		`{"put":{"id":"m1","name":"Again","steps":[{"pause":5}]}}`,
		`{"put":{"id":"m2","name":"Theirs","host":"other","steps":[{"pause":5}]}}`,
		`{"delete":"m1"}`,
		`{"delete":"m2"}`,
		`{"put":{"id":"m2","name":"Over","steps":[{"pause":5}]}}`,
		`{"delete":"none"}`,
		`{"put":{"id":"m3","name":"New","steps":[{"text":"x"}]},"delete":"m1"}`,
		`{}`, `null`,
	}
	for i, b := range fileSeeds(f) {
		f.Add(b, []byte(changes[i%len(changes)]), "box")
	}
	f.Add([]byte{}, []byte(changes[0]), "")
	for _, b := range fileSeeds(f)[:1] {
		for _, c := range changes {
			f.Add(b, []byte(c), "other")
		}
	}
	f.Fuzz(func(t *testing.T, file, change []byte, host string) {
		var c *protocol.MacroChange
		if json.Unmarshal(change, &c) != nil {
			return
		}
		path := filepath.Join(t.TempDir(), "robin", "macros.json")
		if len(file) > 0 {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, file, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		foreign := func(m protocol.Macro) bool { return m.Host != "" && m.Host != host }
		var theirs []protocol.Macro
		if before, err := decode(file); err == nil {
			theirs = filtered(before, foreign)
		}
		s := New(path, host)
		shown, err := s.Change(c)
		after, readErr := os.ReadFile(path)
		if err != nil {
			if len(file) == 0 && !errors.Is(readErr, fs.ErrNotExist) {
				t.Fatalf("a refused change made a file: %v", err)
			}
			if len(file) > 0 && !bytes.Equal(after, file) {
				t.Fatalf("a refused change (%v) rewrote the file", err)
			}
			return
		}
		if len(after) > protocol.MaxMacrosBytes {
			t.Fatalf("wrote %d bytes", len(after))
		}
		all, err := decode(after)
		if err != nil {
			t.Fatalf("the file written does not read: %v", err)
		}
		if mine := filtered(all, func(m protocol.Macro) bool { return !foreign(m) }); !slices.EqualFunc(mine, shown, sameMacro) {
			t.Fatal("the session is shown other macros than the file holds for its host")
		}
		if !slices.EqualFunc(filtered(all, foreign), theirs, sameMacro) {
			t.Fatalf("a change from %q touched another host's macros: %s", host, after)
		}
		switch {
		case c.Put != nil:
			if i := slices.IndexFunc(all, func(m protocol.Macro) bool { return m.ID == c.Put.ID }); i < 0 || !sameMacro(all[i], *c.Put) {
				t.Fatalf("the file lacks the macro put: %s", after)
			}
		default:
			if slices.ContainsFunc(all, func(m protocol.Macro) bool { return m.ID == c.Delete }) {
				t.Fatalf("the file keeps the macro deleted: %s", after)
			}
		}
		if entries, _ := os.ReadDir(filepath.Dir(path)); slices.ContainsFunc(entries, func(e os.DirEntry) bool {
			return strings.HasPrefix(e.Name(), ".macros-")
		}) {
			t.Fatal("a temporary file was left behind")
		}
	})
}

func filtered(all []protocol.Macro, keep func(protocol.Macro) bool) []protocol.Macro {
	var out []protocol.Macro
	for _, m := range all {
		if keep(m) {
			out = append(out, m)
		}
	}
	return out
}

func sameMacro(a, b protocol.Macro) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
