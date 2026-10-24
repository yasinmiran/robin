package web

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
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

// TestConnectionsAreCapped: past maxConns open connections the server takes
// no more until one closes, so another user of this machine cannot run the
// CLI out of file descriptors through the page server.
func TestConnectionsAreCapped(t *testing.T) {
	s, _ := start(t)
	var held []net.Conn
	for range maxConns {
		c, err := net.Dial("tcp", s.host)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if resp := get(t, c, bufio.NewReader(c), s.host, "/"); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /: %s", resp.Status)
		}
		held = append(held, c)
	}

	extra, err := net.Dial("tcp", s.host)
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	r := bufio.NewReader(extra)
	io.WriteString(extra, "GET / HTTP/1.1\r\nHost: "+s.host+"\r\n\r\n")
	extra.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := r.ReadByte(); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("connection %d was served: %v", maxConns+1, err)
	}

	held[0].Close()
	extra.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(r, nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("after one closed: %v, %v", resp, err)
	}
	resp.Body.Close()
}
