//go:build linux

package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode"

	"github.com/yasinmiran/robin/internal/agent/agenttest"
)

// strangerSocketEnv names the socket the stranger's side of a test dials.
const strangerSocketEnv = "ROBIN_TEST_STRANGER_SOCKET"

// hostile is what the stranger writes once connected: a message the
// daemon would act on, behind escape sequences a terminal would.
const hostile = "\x1b]0;robin\x07\x1b[2J" + `{"type":"start","start":{"version":2}}` + "\n"

// strangerDial is the stranger's side: it dials the socket, sends hostile,
// and prints what came of it for the test to read.
func strangerDial() {
	c, err := net.Dial("unix", os.Getenv(strangerSocketEnv))
	if err != nil {
		fmt.Printf("stranger: dial eacces=%v: %v\n", errors.Is(err, syscall.EACCES), err)
		return
	}
	defer c.Close()
	c.Write([]byte(hostile))
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	b, err := io.ReadAll(c)
	fmt.Printf("stranger: read %d bytes: %v\n", len(b), err)
}

// syncLog collects a logger's output while the test reads it.
type syncLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// terminalSafe fails the test when s holds anything a terminal would act
// on: the daemon's log reaches the user's terminal through the proxy.
func terminalSafe(t *testing.T, what, s string) {
	t.Helper()
	for _, r := range s {
		if r != '\n' && (unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r)) {
			t.Fatalf("%s holds %U: %q", what, r, s)
		}
	}
}

// TestAnotherUserCannotReachTheControlSocket: a second account on the
// host is kept from the session's socket by its 0700 run directory, and
// where that directory was opened up, by the peer check, which turns it
// away before reading a byte. What the daemon logs of it, which the CLI
// shows its user, holds nothing the stranger sent.
func TestAnotherUserCannotReachTheControlSocket(t *testing.T) {
	if agenttest.IsStranger() {
		strangerDial()
		return
	}
	s := agenttest.NewStranger(t)
	dir := filepath.Join(agenttest.OpenDir(t), "s1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var logged syncLog
	ln, err := Listen(dir, "a", slog.New(slog.NewTextHandler(&logged, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	sock := filepath.Join(dir, SocketFile)
	dial := func() string { return s.RunTest(t, t.Name(), strangerSocketEnv+"="+sock) }

	if out := dial(); !strings.Contains(out, "stranger: dial eacces=true") {
		t.Fatalf("through the 0700 run directory:\n%s", out)
	}
	if l := logged.String(); l != "" {
		t.Fatalf("a connection the directory refused reached the daemon:\n%s", l)
	}

	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sock, 0o666); err != nil {
		t.Fatal(err)
	}
	if out := dial(); !strings.Contains(out, "stranger: read 0 bytes") {
		t.Fatalf("through an open run directory:\n%s", out)
	}
	want := fmt.Sprintf("connection from uid %d, not %d", s.UID, os.Getuid())
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logged.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("the daemon's log does not say %q:\n%s", want, logged.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// One line, the refusal: nothing of what the stranger sent was read.
	if l := logged.String(); !strings.Contains(l, "controller refused") || strings.Count(l, "\n") != 1 {
		t.Fatalf("the daemon's log:\n%s", l)
	}
	terminalSafe(t, "the daemon's log", logged.String())

	// The session's own user still gets through.
	dialConn(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := ln.Next(ctx); err != nil {
		t.Fatalf("the owner after the stranger: %v", err)
	}
}

// TestListenRefusesAnotherUsersDirectory: a run directory that belongs to
// someone else is no place for a session's socket, whatever its mode.
func TestListenRefusesAnotherUsersDirectory(t *testing.T) {
	s := agenttest.NewStranger(t)
	dir := filepath.Join(agenttest.OpenDir(t), "s1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dir, int(s.UID), int(s.GID)); err != nil {
		t.Fatal(err)
	}
	_, err := Listen(dir, "a", testLog())
	if want := fmt.Sprintf("belongs to uid %d, not us", s.UID); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Listen in the stranger's directory: %v, want %q", err, want)
	}
	if _, err := os.Lstat(filepath.Join(dir, SocketFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a socket in the stranger's directory: %v", err)
	}
}
