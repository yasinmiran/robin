//go:build linux

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/yasinmiran/robin/internal/agent/desktop"
	"github.com/yasinmiran/robin/internal/agent/display"
	"github.com/yasinmiran/robin/internal/agent/x11"
	"github.com/yasinmiran/robin/internal/protocol"
)

// attachX11 streams an X display the user already runs, such as the desktop
// they are logged in to at the machine. Closing it leaves that session
// running with everything the agent changed put back.
func attachX11(ctx context.Context, start protocol.Start, log *slog.Logger) (*openedDesktop, error) {
	x, xauth, err := x11.Attach(start.Attach, display.XAuthorityFiles())
	if err != nil {
		return nil, fmt.Errorf("attach to X11 display %s: %w", start.Attach, err)
	}
	if xauth == "" {
		xauth = "none (host-based access)"
	}
	w, h := x.Size()
	log.Info("attached", "display", start.Attach, "xauthority", xauth, "size", fmt.Sprintf("%dx%d", w, h))

	done := make(chan error, 1)
	closing := make(chan struct{})
	go func() {
		<-x.Done()
		select {
		case <-closing:
		default:
			done <- fmt.Errorf("the X server on %s closed the connection", start.Attach)
		}
	}()
	var once sync.Once
	return &openedDesktop{
		desk: attachedX11{x}, mode: protocol.ModeAttach, name: start.Attach, done: done,
		close: func() {
			once.Do(func() {
				close(closing)
				x.Close()
			})
		},
	}, nil
}

// attachedX11 is a display someone else runs: its size is theirs to set.
type attachedX11 struct{ *x11.Conn }

func (a attachedX11) Resize(int, int) (int, int, error) {
	w, h := a.Size()
	return w, h, desktop.ErrFixedSize
}
