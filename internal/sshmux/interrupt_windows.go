package sshmux

import (
	"context"
	"os/exec"
	"syscall"
)

// shielded is an unstarted ssh in a process group of its own. A console's
// Ctrl+C reaches every process attached to it, and an ssh with a
// connection of its own would die of it before the CLI could tell the host
// anything; a new process group starts with Ctrl+C turned off, so the
// connection outlives the keystroke and the CLI ends the session over it.
// The group still shares the console, where ssh asks for passwords.
func shielded(ctx context.Context, ssh string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, ssh, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	return cmd
}
