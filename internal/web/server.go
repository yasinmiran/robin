// Package web serves the browser client on loopback and carries signaling
// between that page and the CLI over a websocket.
//
// The page and its websocket are the one unauthenticated surface of
// remotejump: whatever drives them drives the remote desktop. Every request
// must carry the listener's own Host, the websocket must come from the page's
// own Origin and present the random token from URL, and only one browser may
// be connected at a time.
package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

//go:embed static
var static embed.FS

// subprotocol is the websocket subprotocol the page requests and the server
// selects. The page also offers tokenPrefix followed by the token, which is
// how the token travels without appearing in any URL.
const (
	subprotocol = "remotejump"
	tokenPrefix = "token."
)

// ErrClosed is returned by Accept once Serve has returned.
var ErrClosed = errors.New("web: server closed")

// Options configures Listen.
type Options struct {
	// Addr is the loopback address to bind. Empty means "127.0.0.1:0".
	Addr string
}

// Server serves the client page and hands authenticated websockets to Accept.
type Server struct {
	ln      net.Listener
	host    string // "127.0.0.1:PORT", the only Host and Origin host accepted
	token   string
	headers http.Header

	live     atomic.Bool
	accept   chan *Browser
	done     chan struct{}
	doneOnce sync.Once
}

// Listen binds a loopback address and generates the session token. It refuses
// any address that is not a literal loopback IP.
func Listen(o Options) (*Server, error) {
	addr := o.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	if ip := net.ParseIP(h); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("web: refusing to listen on non-loopback address %q", addr)
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	host := ln.Addr().String()
	return &Server{
		ln:      ln,
		host:    host,
		token:   hex.EncodeToString(b[:]),
		headers: securityHeaders(host),
		accept:  make(chan *Browser),
		done:    make(chan struct{}),
	}, nil
}

// URL is the address to open in the browser. The token rides in the fragment,
// which browsers never send in requests or Referer headers.
func (s *Server) URL() string {
	return "http://" + s.host + "/#" + s.token
}

// Serve serves until ctx is cancelled, then closes the listener and any
// connected browser. It returns nil after cancellation.
func (s *Server) Serve(ctx context.Context) error {
	defer s.doneOnce.Do(func() { close(s.done) })
	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	stop := context.AfterFunc(ctx, func() { srv.Close() })
	defer stop()
	err := srv.Serve(s.ln)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// Accept returns the next authenticated browser.
func (s *Server) Accept(ctx context.Context) (*Browser, error) {
	select {
	case b := <-s.accept:
		return b, nil
	case <-s.done:
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", serveStatic)
	mux.HandleFunc("GET /ws", s.serveWS)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range s.headers {
			w.Header()[k] = v
		}
		if r.Host != s.host {
			deny(w, http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// serveStatic serves the embedded client. Errors go through deny rather than
// http.FileServer, which strips Cache-Control from error responses.
func serveStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	b, err := fs.ReadFile(static, path.Join("static", name))
	if err != nil {
		deny(w, http.StatusNotFound)
		return
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(b))
}

func (s *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "http://"+s.host || !s.validToken(r) {
		deny(w, http.StatusForbidden)
		return
	}
	if !s.live.CompareAndSwap(false, true) {
		deny(w, http.StatusConflict)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{subprotocol},
		// Origin is checked above, exactly; the library's own check is
		// host-only and lets a missing Origin through.
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.live.Store(false)
		return
	}
	b := newBrowser(r.Context(), c, func() { s.live.Store(false) })
	select {
	case s.accept <- b:
	case <-b.Done():
		return
	case <-r.Context().Done():
		b.Close()
		return
	}
	<-b.Done()
}

// validToken reports whether any offered subprotocol carries the token. Every
// offer is compared in constant time.
func (s *Server) validToken(r *http.Request) bool {
	ok := 0
	for _, h := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(h, ",") {
			p = strings.TrimSpace(p)
			if t, found := strings.CutPrefix(p, tokenPrefix); found {
				ok |= subtle.ConstantTimeCompare([]byte(t), []byte(s.token))
			}
		}
	}
	return ok == 1
}

func deny(w http.ResponseWriter, code int) {
	http.Error(w, http.StatusText(code), code)
}

func securityHeaders(host string) http.Header {
	csp := strings.Join([]string{
		"default-src 'none'",
		"script-src 'self'",
		"style-src 'self'",
		"connect-src ws://" + host,
		"media-src blob: mediastream:",
		"img-src 'self' data:",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"form-action 'none'",
	}, "; ")
	return http.Header{
		"Content-Security-Policy": {csp},
		"X-Frame-Options":         {"DENY"},
		"Referrer-Policy":         {"no-referrer"},
		"Cache-Control":           {"no-store"},
		"X-Content-Type-Options":  {"nosniff"},
	}
}
