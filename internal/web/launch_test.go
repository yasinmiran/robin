package web

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
)

// TestLaunchLinkIsSingleUse: the link the CLI hands the browser opener, and
// so puts on a command line any user of the machine can read, carries no
// token. It redirects to the page with the token once, and is gone after.
func TestLaunchLinkIsSingleUse(t *testing.T) {
	var reused atomic.Int32
	s, err := Listen(Options{LaunchReused: func() { reused.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	serveWith(t, s)
	launch := s.LaunchURL()
	if !regexp.MustCompile(`^http://127\.0\.0\.1:[1-9][0-9]*/open/[0-9a-f]{64}$`).MatchString(launch) {
		t.Fatalf("LaunchURL() = %q", launch)
	}
	if strings.Contains(launch, token(s)) {
		t.Fatal("the launch link carries the token")
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	fetch := func(url, host string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest("GET", url, nil)
		req.Host = host
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, string(body)
	}

	// The Host check comes first, and a refused request does not spend it.
	if resp, _ := fetch(launch, "attacker.example"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong Host: %s", resp.Status)
	}
	wrong := launch[:strings.LastIndex(launch, "/")+1] + strings.Repeat("0", 64)
	for _, u := range []string{wrong, launch[:strings.LastIndex(launch, "/")+1], launch + "0"} {
		if resp, body := fetch(u, s.host); resp.StatusCode != http.StatusNotFound || strings.TrimSpace(body) != "Not Found" {
			t.Fatalf("GET %s: %s %q, want a bare 404", u, resp.Status, body)
		}
	}
	if reused.Load() != 0 {
		t.Fatal("a wrong link counted as the launch link used again")
	}

	resp, body := fetch(launch, s.host)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/#"+token(s) {
		t.Fatalf("first use: %s to %q, want 303 to the page with the token", resp.Status, resp.Header.Get("Location"))
	}
	if body != "" || resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("first use: body %q, headers %v", body, resp.Header)
	}

	resp, body = fetch(launch, s.host)
	if resp.StatusCode != http.StatusNotFound || strings.Contains(body, token(s)) || strings.Contains(resp.Header.Get("Location"), token(s)) {
		t.Fatalf("second use: %s %q, want a 404 without the token", resp.Status, body)
	}
	if reused.Load() != 1 {
		t.Fatalf("LaunchReused called %d times after one reuse", reused.Load())
	}
}
