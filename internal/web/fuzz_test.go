package web

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/yasinmiran/robin/internal/protocol"
)

// FuzzDecode feeds the websocket's frames from the page: what decode
// takes is a text frame of a type the page may send, and a fixed point of
// marshalling, so the bridge relays what the page meant.
func FuzzDecode(f *testing.F) {
	for _, s := range []string{
		`{"type":"offer","sdp":"v=0\r\n"}`,
		`{"type":"candidate","candidate":{"candidate":"candidate:1 1 udp 1 10.0.0.1 5 typ host","sdpMid":"0"}}`,
		`{"type":"detach","detach":"keep"}`,
		`{"type":"bye"}`,
		`{"type":"macros","macroChange":{"delete":"m1"}}`,
		`{"type":"start","start":{"version":2}}`,
		`{"type":"config"}`,
		`{"type":"ping"}`,
		`{"Type":"bye"}`,
		`{"type":"bye","type":"start"}`,
		`null`, `[]`, `x`,
	} {
		f.Add(true, []byte(s))
	}
	f.Add(false, []byte(`{"type":"bye"}`))
	allowed := []protocol.Type{protocol.TypeOffer, protocol.TypeCandidate, protocol.TypeDetach, protocol.TypeBye, protocol.TypeMacros}
	f.Fuzz(func(t *testing.T, text bool, p []byte) {
		typ := websocket.MessageBinary
		if text {
			typ = websocket.MessageText
		}
		m, err := decode(typ, p)
		if err != nil {
			return
		}
		if !text || !slices.Contains(allowed, m.Type) {
			t.Fatalf("took %+v from a frame of type %v", m, typ)
		}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("a decoded message does not marshal: %v", err)
		}
		again, err := decode(websocket.MessageText, b)
		if err != nil {
			t.Fatalf("decode refuses what it decoded, %s: %v", b, err)
		}
		if b2, _ := json.Marshal(again); !bytes.Equal(b, b2) {
			t.Fatalf("not a fixed point:\n%s\n%s", b, b2)
		}
	})
}

// The fuzzer cannot guess the server's secrets, so its inputs name them:
// these placeholders become the listener's host, the token and the launch
// secret before a request is made.
const (
	hostMark   = "{host}"
	tokenMark  = "{token}"
	launchMark = "{launch}"
)

// FuzzRequest makes requests of the page server with any Host, Origin,
// offered subprotocols and path. Whatever it is asked, the server answers
// with its security headers, says nothing of the token but in the one
// redirect of the launch link, refuses every request whose Host is not
// its own, opens no websocket without the page's exact Origin and the
// token, and serves only the page's own files.
func FuzzRequest(f *testing.F) {
	for _, tc := range [][4]string{
		{hostMark, "", "", "/"},
		{hostMark, "", "", "/index.html"},
		{hostMark, "", "", "/app.js"},
		{hostMark, "", "", "/test/bulk.test.js"},
		{hostMark, "", "", "/package.json"},
		{hostMark, "", "", "/../server.go"},
		{hostMark, "", "", "/open/" + launchMark},
		{hostMark, "", "", "/open/" + launchMark + "x"},
		{hostMark, "http://" + hostMark, "robin, token." + tokenMark, "/ws"},
		{hostMark, "http://" + hostMark, "robin,token." + tokenMark + "0", "/ws"},
		{hostMark, "http://" + hostMark, tokenMark, "/ws"},
		{hostMark, "null", "robin, token." + tokenMark, "/ws"},
		{hostMark, "", "robin, token." + tokenMark, "/ws"},
		{"localhost", "http://localhost", "token." + tokenMark, "/ws"},
		{"attacker.example", "", "", "/open/" + launchMark},
		{"", "", "", "/"},
	} {
		f.Add(tc[0], tc[1], tc[2], tc[3])
	}
	s, err := Listen(Options{})
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { s.ln.Close() })
	h := s.handler()
	fill := strings.NewReplacer(hostMark, s.host, tokenMark, s.token, launchMark, s.launch)
	f.Fuzz(func(t *testing.T, host, origin, protocols, p string) {
		s.launchSpent.Store(false)
		s.live.Store(false)
		host, origin, protocols, p = fill.Replace(host), fill.Replace(origin), fill.Replace(protocols), fill.Replace(p)
		u, err := url.ParseRequestURI(p)
		if err != nil {
			return
		}
		r := &http.Request{Method: "GET", URL: u, Host: host, Header: http.Header{}, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1}
		r = r.WithContext(t.Context())
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if protocols != "" {
			r.Header.Set("Sec-WebSocket-Protocol", protocols)
			r.Header.Set("Connection", "Upgrade")
			r.Header.Set("Upgrade", "websocket")
			r.Header.Set("Sec-WebSocket-Version", "13")
			r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		res := w.Result()

		for k, v := range s.headers {
			if got := res.Header.Values(k); !slices.Equal(got, v) {
				t.Fatalf("%s is %q, want %q", k, got, v)
			}
		}
		launched := host == s.host && u.Path == "/open/"+s.launch && res.StatusCode == http.StatusSeeOther
		for k, v := range res.Header {
			if k == "Location" && launched {
				continue
			}
			if strings.Contains(strings.Join(v, ","), s.token) {
				t.Fatalf("header %s carries the token", k)
			}
		}
		if bytes.Contains(w.Body.Bytes(), []byte(s.token)) {
			t.Fatal("the body carries the token")
		}
		if launched && res.Header.Get("Location") != "/#"+s.token {
			t.Fatalf("the launch link went to %q", res.Header.Get("Location"))
		}
		if host != s.host && res.StatusCode != http.StatusForbidden {
			t.Fatalf("Host %q got status %d", host, res.StatusCode)
		}
		if res.StatusCode == http.StatusSwitchingProtocols || s.live.Load() {
			t.Fatal("a recorder cannot take a websocket, yet one opened")
		}
		if u.Path == "/ws" && res.StatusCode != http.StatusForbidden &&
			(origin != "http://"+s.host || !offersToken(protocols, s.token)) {
			t.Fatalf("a websocket without the page's Origin and token got status %d", res.StatusCode)
		}
		if res.StatusCode == http.StatusOK {
			name := strings.TrimPrefix(path.Clean(u.Path), "/")
			if name == "" {
				name = "index.html"
			}
			want, err := fs.ReadFile(static, path.Join("static", name))
			if err != nil || !bytes.Equal(w.Body.Bytes(), want) {
				t.Fatalf("%q served %d bytes that are not a page file", u.Path, w.Body.Len())
			}
		}
	})
}

// offersToken reports whether a Sec-WebSocket-Protocol header offers the
// token, as the page writes it.
func offersToken(header, token string) bool {
	for _, p := range strings.Split(header, ",") {
		if strings.TrimSpace(p) == tokenPrefix+token {
			return true
		}
	}
	return false
}
