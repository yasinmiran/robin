package gui

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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

// TestShowPageKeepsTheTokenOffCommandLines: the browser opener's command
// line, which every user of this machine can read, gets the launch link and
// never the token. --no-browser prints the page's URL on stdout, and an
// opener that cannot start has it printed on stderr.
func TestShowPageKeepsTheTokenOffCommandLines(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the stand-in opener is xdg-open, which only Linux runs")
	}
	dir := t.TempDir()
	opened := filepath.Join(dir, "opened")
	script := "#!/bin/sh\nprintf '%s ' \"$0\" \"$@\" > \"$0.tmp\" && mv \"$0.tmp\" " + opened + "\n"
	if err := os.WriteFile(filepath.Join(dir, "xdg-open"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin:/bin")
	var stdout, stderr bytes.Buffer
	o := Options{Stdout: &stdout, Stderr: &stderr, Log: slog.New(slog.DiscardHandler)}
	srv, err := listenPage(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx) }()
	defer func() { cancel(); <-served }()
	token := srv.URL()[strings.Index(srv.URL(), "#")+1:]

	showPage(o, srv)
	var argv string
	for deadline := time.Now().Add(5 * time.Second); argv == "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		b, _ := os.ReadFile(opened)
		argv = string(b)
	}
	if !strings.Contains(argv, srv.LaunchURL()) || strings.Contains(argv, token) || stdout.Len() > 0 {
		t.Fatalf("the opener ran as %q, stdout %q; want the launch link and no token", argv, stdout.String())
	}

	o.NoBrowser = true
	showPage(o, srv)
	if stdout.String() != srv.URL()+"\n" {
		t.Fatalf("--no-browser printed %q", stdout.String())
	}

	o.NoBrowser = false
	t.Setenv("PATH", t.TempDir())
	showPage(o, srv)
	if !strings.Contains(stderr.String(), "open this URL in your browser: "+srv.URL()) {
		t.Fatalf("without an opener stderr is %q", stderr.String())
	}
}

// TestLaunchLinkUsedTwiceIsSaid: a second fetch of the launch link means the
// user's browser or someone else's got the page first, and the user hears of
// it on stderr, where the browser would only show Not Found.
func TestLaunchLinkUsedTwiceIsSaid(t *testing.T) {
	var stderr bytes.Buffer
	srv, err := listenPage(Options{Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx) }()
	defer func() { cancel(); <-served }()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for i, want := range []int{http.StatusSeeOther, http.StatusNotFound} {
		resp, err := client.Get(srv.LaunchURL())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("fetch %d: %s, want %d", i+1, resp.Status, want)
		}
		if said := strings.Contains(stderr.String(), "launch link was opened again"); said != (i == 1) {
			t.Fatalf("after fetch %d stderr is %q", i+1, stderr.String())
		}
	}
}

func TestOpenBrowserMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := openBrowser("http://127.0.0.1:1/"); err == nil {
		t.Error("openBrowser succeeded with no opener on PATH")
	}
}
