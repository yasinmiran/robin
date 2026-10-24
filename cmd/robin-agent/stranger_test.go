//go:build linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasinmiran/robin/internal/agent"
	"github.com/yasinmiran/robin/internal/agent/agenttest"
	"github.com/yasinmiran/robin/internal/protocol"
)

// A second account's robin-agent end, aimed at a kept session through a
// run link to its owner's run root (the only way its end could name that
// session), cannot end it. The owner's 0700 run root refuses it first;
// opened up, the kernel refuses the signal. Either way the session lives
// on, the stranger hears why in words its terminal shows as they are, and
// the owner's end still works.
func TestEndOfAnotherUsersSessionIsRefused(t *testing.T) {
	needXvfb(t)
	s := agenttest.NewStranger(t)
	// The owner's agent directory may be open to others, as a home
	// directory often is; its run root is the agent's own and private.
	exe := filepath.Join(agenttest.OpenDir(t), "robin-agent-test")
	b, err := os.ReadFile("/proc/self/exe")
	if err == nil {
		err = os.WriteFile(exe, b, 0o755)
	}
	if err != nil {
		t.Fatal(err)
	}
	p := serveAgent(t, exe, "-keep", "1h")
	ready, dir := start(p, exe)
	p.killGroup()
	st := waitSession(t, dir, func(rec protocol.Session) bool { return !rec.Connected })
	root := filepath.Dir(dir)
	if err := os.Symlink(root, filepath.Join(filepath.Dir(s.Exe), "run")); err != nil {
		t.Fatal(err)
	}
	end := func(why, want string) {
		t.Helper()
		cmd := s.Command(s.Exe, "end", ready.ID)
		cmd.Env = append(os.Environ(), "ROBIN_AGENT_MAIN=1")
		out, err := cmd.CombinedOutput()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(string(out), want) {
			t.Fatalf("the stranger's end %s: %v, want exit 1 saying %q:\n%s", why, err, want, out)
		}
		agenttest.TerminalSafe(t, "what the stranger's end "+why+" printed", string(out))
		if !alive(st.PID) {
			t.Fatalf("the stranger's end %s ended the session", why)
		}
		if _, err := agent.ReadSession(dir); err != nil {
			t.Fatalf("the session's record after the stranger's end %s: %v", why, err)
		}
	}

	end("through the private run root", "permission denied")

	for path, mode := range map[string]os.FileMode{root: 0o755, dir: 0o755, filepath.Join(dir, agent.SessionFile): 0o644} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	end("through an open run root", "operation not permitted")

	own := exec.Command(exe, "end", ready.ID)
	own.Env = append(os.Environ(), "ROBIN_AGENT_MAIN=1")
	if out, err := own.CombinedOutput(); err != nil {
		t.Fatalf("the owner's end: %v\n%s", err, out)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the run directory after the owner's end: %v", err)
	}
}
