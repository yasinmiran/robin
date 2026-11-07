package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agentbin"
	"github.com/yasinmiran/robin/internal/probe"
	"github.com/yasinmiran/robin/internal/protocol"
	"github.com/yasinmiran/robin/internal/sshmux"
	"github.com/yasinmiran/robin/internal/sshmux/sshtest"
)

// localhost is this machine over sshtest's throwaway sshd, with env set
// in its sessions: the probe runs here for real, reads only, and finds no
// kept session of this test's.
func localhost(t *testing.T, env ...string) (Options, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	s := sshtest.Start(t, env...)
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

// keptOnLocalhost is localhost with the probe's agent directory in one of
// the test's own, holding one kept session, ab12, that runs from agent. A
// sleep stands in for its daemon, so the probe lists the session and
// nothing robin started is signaled.
func keptOnLocalhost(t *testing.T, agent string) (o Options, agentDir string) {
	t.Helper()
	cache := t.TempDir()
	agentDir = filepath.Join(cache, "robin")
	runDir := filepath.Join(agentDir, "run", "ab12")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	daemon := exec.Command("sleep", "300")
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		daemon.Process.Kill()
		daemon.Wait()
	})
	s := spawned("ab12", agent)
	s.PID = daemon.Process.Pid
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "session.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	o, _, _ = localhost(t, "XDG_CACHE_HOME="+cache)
	return o, agentDir
}

// agentsIn lists the agent files in dir.
func agentsIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), agentbin.NamePrefix) {
			names = append(names, e.Name())
		}
	}
	return names
}

// TestEndUsesTheSessionsOwnAgent: robin end of a session another build
// started runs that build's agent, still in place, and uploads nothing,
// both when that agent ends the session and when it says the session is
// already gone.
func TestEndUsesTheSessionsOwnAgent(t *testing.T) {
	const other = agentbin.NamePrefix + "fedcba9876543210"
	for _, tc := range []struct {
		name string
		exit int
		gone bool
	}{
		{"ended", 0, false},
		{"already gone", protocol.ExitNoSession, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, agentDir := keptOnLocalhost(t, other)
			calls := filepath.Join(t.TempDir(), "calls")
			script := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %s\nexit %d\n", calls, tc.exit)
			if err := os.WriteFile(filepath.Join(agentDir, other), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			err := End(ctx, o, "")
			if tc.gone != errors.Is(err, errNoSession) || (!tc.gone && err != nil) {
				t.Fatalf("End: %v, want no session %v", err, tc.gone)
			}
			if b, _ := os.ReadFile(calls); strings.TrimSpace(string(b)) != "end ab12" {
				t.Fatalf("the session's agent ran %q, want end ab12", b)
			}
			if got := agentsIn(t, agentDir); len(got) != 1 || got[0] != other {
				t.Fatalf("the agent directory holds %v after the end, want only %s", got, other)
			}
		})
	}
}

// TestEndPutsItsAgentInPlaceWhenTheSessionsIsGone: with the session's own
// agent file gone, robin end deploys this CLI's agent and ends the session
// with that.
func TestEndPutsItsAgentInPlaceWhenTheSessionsIsGone(t *testing.T) {
	a, err := agentbin.For(runtime.GOARCH)
	if err != nil {
		t.Skipf("no agent for %s in this build (make builds them): %v", runtime.GOARCH, err)
	}
	o, agentDir := keptOnLocalhost(t, agentbin.NamePrefix+"fedcba9876543210")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := End(ctx, o, "ab12"); err != nil {
		t.Fatal(err)
	}
	if got := agentsIn(t, agentDir); len(got) != 1 || got[0] != a.Name() {
		t.Fatalf("the agent directory holds %v, want %s", got, a.Name())
	}
	if _, err := os.Stat(filepath.Join(agentDir, "run", "ab12")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the session's run dir is still there: %v", err)
	}
}

// TestKeptAgentPath: the session file names its agent; only a bare agent
// file name in the agent directory is run.
func TestKeptAgentPath(t *testing.T) {
	const name = agentbin.NamePrefix + "0123456789abcdef"
	h := &host{report: &probe.Report{AgentDir: "/home/u/.cache/robin"}}
	for agent, want := range map[string]string{
		name:                                   "/home/u/.cache/robin/" + name,
		"":                                     "",
		agentbin.NamePrefix:                    "",
		"sh":                                   "",
		agentbin.NamePrefix + "x/../../bin/sh": "",
		"../" + name:                           "",
	} {
		got, ok := h.keptAgentPath(protocol.Session{Agent: agent})
		if got != want || ok != (want != "") {
			t.Errorf("agent %q: %q, %v; want %q", agent, got, ok, want)
		}
	}
	h.report.AgentDir = ""
	if _, ok := h.keptAgentPath(protocol.Session{Agent: name}); ok {
		t.Error("an agent path with no agent directory")
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

// TestInterruptedReconnectLeavesASessionAnotherCLIHolds: once a redial is
// told the session is busy, another controller may have it, so Ctrl+C in
// this CLI must not end the desktop under it, even when the tries after
// that reach no agent. It says so and how to end it instead.
func TestInterruptedReconnectLeavesASessionAnotherCLIHolds(t *testing.T) {
	o, _, _ := localhost(t)
	o, log := o.fill()
	busy := `{"type":"error","error":"another CLI is connected","code":"` + string(protocol.CodeBusy) + `"}`
	for _, tc := range []struct {
		name string
		// rest is what the agent does from the second try on.
		rest string
	}{
		{"busy every time", "read line\n\tprintf '%s\\n' '" + busy + "'\n\tcat >/dev/null"},
		{"busy, then gone", "exit 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &host{o: o, log: log}
			defer h.close()
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			path := filepath.Join(dir, "robin-agent")
			script := "#!/bin/sh\necho \"$1\" >> " + calls + "\n[ \"$1\" = serve ] || exit 0\n" +
				"if [ \"$(grep -c serve " + calls + ")\" -gt 1 ]; then\n\t" + tc.rest + "\n\texit\nfi\n" +
				"read line\nprintf '%s\\n' '" + busy + "'\ncat >/dev/null\n"
			if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type result struct {
				ag  *agentProc
				err error
			}
			done := make(chan result, 1)
			go func() {
				ag, _, err := h.reconnect(ctx, path, "ab12", time.Now().Add(time.Hour), &tunnel{log: log})
				done <- result{ag, err}
			}()
			// The third try means the second one's answer has been taken in.
			for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				if b, _ := os.ReadFile(calls); strings.Count(string(b), "serve") >= 3 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("reconnect did not try three times")
				}
			}
			cancel()
			r := <-done
			if r.ag != nil || r.err == nil || !strings.Contains(r.err.Error(), "another robin gui") || !strings.Contains(r.err.Error(), "robin end "+o.Dest+" ab12") {
				t.Fatalf("Ctrl+C with the session busy: %v, %v; want it left, with how to end it", r.ag, r.err)
			}
			if b, _ := os.ReadFile(calls); strings.Contains(string(b), "end") {
				t.Fatalf("the host ran %q: the end command ran on a session another CLI holds", b)
			}
		})
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
