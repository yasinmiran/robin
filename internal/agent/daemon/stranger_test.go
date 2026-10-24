//go:build linux

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agent"
	"github.com/yasinmiran/robin/internal/agent/agenttest"
	"github.com/yasinmiran/robin/internal/protocol"
)

// hostile is what a stranger writes once connected: a start message the
// daemon would act on, behind escape sequences a terminal would.
func hostile(t testing.TB) []byte {
	b, err := json.Marshal(protocol.Message{Type: protocol.TypeStart, Start: &protocol.Start{Version: protocol.Version}})
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte("\x1b]0;robin\x07\x1b[2J"), append(b, '\n')...)
}

// strangerDial is the stranger's side: it dials sock, sends hostile, and
// prints what came of it for the test to read.
func strangerDial(t *testing.T, sock string) {
	c, err := net.Dial("unix", sock)
	if err != nil {
		fmt.Printf("stranger: dial eacces=%v: %v\n", errors.Is(err, syscall.EACCES), err)
		return
	}
	defer c.Close()
	c.Write(hostile(t))
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, err := c.Read(make([]byte, 4096))
	fmt.Printf("stranger: read %d bytes, hung up %v: %v\n", n, agenttest.HungUp(err), err)
}

// A second account on the host is kept from the session's socket by its
// 0700 run directory, and where that directory was opened up, by the peer
// check, which turns it away before reading a byte. What the daemon logs
// of it, which the CLI shows its user, holds nothing the stranger sent.
func TestAnotherUserCannotReachTheControlSocket(t *testing.T) {
	if sock := agenttest.StrangerSocket(); sock != "" {
		strangerDial(t, sock)
		return
	}
	s := agenttest.NewStranger(t)
	dir := filepath.Join(agenttest.OpenDir(t), "s1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var logged agenttest.LockedBuffer
	ln, err := Listen(dir, "a", slog.New(slog.NewTextHandler(&logged, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	sock := filepath.Join(dir, SocketFile)
	dial := func() string { return s.RunTest(t, t.Name(), sock) }

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
	if out := dial(); !strings.Contains(out, "stranger: read 0 bytes, hung up true") {
		t.Fatalf("through an open run directory:\n%s", out)
	}
	// admit logs the refusal before it hangs up, and the stranger's run
	// ended after that: the line is there already.
	want := fmt.Sprintf("connection from uid %d, not %d", s.UID, os.Getuid())
	if l := logged.String(); !strings.Contains(l, "controller refused") || !strings.Contains(l, want) || strings.Count(l, "\n") != 1 {
		t.Fatalf("the daemon's log, want one line saying %q:\n%s", want, l)
	}
	agenttest.TerminalSafe(t, "the daemon's log", logged.String())

	// The session's own user still gets through.
	dialConn(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := ln.Next(ctx); err != nil {
		t.Fatalf("the owner after the stranger: %v", err)
	}
}

// A run directory that belongs to someone else is no place for a
// session's socket, whatever its mode.
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

// End takes no process of another user for a session's daemon, however
// its command line reads: not one that session.json names, as when the
// daemon died and another user's process got its pid, and not one it
// finds by its command line while session.json is missing. It removes the
// directory the daemon left behind instead, and leaves that process be.
func TestEndLeavesAnotherUsersLookalikeAlone(t *testing.T) {
	s := agenttest.NewStranger(t)
	root := filepath.Join(t.TempDir(), "run")
	for _, tc := range []struct {
		name, id string
		named    bool // session.json names the lookalike's pid
	}{
		{"found by its command line", "ab12", false},
		{"named by session.json", "cd34", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pid, exited := lookalike(t, s, tc.id)
			dir := filepath.Join(root, tc.id)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if tc.named {
				if err := agent.WriteSession(dir, protocol.Session{ID: tc.id, PID: pid}); err != nil {
					t.Fatal(err)
				}
			} else {
				abandonSocket(t, dir)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := End(ctx, root, tc.id); err != nil {
				t.Fatalf("End beside the stranger's lookalike: %v", err)
			}
			if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the run directory after End: %v", err)
			}
			select {
			case <-exited:
				t.Fatal("End ended the stranger's process")
			default:
			}
		})
	}
}

// lookalike starts a process of the stranger's whose command line reads
// as the daemon of session id's, which runs until the test ends, and
// returns its pid and a channel closed once it has exited.
func lookalike(t *testing.T, s *agenttest.Stranger, id string) (int, <-chan struct{}) {
	t.Helper()
	cmd := s.Command("sh", "-c", "read line", "daemon", "-id", id)
	in, err := cmd.StdinPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		in.Close()
		cmd.Process.Kill()
		<-exited
	})
	return cmd.Process.Pid, exited
}
