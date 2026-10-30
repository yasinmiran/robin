// Package xkb reads keyboard layouts from xkeyboard-config's symbols files,
// as far as a compositor that types only what its layout has needs: which
// printable ASCII characters the layout's keys carry, and its name.
package xkb

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxDepth bounds how deep includes may nest; xkeyboard-config's own go
// four or five levels.
const maxDepth = 16

// Layout is what a layout's first group types.
type Layout struct {
	// Name is the layout's own name for itself, such as "Russian".
	Name string
	syms map[string]bool // keysym names on any level of the keys
}

// Has reports whether the layout types r, a printable ASCII character, on
// one of its keys. A space, a tab and a newline come from keys every
// layout shares (pc), so it has those.
func (l Layout) Has(r rune) bool {
	switch r {
	case ' ', '\t', '\n':
		return true
	}
	name := asciiName(r)
	return name != "" && l.syms[name]
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
// from the symbols directories of dirs.
func Load(dirs []string, layout, variant string) (Layout, error) {
	r := &reader{dirs: dirs, files: map[string][]section{}}
	keys := map[string][]string{}
	name, err := r.apply(keys, layout, variant, modeOverride, 0)
	if err != nil {
		return Layout{}, err
	}
	l := Layout{Name: name, syms: map[string]bool{}}
	for _, levels := range keys {
		for _, s := range levels {
			l.syms[s] = true
		}
	}
	return l, nil
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
	levels  []string
	name    string
}

type reader struct {
	dirs  []string
	files map[string][]section
}

// apply merges the keys of file(variant) into keys and returns the name the
// section gives its group, if any.
func (r *reader) apply(keys map[string][]string, file, variant string, m mode, depth int) (string, error) {
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
			for _, part := range splitInclude(s.include) {
				f, v := ParseSource(part.ref)
				if part.group > 1 {
					continue
				}
				mm := s.mode
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
			keys[s.key] = merge(keys[s.key], s.levels, m == modeAugment || s.mode == modeAugment)
		case s.name != "":
			name = s.name
		}
	}
	return name, nil
}

// merge merges a key's new levels into its old ones level by level, as
// XKB does: NoSymbol (or any) leaves a level as it was, and augmenting
// fills only the levels that have nothing yet.
func merge(old, levels []string, augment bool) []string {
	out := append([]string(nil), old...)
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

func empty(sym string) bool { return sym == "NoSymbol" || sym == "any" }

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
}

// splitInclude splits an include such as "pc+us(basic):2|inet(evdev)" into
// its files, each with the group it is put in (1 unless named).
func splitInclude(s string) []includePart {
	var parts []includePart
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == '+' || r == '|' }) {
		ref, g, _ := strings.Cut(p, ":")
		group := 1
		if g != "" {
			fmt.Sscan(g, &group)
		}
		parts = append(parts, includePart{ref: ref, group: group})
	}
	return parts
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
