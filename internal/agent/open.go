//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/yasinmiran/remotejump/internal/agent/desktop"
	"github.com/yasinmiran/remotejump/internal/agent/display"
	"github.com/yasinmiran/remotejump/internal/agent/x11"
	"github.com/yasinmiran/remotejump/internal/protocol"
)

// openedDesktop is a desktop plus what the session needs to know about it.
type openedDesktop struct {
	desk          desktop.Desktop
	mode          string
	name, session string
	// done fires when the desktop goes away underneath the session; nil
	// when that cannot be observed.
	done  <-chan error
	close func()
}

// openDesktop spawns a headless display or attaches to a running desktop,
// as Start.Attach asks. env is added to the environment of anything spawned.
func openDesktop(ctx context.Context, start protocol.Start, runDir string, env []string, log *slog.Logger) (*openedDesktop, error) {
	switch start.Attach {
	case "":
		return spawnX11(ctx, start, runDir, env)
	case protocol.AttachWayland:
		return attachWayland(ctx, start, log)
	default:
		return attachX11(ctx, start, log)
	}
}

func spawnX11(ctx context.Context, start protocol.Start, runDir string, env []string) (*openedDesktop, error) {
	disp, err := display.Spawn(ctx, display.Options{
		RunDir: runDir, MaxWidth: start.MaxWidth, MaxHeight: start.MaxHeight, Session: start.Session, Env: env,
	})
	if err != nil {
		return nil, fmt.Errorf("spawn display: %w", err)
	}
	x, err := x11.Open(disp.Name, disp.XAuthority)
	if err != nil {
		disp.Close()
		return nil, fmt.Errorf("open display: %w", err)
	}
	return &openedDesktop{
		desk: x, mode: protocol.ModeSpawn, name: disp.Name, session: disp.Session, done: disp.Done(),
		close: func() { x.Close(); disp.Close() },
	}, nil
}

// errAttachUnavailable marks attach modes this build does not have yet.
var errAttachUnavailable = errors.New("attaching to a running desktop is not available in this agent")

func attachWayland(ctx context.Context, start protocol.Start, log *slog.Logger) (*openedDesktop, error) {
	return nil, fmt.Errorf("attach to the Wayland session: %w", errAttachUnavailable)
}
