package deploy

import (
	"crypto/sha256"
	"strings"
	"testing"
	"unicode"
)

// FuzzParseHash feeds what the host's hash tool prints. A digest taken is
// the 64 lowercase hex digits a SHA-256 is written in, and is the output's
// first word, in either case, followed by white space or nothing.
func FuzzParseHash(f *testing.F) {
	sum := strings.Repeat("0f", 32)
	for _, s := range []string{
		sum + "  /a/file\n", sum + " */a/file\n", strings.ToUpper(sum) + "  f\n", "",
		"sha256sum: /a/file: No such file or directory\n", sum[:62] + "  f\n", "SHA2-256(/a/file)= " + sum + "\n",
		" \t" + sum, sum + "0", " " + sum + " x",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, out []byte) {
		got, ok := parseHash(out)
		if !ok {
			if got != "" {
				t.Fatalf("refused %q with %q", out, got)
			}
			return
		}
		if len(got) != 2*sha256.Size || strings.ContainsFunc(got, func(r rune) bool { return !strings.ContainsRune("0123456789abcdef", r) }) {
			t.Fatalf("took the digest %q from %q", got, out)
		}
		rest := strings.TrimLeftFunc(string(out), unicode.IsSpace)
		if len(rest) < len(got) || !strings.EqualFold(rest[:len(got)], got) {
			t.Fatalf("took %q, which does not start %q", got, out)
		}
		if tail := rest[len(got):]; tail != "" && !unicode.IsSpace([]rune(tail)[0]) {
			t.Fatalf("took %q from a longer word in %q", got, out)
		}
	})
}
