package gui

import (
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// printable makes text from the host safe to show on the user's terminal.
// Control characters other than newline and tab, and bytes that are not
// UTF-8, become visible escapes, so an error message or a log line cannot
// move the cursor, retitle the window or send the terminal other commands.
// So do the invisible marks that reorder text and the Unicode line and
// paragraph separators, the set the agent refuses in a file name, so a host
// cannot make a path or a command it prints read as another.
func printable(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case unicode.IsControl(r) && r != '\n' && r != '\t':
			fmt.Fprintf(&b, `\x%02x`, r)
		case unicode.In(r, unicode.Zl, unicode.Zp, unicode.Bidi_Control):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// printableWriter passes everything written to it through printable.
type printableWriter struct{ w io.Writer }

func (p printableWriter) Write(b []byte) (int, error) {
	if _, err := io.WriteString(p.w, printable(string(b))); err != nil {
		return 0, err
	}
	return len(b), nil
}

// printableError is an error whose text is made printable; errors.Is and
// errors.As still see the original.
type printableError struct{ err error }

func (e printableError) Error() string { return printable(e.err.Error()) }

func (e printableError) Unwrap() error { return e.err }
