// Package macros keeps the user's key macros on the client machine, for the
// page to play: one file, macros.json in the user's config directory. The
// page names no path; it asks for one change at a time, which the file
// takes only once Check accepts it and the result fits the caps in
// internal/protocol.
package macros

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/yasinmiran/robin/internal/protocol"
)

// fileVersion is the version the file says it is in.
const fileVersion = 1

// file is macros.json.
type file struct {
	Version int              `json:"version"`
	Macros  []protocol.Macro `json:"macros"`
}

// Path is where the macros are kept: robin/macros.json in the user's config
// directory.
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "robin", "macros.json"), nil
}

// Store is the macros file as a session to one host sees it: the macros
// pinned to that host and those pinned to none. Every call reads the file
// afresh, so a change another session made in the meantime is kept. It is
// safe for concurrent use.
type Store struct {
	path    string
	pathErr error // why there is no path
	host    string
	mu      sync.Mutex
}

// New opens the store at Path for a session to host, the host argument as
// the user typed it.
func New(host string) *Store {
	p, err := Path()
	return &Store{path: p, pathErr: err, host: host}
}

// Open opens the store at path for a session to host.
func Open(path, host string) *Store {
	return &Store{path: path, host: host}
}

// Load returns what the page shows, with Error set when the file cannot be
// read.
func (s *Store) Load() *protocol.Macros {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.read()
	return s.view(all, err)
}

// Change makes the page's change and returns what the page shows after it,
// with Error set when the change was refused or the file could not be
// read or written. A file that cannot be read is left as it is.
func (s *Store) Change(c *protocol.MacroChange) *protocol.Macros {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.read()
	if err != nil {
		return s.view(nil, err)
	}
	next, err := s.apply(all, c)
	if err == nil {
		err = s.write(next)
	}
	if err == nil {
		all = next
	}
	return s.view(all, err)
}

func (s *Store) view(all []protocol.Macro, err error) *protocol.Macros {
	out := &protocol.Macros{Host: s.host, List: []protocol.Macro{}}
	if err != nil {
		out.Error = err.Error()
	}
	for _, m := range all {
		if s.sees(m) {
			out.List = append(out.List, m)
		}
	}
	return out
}

// sees reports whether a macro shows in this session.
func (s *Store) sees(m protocol.Macro) bool {
	return m.Host == "" || m.Host == s.host
}

func (s *Store) apply(all []protocol.Macro, c *protocol.MacroChange) ([]protocol.Macro, error) {
	if c == nil || (c.Put == nil) == (c.Delete == "") {
		return nil, errors.New("a change puts or deletes one macro")
	}
	if c.Put == nil {
		i := slices.IndexFunc(all, func(m protocol.Macro) bool { return m.ID == c.Delete })
		if i < 0 || !s.sees(all[i]) {
			return nil, fmt.Errorf("there is no macro %q to delete", c.Delete)
		}
		return slices.Delete(slices.Clone(all), i, i+1), nil
	}
	m := *c.Put
	if m.Host != "" && m.Host != s.host {
		return nil, fmt.Errorf("a macro made in this session can be pinned to %q only", s.host)
	}
	if err := Check(m); err != nil {
		return nil, err
	}
	next := slices.Clone(all)
	i := slices.IndexFunc(next, func(o protocol.Macro) bool { return o.ID == m.ID })
	switch {
	case i >= 0 && !s.sees(next[i]):
		return nil, fmt.Errorf("macro ID %q is taken by a macro of another host", m.ID)
	case i >= 0:
		next[i] = m
	case len(next) >= protocol.MaxMacros:
		return nil, fmt.Errorf("Robin keeps %d macros at most; delete one first", protocol.MaxMacros)
	default:
		next = append(next, m)
	}
	return next, nil
}

// errUnreadable says what a session does about a file it cannot read.
const errUnreadable = "Robin leaves it as it is and saves no macro until it reads"

func (s *Store) read() ([]protocol.Macro, error) {
	if s.pathErr != nil {
		return nil, fmt.Errorf("macros are not kept: %w", s.pathErr)
	}
	// A FIFO would hold the open up; only a regular file is read.
	st, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file; %s", s.path, errUnreadable)
	}
	f, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, protocol.MaxMacrosBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	if len(b) > protocol.MaxMacrosBytes {
		return nil, fmt.Errorf("%s is over %d KiB; %s", s.path, protocol.MaxMacrosBytes>>10, errUnreadable)
	}
	all, err := decode(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w; %s", s.path, err, errUnreadable)
	}
	return all, nil
}

