package gui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/protocol"
	"github.com/yasinmiran/robin/internal/sshmux"
	"github.com/yasinmiran/robin/internal/sshmux/sshtest"
)

// localhost is this machine over sshtest's throwaway sshd: the probe runs
// here for real, reads only, and finds no kept session of this test's.
func localhost(t *testing.T) (Options, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	s := sshtest.Start(t)
	var stdout, stderr bytes.Buffer
	return Options{
		Dest: s.Dest, SSHArgs: s.Args, Stdout: &stdout, Stderr: &stderr,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, &stdout, &stderr
}

func TestListOverSSH(t *testing.T) {
	o, stdout, stderr := localhost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := List(ctx, o); err != nil {
		t.Fatal(err)
	}
	// Whatever this machine keeps is listed one per line, else said so.
	if lines := strings.TrimSpace(stdout.String()); lines == "" {
		if !strings.Contains(stderr.String(), "no kept sessions on "+o.Dest) {
			t.Fatalf("stdout empty and stderr %q", stderr.String())
		}
	} else {
		for _, l := range strings.Split(lines, "\n") {
			if !strings.Contains(l, "spawn") && !strings.Contains(l, "attach") {
				t.Errorf("line %q does not describe a session", l)
			}
		}
	}
}

func TestEndOverSSHRefusesAnUnknownSession(t *testing.T) {
	o, _, _ := localhost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := End(ctx, o, "zz99-not-a-session")
	if err == nil || !strings.Contains(err.Error(), "no kept session zz99-not-a-session") {
		t.Fatalf("End of nothing: %v", err)
	}
}

func TestConnectReportsABadDestination(t *testing.T) {
	o, _, _ := localhost(t)
	o.Dest = "-bad"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := List(ctx, o); err == nil || !strings.Contains(err.Error(), "invalid destination") {
		t.Fatalf("List to a bad destination: %v", err)
	}
	if err := End(ctx, o, ""); err == nil {
		t.Fatal("End to a bad destination succeeded")
	}
	if err := Run(ctx, o); err == nil {
		t.Fatal("Run to a bad destination succeeded")
	}
}

// endScript stands in for the agent on the host: it records its arguments
// and exits as told.
func endScript(t *testing.T, exit int) (path, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	path = filepath.Join(dir, "robin-agent")
	script := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %s\nexit %d\n", calls, exit)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path, calls
}

// TestEndFallsBackToTheAgentsEndCommand: a bye the agent never answers is
// followed by the agent's end command over the host connection, and an
// exit saying there is no such session counts as done.
func TestEndFallsBackToTheAgentsEndCommand(t *testing.T) {
	o, _, stderr := localhost(t)
	o, log := o.fill()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	m, err := sshmux.Dial(ctx, o.Dest, sshmux.Options{Args: o.SSHArgs, Stderr: o.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	h := &host{o: o, log: log, m: m}
	defer h.close()
	until := time.Now().Add(time.Hour)

	for _, tc := range []struct {
		name  string
		exit  int
		calls string // a failed end is tried once more over a fresh connection
		err   string
	}{
		{"ended", 0, "end ab12", ""},
		{"already gone", protocol.ExitNoSession, "end ab12", ""},
		{"failed", 1, "end ab12\nend ab12", "could not end session ab12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, calls := endScript(t, tc.exit)
			ag, f := agentOverPipes(t)
			go func() {
				if m := <-f.calls; m.Type == protocol.TypeBye {
					f.exit() // the proxy dies with the bye unanswered
				}
			}()
			err := h.end(ag, "ab12", until, path)
			if (err != nil) != (tc.err != "") || (err != nil && !strings.Contains(err.Error(), tc.err)) {
				t.Fatalf("end: %v, want %q", err, tc.err)
			}
			if b, _ := os.ReadFile(calls); strings.TrimSpace(string(b)) != tc.calls {
				t.Fatalf("the host ran %q, want %q", b, tc.calls)
			}
		})
	}
	if !strings.Contains(stderr.String(), "ending the kept session") {
		t.Errorf("stderr: %q", stderr.String())
	}

	// A desktop that goes with its controller needs no second trip.
	path, calls := endScript(t, 0)
	ag, f := agentOverPipes(t)
	go func() {
		<-f.calls
		f.exit()
	}()
	if err := h.end(ag, "ab12", time.Time{}, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(calls); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the end command ran for a desktop that was not kept")
	}
}

// TestAbandonEndsTheDesktopAndKeepsTheError: an error that stops the CLI
// after Ready ends the kept desktop too, and the error says when that
// failed as well.
func TestAbandonEndsTheDesktopAndKeepsTheError(t *testing.T) {
	o, _, _ := localhost(t)
	o, log := o.fill()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	m, err := sshmux.Dial(ctx, o.Dest, sshmux.Options{Args: o.SSHArgs, Stderr: o.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	h := &host{o: o, log: log, m: m}
	defer h.close()
	ready := &protocol.Ready{ID: "ab12", Keep: 3600}
	cause := errors.New("page server stopped")

	// The agent answers the bye: nothing else to do, the error is untouched.
	path, calls := endScript(t, 0)
	ag, f := agentOverPipes(t)
	go func() {
		if m := <-f.calls; m.Type == protocol.TypeBye {
			f.conn.Send(protocol.Message{Type: protocol.TypeEnded, Error: "the CLI ended the session"})
			f.outW.Close()
		}
	}()
	if err := h.abandon(cause, ag, ready, path); err != cause {
		t.Fatalf("abandon with bye answered: %v", err)
	}
	if _, err := os.Stat(calls); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the end command ran though bye was answered")
	}

	// Nothing answers and the end command fails: both reasons come back.
	path, calls = endScript(t, 1)
	ag, f = agentOverPipes(t)
	go func() {
		<-f.calls
		f.exit()
	}()
	err = h.abandon(cause, ag, ready, path)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "could not end session ab12") {
		t.Fatalf("abandon with the end failing: %v", err)
	}
	if b, _ := os.ReadFile(calls); !strings.Contains(string(b), "end ab12") {
		t.Fatalf("the host ran %q", b)
	}
}

// TestReconnectEndsTheSessionWhenItsContextEnds: Ctrl+C during a redial
// ends the kept desktop over a fresh connection, and reconnect returns no
// agent to go on with, which is how Run tells it from a reconnect.
func TestReconnectEndsTheSessionWhenItsContextEnds(t *testing.T) {
	o, _, _ := localhost(t)
	o, log := o.fill()
	h := &host{o: o, log: log}
	defer h.close()
	path, calls := endScript(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ag, ready, err := h.reconnect(ctx, path, "ab12", time.Now().Add(time.Hour), &tunnel{log: log})
	if ag != nil || ready != nil || err != nil {
		t.Fatalf("reconnect after Ctrl+C: %v, %v, %v; want no agent and no error", ag, ready, err)
	}
	if b, _ := os.ReadFile(calls); strings.TrimSpace(string(b)) != "end ab12" {
		t.Fatalf("the host ran %q, want the end command", b)
	}
}

// TestAgentOutlivesCtrlC: Ctrl+C ends Run's context, which startAgent
// does not take, and without a ControlMaster sends the agent's own ssh
// SIGINT. The connection stays up, so the CLI ends the session over it
// with bye rather than with the end command over a fresh login.
func TestAgentOutlivesCtrlC(t *testing.T) {
	o, _, _ := localhost(t)
	o.NoMux = true
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	m, err := dial(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	echo := filepath.Join(t.TempDir(), "robin-agent")
	if err := os.WriteFile(echo, []byte("#!/bin/sh\nexec cat\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ag, err := startAgent(m, echo, serveArgs{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer ag.stop()
	roundTrip := func(what string) {
		t.Helper()
		if err := ag.conn.Send(protocol.Message{Type: protocol.TypeBye, Error: what}); err != nil {
			t.Fatalf("send %s: %v", what, err)
		}
		if m, err := ag.conn.Recv(ctx); err != nil || m.Error != what {
			t.Fatalf("echo of %s: %+v, %v", what, m, err)
		}
	}
	roundTrip("before")
	if err := sshtest.CtrlC(ag.proc); err != nil {
		t.Fatalf("Ctrl+C: %v", err)
	}
	roundTrip("after")
}
