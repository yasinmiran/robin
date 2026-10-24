//go:build linux

package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/yasinmiran/robin/internal/tail"
)

// Detach modes: how the daemon leaves the SSH session behind.
const (
	// DetachSetsid starts it in a session of its own, which sshd's SIGHUP
	// and the exec channel's pipes never reach. It stays in logind's
	// session scope, so KillUserProcesses=yes would still kill it.
	DetachSetsid = "setsid"
	// DetachSystemdRun starts it as a transient unit of the user's systemd
	// manager, outside the session scope. The unit lives while the user has
	// any session or lingering is enabled.
	DetachSystemdRun = "systemd-run"
)

// startTimeout bounds the wait for a new daemon's socket.
const startTimeout = 15 * time.Second

// Spawn says how to start a daemon.
type Spawn struct {
	// Exe is the agent binary, run as `Exe daemon ...`.
	Exe string
	// RunRoot holds the session directories.
	RunRoot string
	Keep    time.Duration
	Verbose bool
	// Detach is DetachSetsid or DetachSystemdRun.
	Detach string
	// Env is passed to a daemon under systemd-run, whose unit does not
	// inherit this process's environment. Setsid inherits everything.
	Env []string
}

// Start begins a daemon for a new session and returns its id and run
// directory once its socket accepts. The directory is removed when the
// daemon fails to come up.
func Start(ctx context.Context, sp Spawn) (id, dir string, err error) {
	id, err = newID()
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(sp.RunRoot, 0o700); err != nil {
		return "", "", err
	}
	dir = filepath.Join(sp.RunRoot, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", "", err
	}
	cmd := sp.command(id, dir)
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return "", "", fmt.Errorf("start daemon: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	if err := waitSocket(ctx, dir, sp.Detach, exited); err != nil {
		os.RemoveAll(dir)
		return "", "", err
	}
	return id, dir, nil
}

func newID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// command builds the daemon's process. Setsid puts it in a session of its
// own with stdio on /dev/null; it opens its log itself, so systemd-run's
// unit needs no pipe back to us.
func (sp Spawn) command(id, dir string) *exec.Cmd {
	args := daemonArgs(sp.Exe, id, dir, sp.Keep, sp.Verbose)
	if sp.Detach == DetachSystemdRun {
		cmd := exec.Command("systemd-run", systemdRunArgs(id, sp.Env, args)...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
		return cmd
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}

// daemonArgs is the daemon's command line.
func daemonArgs(exe, id, dir string, keep time.Duration, verbose bool) []string {
	args := []string{exe, "daemon", "-id", id, "-dir", dir, "-keep", keep.String()}
	if verbose {
		args = append(args, "-v")
	}
	return args
}

// systemdRunArgs wraps a daemon command line for systemd-run: a quiet,
// collected user unit named after the session, with the environment the
// daemon needs set explicitly.
func systemdRunArgs(id string, env, cmd []string) []string {
	args := []string{"--user", "--quiet", "--collect", "--unit=robin-" + id}
	for _, kv := range env {
		args = append(args, "--setenv="+kv)
	}
	return append(append(args, "--"), cmd...)
}

// waitSocket waits for the daemon's socket, giving up when the daemon
// exits first (a setsid daemon is our child; systemd-run returns once the
// unit started, so its exit says nothing) or the time runs out. The error
// carries the end of the daemon's log, where the reason is.
func waitSocket(ctx context.Context, dir, detach string, exited <-chan error) error {
	sock := filepath.Join(dir, SocketFile)
	deadline := time.After(startTimeout)
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if fi, err := os.Lstat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return nil
		}
		select {
		case <-tick.C:
		case err := <-exited:
			if detach == DetachSystemdRun && err == nil {
				exited = nil
				continue
			}
			return fmt.Errorf("daemon exited before listening (%v)%s", err, logTail(dir))
		case <-deadline:
			return fmt.Errorf("daemon did not listen within %s%s", startTimeout, logTail(dir))
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// logTail is the end of the daemon's log, formatted to follow an error.
func logTail(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, LogFile))
	if err != nil {
		return ""
	}
	t := &tail.Buffer{Max: 4 << 10}
	t.Write(b)
	if s := strings.TrimSpace(t.String()); s != "" {
		return "\ndaemon log:\n" + s
	}
	return ""
}

// Find returns the run directory of session id under runRoot when a daemon
// listens there.
func Find(runRoot, id string) (string, error) {
	if id == "" || strings.ContainsAny(id, "/\\") || id == "." || id == ".." {
		return "", fmt.Errorf("bad session id %q", id)
	}
	dir := filepath.Join(runRoot, id)
	fi, err := os.Lstat(filepath.Join(dir, SocketFile))
	if errors.Is(err, os.ErrNotExist) || (err == nil && fi.Mode()&os.ModeSocket == 0) {
		return "", fmt.Errorf("no session %s on this host", id)
	}
	if err != nil {
		return "", err
	}
	return dir, nil
}
