// Package xkb reads keyboard layouts from xkeyboard-config's symbols and
// keycodes files, as far as mutter, which types a keysym only on a key of
// its layout, needs: which printable ASCII characters it types, and the
// layout's name.
package xkb

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// maxDepth bounds how deep includes may nest; xkeyboard-config's own go
// four or five levels.
const maxDepth = 16

// maxIncludes bounds the includes one Load applies in all. Depth alone
// does not bound the work: sections that each include the next several
// times over would be applied that many times to the power of the depth.
// xkeyboard-config's layouts apply a few dozen.
const maxIncludes = 1024

// maxLevel is the highest level, counted from 0, that mutter types a
// keysym on: it presses Shift for level 1 and AltGr for level 2, and for a
// higher one it presses the key alone, which types level 0 instead.
const maxLevel = 2

// Layout is what a layout's first group types.
type Layout struct {
	// Name is the layout's own name for itself, such as "Russian".
	Name string
	// levels holds the level mutter takes each keysym from: it looks a
	// keysym up on the keys in the order of their codes, and on each key
	// level by level, and takes the first it finds.
	levels map[string]int
}

// Has reports whether the layout types r, a printable ASCII character,
// when mutter is asked for its keysym. A tab and a newline come from keys
// every layout shares, so it has those, and a space.
func (l Layout) Has(r rune) bool {
	switch r {
	case ' ', '\t', '\n':
		return true
	}
	level, ok := l.levels[asciiName(r)]
	return ok && level <= maxLevel
}

// Latin reports whether the layout types the letters a to z.
func (l Layout) Latin() bool {
	for r := 'a'; r <= 'z'; r++ {
		if !l.Has(r) {
			return false
		}
	}
	return true
}

// Dirs lists where libxkbcommon looks for layouts, in its order: the user's
// own before the system's.
func Dirs() []string {
	var dirs []string
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		dirs = append(dirs, filepath.Join(d, "xkb"))
	} else if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".config", "xkb"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".xkb"))
	}
	dirs = append(dirs, "/etc/xkb")
	if d := os.Getenv("XKB_CONFIG_ROOT"); d != "" {
		return append(dirs, d)
	}
	return append(dirs, "/usr/share/X11/xkb")
}

// Load reads layout, in variant or its default one when variant is "",
// over pc's keys, from the symbols directories of dirs, with the evdev
// keycodes, which mutter's keymap has, from the first of them that has
// those.
func Load(dirs []string, layout, variant string) (Layout, error) {
	codes, err := keycodes(dirs)
	if err != nil {
		return Layout{}, err
	}
	r := &reader{dirs: dirs, codes: codes, files: map[string][]section{}}
	keys := map[int][][]string{}
	// The keymap puts the layout over pc's keys, as xkeyboard-config's
	// rules do for every PC model.
	if _, err := r.apply(keys, "pc", "", modeOverride, 0); err != nil {
		return Layout{}, err
	}
	name, err := r.apply(keys, layout, variant, modeOverride, 0)
	if err != nil {
		return Layout{}, err
	}
	l := Layout{Name: name, levels: map[string]int{}}
	for _, code := range slices.Sorted(maps.Keys(keys)) {
		for level, syms := range keys[code] {
			for _, s := range syms {
				if _, seen := l.levels[s]; !seen {
					l.levels[s] = level
				}
			}
		}
	}
	return l, nil
}

// keycodes reads keycodes/evdev from the first of dirs that has it.
func keycodes(dirs []string) (map[string]int, error) {
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(d, "keycodes", "evdev"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		codes, err := parseKeycodes(string(b))
		if err != nil {
			return nil, fmt.Errorf("xkb: keycodes/evdev: %w", err)
		}
		return codes, nil
	}
	return nil, errors.New("xkb: no keycodes/evdev")
}

// ParseSource splits a layout as GNOME and setxkbmap name it, "layout" or
// "layout+variant", or "layout(variant)" as an include does.
func ParseSource(id string) (layout, variant string) {
	if l, v, ok := strings.Cut(id, "+"); ok {
		return l, v
	}
	if l, v, ok := strings.Cut(id, "("); ok {
		return l, strings.TrimSuffix(v, ")")
	}
	return id, ""
}

type mode int

const (
	modeOverride mode = iota
	modeAugment
)

// section is one xkb_symbols block of a file.
type section struct {
	name  string
	def   bool
	stmts []stmt
}

// stmt is a statement of a section that matters here: an include, a key's
// first group, or the group's name.
type stmt struct {
	include string
	mode    mode
	key     string
	levels  [][]string
	name    string
}

type reader struct {
	dirs  []string
	codes map[string]int // by key name
	files map[string][]section
	// includes counts the includes applied so far.
	includes int
}

