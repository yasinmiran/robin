package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/yasinmiran/robin/internal/protocol"
)

func start(t *testing.T) (*Server, context.CancelFunc) {
	t.Helper()
	s, err := Listen(Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return s, cancel
}

func token(s *Server) string { return s.URL()[strings.Index(s.URL(), "#")+1:] }

func dialOpts(s *Server) *websocket.DialOptions {
	return &websocket.DialOptions{
		HTTPHeader:   http.Header{"Origin": {"http://" + s.host}},
		Subprotocols: []string{subprotocol, tokenPrefix + token(s)},
	}
}

func dial(t *testing.T, s *Server, o *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, "ws://"+s.host+"/ws", o)
	if c != nil {
		t.Cleanup(func() { c.CloseNow() })
	}
	return c, resp, err
}

// connect dials with valid credentials and returns both ends.
func connect(t *testing.T, s *Server) (*websocket.Conn, *Browser) {
	t.Helper()
	c, _, err := dial(t, s, dialOpts(s))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := s.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	return c, b
}

func wantStatus(t *testing.T, resp *http.Response, err error, code int) {
	t.Helper()
	if err == nil {
		t.Fatal("dial succeeded, want rejection")
	}
	if resp == nil || resp.StatusCode != code {
		t.Fatalf("got response %v (err %v), want status %d", resp, err, code)
	}
}

func TestListenRefusesNonLoopback(t *testing.T) {
	for _, addr := range []string{":0", "0.0.0.0:0", "[::]:0", "localhost:0", "192.0.2.1:0", "example.com:0", "nonsense"} {
		if s, err := Listen(Options{Addr: addr}); err == nil {
			s.ln.Close()
			t.Errorf("Listen(%q) succeeded", addr)
		}
	}
}

