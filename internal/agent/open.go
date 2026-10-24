//go:build linux

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/yasinmiran/robin/internal/agent/desktop"
	"github.com/yasinmiran/robin/internal/agent/display"
	"github.com/yasinmiran/robin/internal/agent/wayland"
	"github.com/yasinmiran/robin/internal/agent/x11"
	"github.com/yasinmiran/robin/internal/protocol"
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
	// openCurtain readies a curtain over an attached desktop's screens, or
	// says why there can be none; nil for a spawned desktop.
	openCurtain func(Hold) (curtain, error)
}

// openDesktop spawns a headless display or attaches to a running desktop,
// as Start.Attach asks. ctx bounds the opening only. env is added to the
// environment of anything spawned; stateDir keeps what outlives a session;
// notify reaches the user before Ready.
func openDesktop(ctx context.Context, start protocol.Start, runDir, stateDir string, env []string, notify func(protocol.Notice), log *slog.Logger) (*openedDesktop, error) {
	switch start.Attach {
	case "":
		return spawnX11(ctx, start, runDir, env, log)
	case protocol.AttachWayland:
		return attachWayland(ctx, start, stateDir, notify, log)
	default:
		return attachX11(ctx, start, log)
	}
}

func spawnX11(ctx context.Context, start protocol.Start, runDir string, env []string, log *slog.Logger) (*openedDesktop, error) {
	disp, err := display.Spawn(ctx, display.Options{
		RunDir: runDir, MaxWidth: start.MaxWidth, MaxHeight: start.MaxHeight, Session: start.Session, Env: env, Log: log,
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

// waylandTokenFile holds the portal's restore token in the agent's
// directory. Its name must not start with agentbin.NamePrefix, or deploying
// a new agent would delete it as a stale one.
const waylandTokenFile = "wayland-restore-token"

func attachWayland(ctx context.Context, start protocol.Start, stateDir string, notify func(protocol.Notice), log *slog.Logger) (*openedDesktop, error) {
	token := ""
	if stateDir != "" {
		token = filepath.Join(stateDir, waylandTokenFile)
	}
	w, err := wayland.Open(ctx, wayland.Options{
		TokenFile: token, MaxWidth: start.MaxWidth, MaxHeight: start.MaxHeight, FPS: start.FPS,
		Notify: notify, Log: log,
	})
	if err != nil {
		return nil, fmt.Errorf("attach to the Wayland session: %w", err)
	}
	return &openedDesktop{
		desk: w, mode: protocol.ModeAttach, name: protocol.AttachWayland, done: w.Done(),
		close: func() { w.Close() },
		openCurtain: func(hold Hold) (curtain, error) {
			c, err := wayland.OpenCurtain(hold, log)
			if err != nil {
				return nil, err // not c: a nil *gamma.Curtain would be a non-nil curtain
			}
			return c, nil
		},
	}, nil
}
