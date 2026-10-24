//go:build linux

package files

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCheckNameAccepts(t *testing.T) {
	for _, name := range []string{
		"report.pdf", "a", ".bashrc", "...", "with space", " leading", "trailing ", "-dash",
		"日本語.txt", "emoji 👩‍💻.png", "naïve café", strings.Repeat("x", MaxName),
		strings.Repeat("é", MaxName/2), "back\\slash", "colon:name", "a..b",
	} {
		if err := CheckName(name); err != nil {
			t.Errorf("CheckName(%q) = %v, want nil", name, err)
		}
	}
}

func TestCheckNameRefuses(t *testing.T) {
	for name, input := range map[string]string{
		"empty":            "",
		"dot":              ".",
		"dot dot":          "..",
		"path":             "a/b",
		"parent path":      "../.ssh/authorized_keys",
		"absolute":         "/etc/passwd",
		"trailing slash":   "dir/",
		"NUL":              "a\x00b",
		"newline":          "a\nb",
		"carriage return":  "a\rb",
		"tab":              "a\tb",
		"escape":           "\x1b[31mred",
		"DEL":              "a\x7fb",
		"C1 control":       "a\u0085b",
		"right-to-left":    "invoice\u202efdp.exe",
		"isolate":          "a\u2066b",
		"left-to-right":    "a\u200eb",
		"arabic mark":      "a\u061cb",
		"line separator":   "a\u2028b",
		"para separator":   "a\u2029b",
		"too long":         strings.Repeat("x", MaxName+1),
		"too long in utf8": strings.Repeat("é", MaxName/2+1),
		"not utf-8":        "a\xffb",
	} {
		t.Run(name, func(t *testing.T) {
			if err := CheckName(input); !errors.Is(err, ErrName) {
				t.Errorf("CheckName(%q) = %v, want ErrName", input, err)
			}
		})
	}
}

func TestNumbered(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
		want string
	}{
		{"report.pdf", 0, "report.pdf"},
		{"report.pdf", 1, "report (1).pdf"},
		{"report.pdf", 12, "report (12).pdf"},
		{"noext", 1, "noext (1)"},
		{".bashrc", 1, ".bashrc (1)"},
		{".config.json", 2, ".config (2).json"},
		{"archive.tar.gz", 1, "archive (1).tar.gz"},
		{"archive.tar.zst", 3, "archive (3).tar.zst"},
		{".tar.gz", 1, ".tar (1).gz"},
		{"my.report.v2.pdf", 1, "my.report.v2 (1).pdf"},
		{"trailing.", 1, "trailing (1)."},
	} {
		if got := Numbered(tc.name, tc.n); got != tc.want {
			t.Errorf("Numbered(%q, %d) = %q, want %q", tc.name, tc.n, got, tc.want)
		}
	}
}

func TestNumberedStaysWithinMaxName(t *testing.T) {
	for _, name := range []string{
		strings.Repeat("x", MaxName-4) + ".pdf",
		strings.Repeat("é", 120) + ".txt",
		strings.Repeat("y", MaxName),
		"z." + strings.Repeat("e", MaxName-2), // an "extension" that leaves no room
	} {
		for _, n := range []int{1, 99, 999} {
			got := Numbered(name, n)
			if len(got) > MaxName || !utf8.ValidString(got) {
				t.Errorf("Numbered(%d bytes, %d) = %d bytes, valid UTF-8 %v", len(name), n, len(got), utf8.ValidString(got))
			}
			if err := CheckName(got); err != nil {
				t.Errorf("Numbered(%d bytes, %d) = %q: %v", len(name), n, got, err)
			}
			if !strings.Contains(got, " (") {
				t.Errorf("Numbered(%d bytes, %d) = %q lacks its number", len(name), n, got)
			}
		}
	}
	long := strings.Repeat("x", MaxName-4) + ".pdf"
	if got := Numbered(long, 1); !strings.HasSuffix(got, " (1).pdf") {
		t.Errorf("Numbered of a long name = %q, want the extension kept", got)
	}
}