func TestURL(t *testing.T) {
	a, err := Listen(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.ln.Close()
	b, err := Listen(Options{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.ln.Close()

	if !regexp.MustCompile(`^http://127\.0\.0\.1:[1-9][0-9]*/#[0-9a-f]{64}$`).MatchString(a.URL()) {
		t.Errorf("URL() = %q", a.URL())
	}
	if token(a) == token(b) {
		t.Error("two servers share a token")
	}
}

func TestHostHeader(t *testing.T) {
	s, err := Listen(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.ln.Close()
	h := s.handler()
	_, port, _ := strings.Cut(s.host, ":")

	for host, want := range map[string]int{
		s.host:                     http.StatusOK,
		"localhost:" + port:        http.StatusForbidden,
		"attacker.example:" + port: http.StatusForbidden,
		"127.0.0.1":                http.StatusForbidden,
		"":                         http.StatusForbidden,
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("Host %q: status %d, want %d", host, w.Code, want)
		}
	}
}

func TestHostHeaderWebsocket(t *testing.T) {
	s, _ := start(t)
	o := dialOpts(s)
	o.Host = "attacker.example"
	_, resp, err := dial(t, s, o)
	wantStatus(t, resp, err, http.StatusForbidden)
}

func TestSecurityHeaders(t *testing.T) {
	s, err := Listen(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.ln.Close()
	h := s.handler()

	for _, tc := range []struct {
		method, path, host string
		code               int
	}{
		{"GET", "/", s.host, http.StatusOK},
		{"GET", "/app.js", s.host, http.StatusOK},
		{"GET", "/style.css", s.host, http.StatusOK},
		{"GET", "/missing", s.host, http.StatusNotFound},
		{"POST", "/", s.host, http.StatusMethodNotAllowed},
		{"GET", "/", "attacker.example", http.StatusForbidden},
		{"GET", "/ws", s.host, http.StatusForbidden},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Host = tc.host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Errorf("%s %s: status %d, want %d", tc.method, tc.path, w.Code, tc.code)
		}
		want := map[string]string{
			"Content-Security-Policy": "default-src 'none'; script-src 'self'; style-src 'self'; connect-src ws://" + s.host +
				"; media-src blob: mediastream:; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'",
			"X-Frame-Options":        "DENY",
			"Referrer-Policy":        "no-referrer",
			"Cache-Control":          "no-store",
			"X-Content-Type-Options": "nosniff",
		}
		for k, v := range want {
			if got := w.Header().Get(k); got != v {
				t.Errorf("%s %s: %s = %q, want %q", tc.method, tc.path, k, got, v)
			}
		}
	}
}

func TestStaticContentTypes(t *testing.T) {
	s, err := Listen(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.ln.Close()
	h := s.handler()
	for path, want := range map[string]string{
		"/":          "text/html",
		"/app.js":    "text/javascript",
		"/style.css": "text/css",
		"/icon.svg":  "image/svg+xml",
	} {
		r := httptest.NewRequest("GET", path, nil)
		r.Host = s.host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, want) {
			t.Errorf("%s: Content-Type %q, want %s", path, got, want)
		}
	}
}

// TestStaticServesEveryPageFile keeps the embed's list of extensions in step
// with static/: a page file it misses would 404 at runtime.
func TestStaticServesEveryPageFile(t *testing.T) {
	entries, err := os.ReadDir("static")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Listen(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.ln.Close()
	h := s.handler()
	served := 0
	for _, e := range entries {
		if e.IsDir() || e.Name() == "package.json" {
			continue
		}
		r := httptest.NewRequest("GET", "/"+e.Name(), nil)
		r.Host = s.host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("/%s: status %d, want %d", e.Name(), w.Code, http.StatusOK)
		}
		served++
	}
	if served == 0 {
		t.Fatal("static holds no page files")
	}
}

func TestStaticLeavesOutTests(t *testing.T) {
	s, err := Listen(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.ln.Close()
	h := s.handler()
	for _, path := range []string{"/package.json", "/test/stats.test.js", "/test/"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Host = s.host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want %d", path, w.Code, http.StatusNotFound)
		}
	}
}

func TestOriginRequired(t *testing.T) {
	s, _ := start(t)
	_, port, _ := strings.Cut(s.host, ":")
	for _, origin := range []string{"", "null", "https://" + s.host, "http://localhost:" + port, "http://attacker.example", "http://" + s.host + "/"} {
		o := dialOpts(s)
		o.HTTPHeader = http.Header{}
		if origin != "" {
			o.HTTPHeader.Set("Origin", origin)
		}
		_, resp, err := dial(t, s, o)
		wantStatus(t, resp, err, http.StatusForbidden)
	}
}

func TestTokenRequired(t *testing.T) {
	s, _ := start(t)
	tok := token(s)
	for name, protos := range map[string][]string{
		"none":       nil,
		"bare":       {subprotocol},
		"wrong":      {subprotocol, tokenPrefix + strings.Repeat("0", 64)},
		"truncated":  {subprotocol, tokenPrefix + tok[:63]},
		"extended":   {subprotocol, tokenPrefix + tok + "0"},
		"unprefixed": {subprotocol, tok},
	} {
		o := dialOpts(s)
		o.Subprotocols = protos
		_, resp, err := dial(t, s, o)
		if err == nil {
			t.Errorf("%s: dial succeeded", name)
			continue
		}
		if resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: response %v, want 403", name, resp)
		}
	}
}

func TestRejectionCarriesNoDetail(t *testing.T) {
	s, _ := start(t)
	o := dialOpts(s)
	o.Subprotocols = []string{subprotocol, tokenPrefix + "nope"}
	_, resp, err := dial(t, s, o)
	wantStatus(t, resp, err, http.StatusForbidden)
	// The body is the bare status text, identical for every auth failure.
	r := httptest.NewRequest("GET", "/ws", nil)
	r.Host = s.host
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	if got := strings.TrimSpace(w.Body.String()); got != "Forbidden" {
		t.Errorf("body %q, want %q", got, "Forbidden")
	}
}

func TestSubprotocolSelected(t *testing.T) {
	s, _ := start(t)
	c, _ := connect(t, s)
	if c.Subprotocol() != subprotocol {
		t.Errorf("subprotocol %q, want %q", c.Subprotocol(), subprotocol)
	}
}

func TestRelay(t *testing.T) {
	s, _ := start(t)
	c, b := connect(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := protocol.Message{Type: protocol.TypeConfig, Config: &protocol.Config{
		ICEServers: []protocol.ICEServer{{URLs: []string{"stun:stun.example:3478"}}},
	}}
	if err := b.Send(cfg); err != nil {
		t.Fatal(err)
	}
	typ, p, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got protocol.Message
	if typ != websocket.MessageText || json.Unmarshal(p, &got) != nil || got.Type != protocol.TypeConfig || got.Config.ICEServers[0].URLs[0] != "stun:stun.example:3478" {
		t.Errorf("page got %v %s", typ, p)
	}

	mid := "0"
	for _, sent := range []protocol.Message{
		{Type: protocol.TypeOffer, SDP: "v=0\r\n"},
		{Type: protocol.TypeCandidate, Candidate: &protocol.Candidate{Candidate: "candidate:1 1 udp 1 192.0.2.1 9 typ host", SDPMid: &mid}},
		{Type: protocol.TypeCandidate, Candidate: &protocol.Candidate{}},
	} {
		p, _ := json.Marshal(sent)
		if err := c.Write(ctx, websocket.MessageText, p); err != nil {
			t.Fatal(err)
		}
		m, err := b.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := json.Marshal(sent)
		if gotJSON, _ := json.Marshal(m); string(gotJSON) != string(want) {
			t.Errorf("Recv = %s, want %s", gotJSON, want)
		}
	}
}

func TestPageMessagesRestricted(t *testing.T) {
	big := `{"type":"offer","sdp":"` + strings.Repeat("a", protocol.MaxLineBytes) + `"}`
	for name, frame := range map[string]struct {
		typ websocket.MessageType
		p   string
	}{
		"start":     {websocket.MessageText, `{"type":"start","start":{"version":1}}`},
		"bye":       {websocket.MessageText, `{"type":"bye"}`},
		"untyped":   {websocket.MessageText, `{"sdp":"v=0"}`},
		"malformed": {websocket.MessageText, `{"type":`},
		"binary":    {websocket.MessageBinary, `{"type":"offer","sdp":"v=0"}`},
		"oversized": {websocket.MessageText, big},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := start(t)
			c, b := connect(t, s)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c.Write(ctx, frame.typ, []byte(frame.p))
			if m, err := b.Recv(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Recv = %v, %v; want the connection closed", m, err)
			}
			<-b.Done()
		})
	}
}

func TestOneBrowserAtATime(t *testing.T) {
	s, _ := start(t)
	first, b := connect(t, s)

	_, resp, err := dial(t, s, dialOpts(s))
	wantStatus(t, resp, err, http.StatusConflict)

	// The page going away frees the slot, as a reload would.
	first.Close(websocket.StatusNormalClosure, "")
	select {
	case <-b.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("browser not done after the page closed")
	}
	_, b2 := connect(t, s)

	// Closing from the server side frees it too.
	b2.Close()
	connect(t, s)
}

func TestServeCancelClosesBrowser(t *testing.T) {
	s, cancel := start(t)
	_, b := connect(t, s)
	cancel()
	select {
	case <-b.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("browser still open after Serve was cancelled")
	}
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if _, err := s.Accept(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("Accept after cancel = %v, want ErrClosed", err)
	}
}
