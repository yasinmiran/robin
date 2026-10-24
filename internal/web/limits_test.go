package web

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/protocol"
)

// serveWith starts s, set up by the caller, and stops it at the end of t.
func serveWith(t *testing.T, s *Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
}

// get sends one GET for path with host over c and reads the response.
func get(t *testing.T, c net.Conn, r *bufio.Reader, host, path string) *http.Response {
	t.Helper()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: "+host+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

// closedWithin reports whether the server closes c within d.
func closedWithin(c net.Conn, r *bufio.Reader, d time.Duration) bool {
	c.SetReadDeadline(time.Now().Add(d))
	_, err := r.ReadByte()
	return errors.Is(err, io.EOF)
}

// TestIdleConnectionsAreClosed: a connection that made its request and went
// quiet is closed after the idle timeout, so nobody can hold the page
// server's connections by opening them and waiting.
func TestIdleConnectionsAreClosed(t *testing.T) {
	s, err := Listen(Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.idle = 200 * time.Millisecond
	serveWith(t, s)

	c, err := net.Dial("tcp", s.host)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := bufio.NewReader(c)
	if resp := get(t, c, r, s.host, "/"); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /: %s", resp.Status)
	}
	if !closedWithin(c, r, 2*time.Second) {
		t.Fatal("an idle connection stayed open past the idle timeout")
	}
}

// TestRefusalClosesTheConnection: a request the server refuses ends its
// connection there, so a wrong Host cannot hold one either.
func TestRefusalClosesTheConnection(t *testing.T) {
	s, _ := start(t)
	c, err := net.Dial("tcp", s.host)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := bufio.NewReader(c)
	resp := get(t, c, r, "attacker.example", "/")
	if resp.StatusCode != http.StatusForbidden || !resp.Close {
		t.Fatalf("wrong Host: %s, close %v; want 403 and the connection closed", resp.Status, resp.Close)
	}
	if !closedWithin(c, r, 2*time.Second) {
		t.Fatal("the connection stayed open after a refusal")
	}
}

// hold opens maxConns connections the way another user of this machine
// could keep them: silent, idle after a request, or stalled inside one.
func hold(t *testing.T, s *Server, how string) {
	t.Helper()
	for range maxConns {
		c, err := net.Dial("tcp", s.host)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		switch how {
		case "idle":
			if resp := get(t, c, bufio.NewReader(c), s.host, "/"); resp.StatusCode != http.StatusOK {
				t.Fatalf("GET /: %s", resp.Status)
			}
		case "stalled":
			io.WriteString(c, "GET / HTTP/1.1\r\nHost: "+s.host+"\r\n")
		}
	}
}

// TestFullServerStillServesTheLaunchLink: another user of this machine can
// fill every connection the page server holds, but the browser's fresh one
// is served at once, so the launch link reaches the user's browser and a
// race for it is seen on stderr rather than left as a tab that never loads.
// The server never holds more than maxConns open while it makes room.
func TestFullServerStillServesTheLaunchLink(t *testing.T) {
	for _, how := range []string{"silent", "idle", "stalled"} {
		t.Run(how, func(t *testing.T) {
			s, _ := start(t)
			hold(t, s, how)
			// Every held connection is in the server's hands before the
			// browser's arrives, as it would be for one held in advance.
			waitOpen(t, s, maxConns)

			c, err := net.Dial("tcp", s.host)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(time.Second))
			io.WriteString(c, "GET "+strings.TrimPrefix(s.LaunchURL(), "http://"+s.host)+" HTTP/1.1\r\nHost: "+s.host+"\r\n\r\n")
			resp, err := http.ReadResponse(bufio.NewReader(c), nil)
			if err != nil {
				t.Fatalf("the launch link on a full server: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/#"+token(s) {
				t.Fatalf("the launch link on a full server: %s to %q", resp.Status, resp.Header.Get("Location"))
			}
			if peak := s.conns.Peak(); peak > maxConns {
				t.Fatalf("the server held %d connections at once, the limit is %d", peak, maxConns)
			}
		})
	}
}

// TestFullServerKeepsTheWebsocket: making room never costs the page its
// websocket, however many connections arrive after it.
func TestFullServerKeepsTheWebsocket(t *testing.T) {
	s, _ := start(t)
	ws, b := connect(t, s)
	hold(t, s, "idle")
	hold(t, s, "silent")
	if err := b.Send(protocol.Message{Type: protocol.TypeConfig}); err != nil {
		t.Fatalf("the websocket after %d more connections: %v", 2*maxConns, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := ws.Read(ctx); err != nil {
		t.Fatalf("the websocket after %d more connections: %v", 2*maxConns, err)
	}
}

// waitOpen waits until the server counts n open connections.
func waitOpen(t *testing.T, s *Server, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); s.conns.Open() < n; {
		if time.Now().After(deadline) {
			t.Fatalf("the server counts %d connections, want %d", s.conns.Open(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