// apply merges the keys of file(variant) into keys, by keycode, and returns
// the name the section gives its group, if any. A key without a code is
// not on the keyboard, and is left out.
func (r *reader) apply(keys map[int][][]string, file, variant string, m mode, depth int) (string, error) {
	if depth > maxDepth {
		return "", fmt.Errorf("xkb: includes nest deeper than %d at %s(%s)", maxDepth, file, variant)
	}
	secs, err := r.load(file)
	if err != nil {
		return "", err
	}
	sec, ok := pick(secs, variant)
	if !ok {
		return "", fmt.Errorf("xkb: no variant %q in symbols/%s", variant, file)
	}
	var name string
	for _, s := range sec.stmts {
		switch {
		case s.include != "":
			for _, part := range splitInclude(s.include, s.mode) {
				f, v := ParseSource(part.ref)
				if part.group != 1 {
					continue
				}
				if r.includes++; r.includes > maxIncludes {
					return "", fmt.Errorf("xkb: more than %d includes at %s(%s)", maxIncludes, f, v)
				}
				mm := part.mode
				if m == modeAugment {
					mm = modeAugment
				}
				n, err := r.apply(keys, f, v, mm, depth+1)
				if err != nil {
					return "", err
				}
				if name == "" {
					name = n
				}
			}
		case s.key != "":
			if code, ok := r.codes[s.key]; ok {
				keys[code] = merge(keys[code], s.levels, m == modeAugment || s.mode == modeAugment)
			}
		case s.name != "":
			name = s.name
		}
	}
	return name, nil
}

// merge merges a key's new levels into its old ones level by level, as
// XKB does: NoSymbol (or any) leaves a level as it was, and augmenting
// fills only the levels that have nothing yet.
func merge(old, levels [][]string, augment bool) [][]string {
	out := append([][]string(nil), old...)
	for i, s := range levels {
		if i >= len(out) {
			out = append(out, s)
			continue
		}
		if empty(s) || augment && !empty(out[i]) {
			continue
		}
		out[i] = s
	}
	return out
}

func empty(level []string) bool {
	return len(level) == 0 || len(level) == 1 && (level[0] == "NoSymbol" || level[0] == "any")
}

func pick(secs []section, variant string) (section, bool) {
	for _, s := range secs {
		if variant == "" && s.def || variant != "" && s.name == variant {
			return s, true
		}
	}
	if variant == "" && len(secs) > 0 {
		return secs[0], true
	}
	return section{}, false
}

// load reads and parses symbols/file from the first directory that has it.
func (r *reader) load(file string) ([]section, error) {
	if secs, ok := r.files[file]; ok {
		return secs, nil
	}
	// A file may sit in a directory of symbols, as sun_vndr/us does, but
	// never outside symbols.
	if !filepath.IsLocal(file) || strings.ContainsRune(file, '\\') {
		return nil, fmt.Errorf("xkb: bad symbols file name %q", file)
	}
	var lastErr error = fmt.Errorf("xkb: no symbols/%s", file)
	for _, d := range r.dirs {
		b, err := os.ReadFile(filepath.Join(d, "symbols", file))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			lastErr = err
			continue
		}
		secs, err := parse(string(b))
		if err != nil {
			return nil, fmt.Errorf("xkb: symbols/%s: %w", file, err)
		}
		r.files[file] = secs
		return secs, nil
	}
	return nil, lastErr
}

type includePart struct {
	ref   string
	group int
	mode  mode
}

// splitInclude splits an include such as "pc+us(basic):2|inet(evdev)" into
// its files, each with the group it is put in (1 unless named) and how it
// merges: the first as the statement does, one after a + by overriding and
// one after a | by augmenting. A part whose group is not a number is left
// out.
func splitInclude(s string, first mode) []includePart {
	var parts []includePart
	for m := first; ; {
		end := strings.IndexAny(s, "+|")
		part := s
		if end >= 0 {
			part = s[:end]
		}
		ref, g, named := strings.Cut(part, ":")
		group, err := 1, error(nil)
		if named {
			group, err = strconv.Atoi(g)
		}
		if ref != "" && err == nil {
			parts = append(parts, includePart{ref: ref, group: group, mode: m})
		}
		if end < 0 {
			return parts
		}
		m = modeOverride
		if s[end] == '|' {
			m = modeAugment
		}
		s = s[end+1:]
	}
}

// asciiNames are the keysym names of the printable ASCII characters that
// are not letters or digits, whose names are the characters themselves.
var asciiNames = map[rune]string{
	'!': "exclam", '"': "quotedbl", '#': "numbersign", '$': "dollar", '%': "percent",
	'&': "ampersand", '\'': "apostrophe", '(': "parenleft", ')': "parenright", '*': "asterisk",
	'+': "plus", ',': "comma", '-': "minus", '.': "period", '/': "slash", ':': "colon",
	';': "semicolon", '<': "less", '=': "equal", '>': "greater", '?': "question", '@': "at",
	'[': "bracketleft", '\\': "backslash", ']': "bracketright", '^': "asciicircum",
	'_': "underscore", '`': "grave", '{': "braceleft", '|': "bar", '}': "braceright",
	'~': "asciitilde",
}

func asciiName(r rune) string {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return string(r)
	}
	return asciiNames[r]
}
