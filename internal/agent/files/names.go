// Package files moves whole files between the browser and the host:
// uploads into the user's download directory, and the files a program on
// the host offers through robin-agent send.
package files

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxName is the longest file name in bytes, Linux's NAME_MAX.
const MaxName = 255

// ErrName means a name cannot name a file here.
var ErrName = errors.New("files: bad file name")

// CheckName reports whether name may name a file the browser sends or the
// host offers: one path component of valid UTF-8, at most MaxName bytes,
// neither "." nor "..", with no control characters (NUL and newlines
// included) and none of the invisible ones that reorder text, which could
// make a name read as something it is not.
func CheckName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: empty", ErrName)
	case name == "." || name == "..":
		return fmt.Errorf("%w: %q", ErrName, name)
	case len(name) > MaxName:
		return fmt.Errorf("%w: %d bytes, the limit is %d", ErrName, len(name), MaxName)
	case !utf8.ValidString(name):
		return fmt.Errorf("%w: not UTF-8", ErrName)
	case strings.ContainsRune(name, '/'):
		return fmt.Errorf("%w: %q is a path, not a name", ErrName, name)
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp, unicode.Bidi_Control) {
			return fmt.Errorf("%w: %q holds the control character %U", ErrName, name, r)
		}
	}
	return nil
}

// Numbered is the name to try when name is taken for the nth time: " (n)"
// goes before the extension, and before ".tar" in a compressed tarball, so
// "a.pdf" becomes "a (1).pdf" and "b.tar.gz" "b (1).tar.gz". The stem is
// cut, at a character boundary, so the result stays within MaxName. A name
// that starts with its only dot has no extension. n of 0 is name itself.
func Numbered(name string, n int) string {
	if n == 0 {
		return name
	}
	stem, ext := split(name)
	suffix := fmt.Sprintf(" (%d)", n)
	keep := MaxName - len(suffix) - len(ext)
	if keep < 1 {
		// An extension this long is not one.
		stem, ext = name, ""
		keep = MaxName - len(suffix)
	}
	for len(stem) > keep {
		_, size := utf8.DecodeLastRuneInString(stem)
		stem = stem[:len(stem)-size]
	}
	return stem + suffix + ext
}

func split(name string) (stem, ext string) {
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 {
		return name, ""
	}
	stem, ext = name[:dot], name[dot:]
	if strings.HasSuffix(stem, ".tar") && len(stem) > len(".tar") {
		stem, ext = stem[:len(stem)-len(".tar")], ".tar"+ext
	}
	return stem, ext
}
