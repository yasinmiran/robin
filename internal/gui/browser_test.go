package gui

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

func TestBrowserCommand(t *testing.T) {
	const url = "http://127.0.0.1:1234/#token"
	missing := func(string) (string, error) { return "", errors.New("not found") }
	termux := func(name string) (string, error) {
		if name == "termux-open-url" {
			return "/data/data/com.termux/files/usr/bin/termux-open-url", nil
		}
		return "", errors.New("not found")
	}
	for _, tc := range []struct {
		name     string
		goos     string
		lookPath func(string) (string, error)
		want     []string
	}{
		{"macOS", "darwin", termux, []string{"open", url}},
		{"Windows", "windows", termux, []string{"rundll32", "url.dll,FileProtocolHandler", url}},
		{"Linux", "linux", missing, []string{"xdg-open", url}},
		{"Termux", "linux", termux, []string{"termux-open-url", url}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, args := browserCommand(tc.goos, tc.lookPath, url)
			if got := append([]string{name}, args...); !slices.Equal(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOpenBrowserTermux runs openBrowser with a termux-open-url on PATH, as
// Termux has, and checks the URL reaches it whole.
func TestOpenBrowserTermux(t *testing.T) {
	if runtime.GOOS != "linux" {
		// Elsewhere openBrowser runs the platform's own opener, which would
		// open a real browser.
		t.Skip("termux-open-url is looked up on Linux only")
	}
	dir := t.TempDir()
	opened := filepath.Join(dir, "opened")
	script := "#!/bin/sh\nprintf %s \"$1\" > \"$0.tmp\" && mv \"$0.tmp\" " + opened + "\n"
	if err := os.WriteFile(filepath.Join(dir, "termux-open-url"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin:/bin")
	const url = "http://127.0.0.1:1234/#token"
	if err := openBrowser(url); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(opened); err == nil {
			if string(b) != url {
				t.Fatalf("termux-open-url got %q, want %q", b, url)
			}
			return
		}
	}
	t.Fatal("termux-open-url never ran")
}

func TestOpenBrowserMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := openBrowser("http://127.0.0.1:1/"); err == nil {
		t.Error("openBrowser succeeded with no opener on PATH")
	}
}
