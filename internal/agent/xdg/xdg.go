// Package xdg finds the user's session directories on the host, for an
// agent that arrives over SSH without them in its environment.
package xdg

import (
	"fmt"
	"os"
)

// RuntimeDir is the user's XDG_RUNTIME_DIR, where their PipeWire, sound
// server and session bus listen. Over SSH it may be unset, so it falls back
// to where systemd-logind puts it.
func RuntimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	return fmt.Sprintf("/run/user/%d", os.Getuid())
}
