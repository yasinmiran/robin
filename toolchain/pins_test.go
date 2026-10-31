// Package toolchain holds the shell scripts that fetch and build the agents'
// C toolchain; its test checks that every download stays pinned by hash.
package toolchain

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// fetchCall matches `fetch "URL" "SHA" "NAME"` once continuation lines
	// are joined.
	fetchCall  = regexp.MustCompile(`\bfetch "([^"]+)"\s+"([^"]+)"\s+"([^"]+)"`)
	assignment = regexp.MustCompile(`(?m)^\s*([A-Za-z_][A-Za-z0-9_]*)=([^\s;]+)`)
	caseSHA    = regexp.MustCompile(`\bsha=([^\s;]+)`)
	downloader = regexp.MustCompile(`\b(curl|wget)\b`)
)

// script is a shell file with its comments dropped and its continuation
// lines joined.
func script(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		lines = append(lines, l)
	}
	return strings.ReplaceAll(strings.Join(lines, "\n"), "\\\n", " ")
}

// TestDownloadsArePinned fails when a toolchain script or the Makefile
// downloads anything other than through lib.sh's fetch, or names a fetch
// whose hash is not a literal SHA-256.
func TestDownloadsArePinned(t *testing.T) {
	scripts, err := filepath.Glob("*.sh")
	if err != nil || len(scripts) == 0 {
		t.Fatalf("no toolchain scripts: %v", err)
	}
	lib := script(t, "lib.sh")
	pinned := map[string]bool{}
	for _, m := range assignment.FindAllStringSubmatch(lib, -1) {
		if sha256Hex.MatchString(m[2]) {
			pinned[m[1]] = true
		}
	}

	fetches := 0
	for _, path := range append(scripts, "../Makefile") {
		text := script(t, path)
		for _, m := range downloader.FindAllStringIndex(text, -1) {
			if path != "lib.sh" {
				t.Errorf("%s downloads with %s instead of fetch", path, text[m[0]:m[1]])
			}
		}
		for _, m := range fetchCall.FindAllStringSubmatch(text, -1) {
			fetches++
			url, hash := m[1], m[2]
			if !strings.HasPrefix(url, "https://") {
				t.Errorf("%s fetches %s over something other than https", path, url)
			}
			name, ok := strings.CutPrefix(hash, "$")
			switch {
			case ok && pinned[name]:
			case ok && name == "sha":
				shas := caseSHA.FindAllStringSubmatch(text, -1)
				if len(shas) == 0 {
					t.Errorf("%s fetches %s by $sha, which it never sets", path, url)
				}
				for _, s := range shas {
					if !sha256Hex.MatchString(s[1]) {
						t.Errorf("%s sets sha=%s, not a SHA-256", path, s[1])
					}
				}
			default:
				t.Errorf("%s fetches %s by %q, not a pinned SHA-256", path, url, hash)
			}
		}
	}
	if fetches < 4 {
		t.Errorf("found %d fetches, want zig, nasm, openh264 and opus at least", fetches)
	}

	if n := len(downloader.FindAllString(lib, -1)); n != 1 {
		t.Errorf("lib.sh downloads in %d places, want only fetch's", n)
	}
	for _, check := range []string{`got=$(sha256 "$dest")`, `[ "$got" != "$2" ]`, `rm -f "$dest"`, `die "sha256 mismatch`} {
		if !strings.Contains(lib, check) {
			t.Errorf("lib.sh's fetch no longer has %s", check)
		}
	}
}