// decode reads the file's JSON, which must hold nothing the schema does
// not name and only macros Check accepts.
func decode(b []byte) ([]protocol.Macro, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	var f file
	if err := d.Decode(&f); err != nil {
		return nil, err
	}
	if err := d.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one JSON value")
	}
	if f.Version != fileVersion {
		return nil, fmt.Errorf("version %d, where this Robin reads %d", f.Version, fileVersion)
	}
	if len(f.Macros) > protocol.MaxMacros {
		return nil, fmt.Errorf("%d macros, over the %d Robin keeps", len(f.Macros), protocol.MaxMacros)
	}
	seen := make(map[string]bool, len(f.Macros))
	for _, m := range f.Macros {
		if err := Check(m); err != nil {
			return nil, err
		}
		if seen[m.ID] {
			return nil, fmt.Errorf("macro ID %q twice", m.ID)
		}
		seen[m.ID] = true
	}
	return f.Macros, nil
}

func encode(all []protocol.Macro) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	if err := e.Encode(file{Version: fileVersion, Macros: all}); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// write replaces the file through a rename, so a reader finds the old file
// or the new one and never a part. The file is 0600 and its directory
// 0700: what a macro types may be a password.
func (s *Store) write(all []protocol.Macro) (err error) {
	b, err := encode(all)
	if err != nil {
		return err
	}
	if len(b) > protocol.MaxMacrosBytes {
		return fmt.Errorf("the macros would take %d KiB, over the %d KiB the file holds", (len(b)+1023)>>10, protocol.MaxMacrosBytes>>10)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	// CreateTemp makes the file 0600, and a name of its own that no other
	// file has.
	f, err := os.CreateTemp(dir, ".macros-*.json")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path)
}

// Check reports what keeps m out of the file.
func Check(m protocol.Macro) error {
	if !validID(m.ID) {
		return fmt.Errorf("macro ID %q is not 1 to %d letters, digits, - or _", m.ID, protocol.MacroIDChars)
	}
	if len(m.Name) > protocol.MaxMacroNameBytes || !printable(m.Name) || strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("a macro's name is 1 to %d bytes of printable text", protocol.MaxMacroNameBytes)
	}
	if len(m.Host) > protocol.MaxMacroHostBytes || !printable(m.Host) {
		return fmt.Errorf("macro %q: a host is up to %d bytes of printable text", m.Name, protocol.MaxMacroHostBytes)
	}
	if n := len(m.Steps); n == 0 || n > protocol.MaxMacroSteps {
		return fmt.Errorf("macro %q: %d steps, where a macro takes 1 to %d", m.Name, n, protocol.MaxMacroSteps)
	}
	for i, st := range m.Steps {
		if err := checkStep(st); err != nil {
			return fmt.Errorf("macro %q, step %d: %w", m.Name, i+1, err)
		}
	}
	return nil
}

func checkStep(st protocol.MacroStep) error {
	set := 0
	for _, is := range []bool{len(st.Keys) > 0, st.Text != "", st.Pause != 0} {
		if is {
			set++
		}
	}
	if set != 1 {
		return errors.New("a step is one chord, one run of text or one pause")
	}
	switch {
	case len(st.Keys) > protocol.MaxMacroChordKeys:
		return fmt.Errorf("a chord takes %d keys at most", protocol.MaxMacroChordKeys)
	case len(st.Keys) > 0:
		for _, k := range st.Keys {
			if k.Key == "" || len(k.Key) > protocol.MaxMacroKeyBytes || !printable(k.Key) {
				return fmt.Errorf("a key is 1 to %d bytes of printable text", protocol.MaxMacroKeyBytes)
			}
			if len(k.Code) > protocol.MaxMacroKeyBytes || !alnum(k.Code) {
				return fmt.Errorf("a key's code is up to %d letters and digits", protocol.MaxMacroKeyBytes)
			}
		}
	case st.Text != "":
		if len(st.Text) > protocol.MaxMacroTextBytes || !utf8.ValidString(st.Text) {
			return fmt.Errorf("text is up to %d KiB of UTF-8", protocol.MaxMacroTextBytes>>10)
		}
	case st.Pause < 0 || st.Pause > protocol.MaxMacroPauseMs:
		return fmt.Errorf("a pause is 1 to %d ms", protocol.MaxMacroPauseMs)
	}
	return nil
}

func validID(id string) bool {
	if id == "" || len(id) > protocol.MacroIDChars {
		return false
	}
	for _, r := range id {
		if !(r == '-' || r == '_' || r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r))) {
			return false
		}
	}
	return true
}

// printable reports whether s is UTF-8 without control characters.
func printable(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	return !strings.ContainsFunc(s, unicode.IsControl)
}

func alnum(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool {
		return r >= utf8.RuneSelf || !(unicode.IsLetter(r) || unicode.IsDigit(r))
	})
}
