//go:build !windows

package sshmux

import (
	"context"
	"os/exec"
)

// shielded is an unstarted ssh that ignores SIGINT. Ctrl+C in a terminal
// reaches every process in the foreground group, and an ssh with a
// connection of its own would die of it before the CLI could tell the host
// anything; ssh keeps an ignore it inherits (its client loop installs a
// SIGINT handler only when the signal is not already ignored), so the
// connection outlives the keystroke and the CLI ends the session over it,
// then closes it. Sessions over a master install their handler
// unconditionally and do die, which the CLI expects. Go starts a child with
// no ignores of its own, so /bin/sh sets this one before it becomes ssh.
func shielded(ctx context.Context, ssh string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "/bin/sh", append([]string{"-c", `trap '' INT; exec "$0" "$@"`, ssh}, args...)...)
}
