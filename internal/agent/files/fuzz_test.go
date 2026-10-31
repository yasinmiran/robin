//go:build linux

package files

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/yasinmiran/robin/internal/runes"
)

// nameSeeds are names TestCheckNameAccepts and TestCheckNameRefuses try.
var nameSeeds = []string{
	"a.txt", "résumé.pdf", "b.tar.gz", ".bashrc", "日本語.png", "a b (1).txt", strings.Repeat("x", MaxName),
	"", ".", "..", "a/b", "../x", "a\x00b", "a\nb", "‮txt.exe", "a b", "\xff", strings.Repeat("é", 128),
}

// FuzzCheckName feeds the names a page or a host program gives files: a
// name CheckName takes is one path component that stays in its
// directory, and every name Numbered makes of it is taken too, distinct
// from it and within MaxName.
func FuzzCheckName(f *testing.F) {
	for i, s := range nameSeeds {
		f.Add(s, uint16(i))
	}
	f.Fuzz(func(t *testing.T, name string, n uint16) {
		if err := CheckName(name); err != nil {
			return
		}
		if !utf8.ValidString(name) || len(name) > MaxName || !filepath.IsLocal(name) || filepath.Base(name) != name ||
			strings.ContainsFunc(name, func(r rune) bool { return unicode.IsControl(r) || runes.ReordersText(r) }) {
			t.Fatalf("CheckName took %q", name)
		}
		numbered := Numbered(name, int(n)+1)
		if err := CheckName(numbered); err != nil || numbered == name {
			t.Fatalf("Numbered(%q, %d) = %q: %v", name, n+1, numbered, err)
		}
		if strings.HasPrefix(numbered, ".") && !strings.HasPrefix(name, ".") {
			t.Fatalf("Numbered(%q, %d) = %q hides the file", name, n+1, numbered)
		}
	})
}

// FuzzUpload uploads data under any name into a download directory that
// holds files of its own. An upload refused leaves the directory as it
// was; one kept adds exactly one file, of the data, under a name
// CheckName takes that is no hidden file, and replaces nothing.
func FuzzUpload(f *testing.F) {
	for _, s := range nameSeeds {
		f.Add(s, []byte("payload"))
	}
	f.Add("a (1).txt", []byte{})
	existing := map[string]string{"a.txt": "mine", "a (1).txt": "mine too", "b.tar.gz": "tarball", "c": "no extension"}
	f.Fuzz(func(t *testing.T, name string, data []byte) {
		in := testInbox(t)
		if err := os.MkdirAll(in.Dir(), 0o700); err != nil {
			t.Fatal(err)
		}
		for n, body := range existing {
			if err := os.WriteFile(filepath.Join(in.Dir(), n), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		saved := ""
		u, err := in.Accept(name, int64(len(data)))
		if err == nil {
			if _, err = u.Write(data); err == nil {
				err = u.Commit()
			} else {
				u.Abort(err)
			}
		}
		if err == nil {
			saved = u.Saved()
		}
		got := map[string]string{}
		entries, rerr := os.ReadDir(in.Dir())
		if rerr != nil {
			t.Fatal(rerr)
		}
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(in.Dir(), e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			got[e.Name()] = string(b)
		}
		want := maps.Clone(existing)
		if err == nil {
			if _, taken := existing[saved]; taken || CheckName(saved) != nil || strings.HasPrefix(saved, ".") {
				t.Fatalf("%q was kept as %q", name, saved)
			}
			want[saved] = string(data)
		}
		if !maps.Equal(got, want) {
			t.Fatalf("uploading %q (%v) left the directory holding %q", name, err, keys(got))
		}
		if saved != "" && !bytes.Equal([]byte(got[saved]), data) {
			t.Fatal("the file kept is not the data")
		}
	})
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// FuzzPrintable feeds the detail of a failure, which can carry the
// browser's words to the agent's log and the user's terminal: printable
// leaves no control character and nothing that reorders text.
func FuzzPrintable(f *testing.F) {
	for _, s := range nameSeeds {
		f.Add(s)
	}
	f.Add("\x1b]0;title\x07\x9b2J")
	f.Fuzz(func(t *testing.T, s string) {
		if strings.ContainsFunc(printable(s), func(r rune) bool { return unicode.IsControl(r) || runes.ReordersText(r) }) {
			t.Fatalf("printable(%q) = %q", s, printable(s))
		}
	})
}
