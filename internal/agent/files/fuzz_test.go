//go:build linux

package files

import (
	"cmp"
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
// holds files of its own, a link that leads nowhere, one that leads out
// of the directory and a directory. An upload refused leaves the
// directory as it was; one kept adds exactly one regular file, of the
// data, under a name CheckName takes that is no hidden file, and
// replaces nothing: each link still leads where it did, and what lies
// outside is untouched.
func FuzzUpload(f *testing.F) {
	for _, s := range nameSeeds {
		f.Add(s, []byte("payload"))
	}
	f.Add("a (1).txt", []byte{})
	f.Add("a (2).txt", []byte("through the link"))
	f.Add("a (3).txt", []byte("into the directory"))
	existing := map[string]string{"a.txt": "mine", "a (1).txt": "mine too", "c": "no extension"}
	links := map[string]string{"b.tar.gz": "nowhere", "a (2).txt": ""}
	const dir = "a (3).txt"
	f.Fuzz(func(t *testing.T, name string, data []byte) {
		in := testInbox(t)
		if err := os.MkdirAll(filepath.Join(in.Dir(), dir), 0o700); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte("not the inbox's"), 0o600); err != nil {
			t.Fatal(err)
		}
		for n, body := range existing {
			if err := os.WriteFile(filepath.Join(in.Dir(), n), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		for n, target := range links {
			if err := os.Symlink(cmp.Or(target, outside), filepath.Join(in.Dir(), n)); err != nil {
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
			got[e.Name()] = entryAt(t, filepath.Join(in.Dir(), e.Name()))
		}
		want := maps.Clone(existing)
		for n, target := range links {
			want[n] = "link to " + cmp.Or(target, outside)
		}
		want[dir] = "directory"
		if err == nil {
			if _, taken := want[saved]; taken || CheckName(saved) != nil || strings.HasPrefix(saved, ".") {
				t.Fatalf("%q was kept as %q", name, saved)
			}
			want[saved] = string(data)
		}
		if !maps.Equal(got, want) {
			t.Fatalf("uploading %q (%v) left the directory holding %q", name, err, got)
		}
		if fi, err := os.Lstat(filepath.Join(in.Dir(), saved)); saved != "" && (err != nil || !fi.Mode().IsRegular()) {
			t.Fatalf("%q was kept as %q, which is no regular file: %v", name, saved, err)
		}
		if b, err := os.ReadFile(outside); err != nil || string(b) != "not the inbox's" {
			t.Fatalf("uploading %q changed the file a link leads to: %q, %v", name, b, err)
		}
		if sub, err := os.ReadDir(filepath.Join(in.Dir(), dir)); err != nil || len(sub) > 0 {
			t.Fatalf("uploading %q put %d files in a directory: %v", name, len(sub), err)
		}
	})
}

// entryAt is what lies at path, without following a link: a regular
// file's contents, where a link leads, or that it is a directory.
func entryAt(t *testing.T, path string) string {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			t.Fatal(err)
		}
		return "link to " + target
	case fi.IsDir():
		return "directory"
	case !fi.Mode().IsRegular():
		return "special file " + fi.Mode().String()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
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
