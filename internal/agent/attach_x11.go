//go:build linux

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yasinmiran/robin/internal/agent/desktop"
	"github.com/yasinmiran/robin/internal/agent/display"
	"github.com/yasinmiran/robin/internal/agent/x11"
	"github.com/yasinmiran/robin/internal/protocol"
)

// monitorRecheck is how often an attached desktop's monitors are listed
// again. Monitors set with RRSetMonitor change without any RandR event.
const monitorRecheck = time.Second

// attachX11 streams an X display the user already runs, such as the desktop
// they are logged in to at the machine. Closing it leaves that session
// running with everything the agent changed put back.
func attachX11(ctx context.Context, start protocol.Start, log *slog.Logger) (*openedDesktop, error) {
	x, xauth, err := x11.Attach(start.Attach, display.XAuthorityFiles())
	if err != nil {
		return nil, fmt.Errorf("attach to X11 display %s: %w", start.Attach, err)
	}
	cookieFile := xauth
	openCurtain := func(hold Hold) (curtain, error) {
		c, err := x11.OpenCurtain(start.Attach, cookieFile, hold, log)
		if err != nil {
			return nil, err // not c: a nil *gamma.Curtain would be a non-nil curtain
		}
		return c, nil
	}
	if xauth == "" {
		xauth = "none (host-based access)"
	}
	a, err := newAttachedX11(x, start.Monitor, log)
	if err != nil {
		x.Close()
		return nil, fmt.Errorf("attach to X11 display %s: %w", start.Attach, err)
	}
	w, h := x.Size()
	vw, vh := a.Size()
	log.Info("attached", "display", start.Attach, "xauthority", xauth, "size", fmt.Sprintf("%dx%d", w, h),
		"monitor", a.MonitorState().Current, "shown", fmt.Sprintf("%dx%d", vw, vh))

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
		desk: a, mode: protocol.ModeAttach, name: start.Attach, done: done,
		close: func() {
			once.Do(func() {
				close(closing)
				x.Close()
			})
		},
		openCurtain: openCurtain,
	}, nil
}

// attachedX11 is a display someone else runs: its size is theirs to set.
// On a desktop of several monitors it shows one of them at a time, or all
// of them as one picture.
type attachedX11 struct {
	*x11.Conn
	log *slog.Logger

	mu      sync.Mutex
	list    []protocol.Monitor
	current string // a monitor's name, or protocol.MonitorAll
	updates chan struct{}
	closed  bool
}

var _ desktop.MultiMonitor = (*attachedX11)(nil)

// newAttachedX11 shows the monitor want names (see protocol.Start.Monitor)
// and follows the monitors as they change until x closes.
func newAttachedX11(x *x11.Conn, want string, log *slog.Logger) (*attachedX11, error) {
	a := &attachedX11{Conn: x, log: log, updates: make(chan struct{}, 1)}
	list, err := a.listMonitors()
	if err != nil {
		log.Warn("monitors unknown; showing the whole desktop", "err", err)
	}
	current, err := pickMonitor(list, want)
	if err != nil {
		return nil, err
	}
	a.list, a.current = list, current
	x.SetViewport(a.viewportLocked())
	go a.follow()
	return a, nil
}

// Size is the size of what is shown.
func (a *attachedX11) Size() (w, h int) { return a.ViewSize() }

func (a *attachedX11) Resize(int, int) (int, int, error) {
	w, h := a.Size()
	return w, h, desktop.ErrFixedSize
}

func (a *attachedX11) MonitorState() protocol.Monitors {
	a.mu.Lock()
	defer a.mu.Unlock()
	return protocol.Monitors{List: slices.Clone(a.list), Current: a.current}
}

func (a *attachedX11) MonitorUpdates() <-chan struct{} { return a.updates }

func (a *attachedX11) ShowMonitor(name string) error {
	a.mu.Lock()
	if name != protocol.MonitorAll && monitorIndex(a.list, name) < 0 {
		err := fmt.Errorf("no monitor %q; %s", name, describeMonitors(a.list))
		a.mu.Unlock()
		return err
	}
	if name == a.current {
		a.mu.Unlock()
		return nil
	}
	a.current = name
	r := a.viewportLocked()
	a.mu.Unlock()
	a.SetViewport(r)
	a.notify()
	return nil
}

// follow lists the monitors again on every RandR change and every
// monitorRecheck. A monitor that went away gives way to the default one.
func (a *attachedX11) follow() {
	t := time.NewTicker(monitorRecheck)
	defer t.Stop()
	defer func() {
		a.mu.Lock()
		a.closed = true
		close(a.updates)
		a.mu.Unlock()
	}()
	for {
		select {
		case <-a.Done():
			return
		case <-a.MonitorsChanged():
		case <-t.C:
		}
		list, err := a.listMonitors()
		if err != nil {
			a.log.Debug("list monitors", "err", err)
			continue
		}
		a.mu.Lock()
		if slices.Equal(list, a.list) {
			a.mu.Unlock()
			continue
		}
		a.list = list
		if a.current != protocol.MonitorAll && monitorIndex(list, a.current) < 0 {
			gone := a.current
			a.current = defaultMonitor(list)
			a.log.Info("monitor went away", "monitor", gone, "showing", a.current)
		}
		r := a.viewportLocked()
		a.mu.Unlock()
		a.SetViewport(r)
		a.notify()
	}
}

func (a *attachedX11) notify() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	select {
	case a.updates <- struct{}{}:
	default: // one pending update is enough; readers ask for the state
	}
}

// viewportLocked is the rectangle of the root the current choice shows; the
// zero Rect is the whole root. a.mu must be held.
func (a *attachedX11) viewportLocked() x11.Rect {
	i := monitorIndex(a.list, a.current)
	if i < 0 {
		return x11.Rect{}
	}
	m := a.list[i]
	return x11.Rect{X: m.X, Y: m.Y, W: m.W, H: m.H}
}

func (a *attachedX11) listMonitors() ([]protocol.Monitor, error) {
	ms, err := a.Conn.Monitors()
	if err != nil {
		return nil, err
	}
	list := make([]protocol.Monitor, len(ms))
	for i, m := range ms {
		list[i] = protocol.Monitor{Name: m.Name, X: m.X, Y: m.Y, W: m.W, H: m.H, Primary: m.Primary}
	}
	return list, nil
}

// pickMonitor resolves a protocol.Start.Monitor value against the monitors
// the display has: a name, a 1-based number, MonitorPrimary (or empty) for
// the default one, or MonitorAll.
func pickMonitor(list []protocol.Monitor, want string) (string, error) {
	switch want {
	case "", protocol.MonitorPrimary:
		return defaultMonitor(list), nil
	case protocol.MonitorAll:
		return protocol.MonitorAll, nil
	}
	if n, err := strconv.Atoi(want); err == nil {
		if n >= 1 && n <= len(list) {
			return list[n-1].Name, nil
		}
	} else if monitorIndex(list, want) >= 0 {
		return want, nil
	}
	return "", fmt.Errorf("no monitor %q; %s", want, describeMonitors(list))
}

// defaultMonitor is the primary monitor, else the one at the desktop's
// origin, else the first; with no monitors listed it is the whole desktop.
func defaultMonitor(list []protocol.Monitor) string {
	if len(list) == 0 {
		return protocol.MonitorAll
	}
	for _, m := range list {
		if m.Primary {
			return m.Name
		}
	}
	for _, m := range list {
		if m.X == 0 && m.Y == 0 {
			return m.Name
		}
	}
	return list[0].Name
}

func monitorIndex(list []protocol.Monitor, name string) int {
	return slices.IndexFunc(list, func(m protocol.Monitor) bool { return m.Name == name })
}

// describeMonitors lists the monitors the way --monitor accepts them.
func describeMonitors(list []protocol.Monitor) string {
	if len(list) == 0 {
		return `the display lists no monitors; use "all"`
	}
	parts := make([]string, len(list))
	for i, m := range list {
		parts[i] = fmt.Sprintf("%d %s %dx%d+%d+%d", i+1, m.Name, m.W, m.H, m.X, m.Y)
		if m.Primary {
			parts[i] += " (primary)"
		}
	}
	return "the display has " + strings.Join(parts, ", ") + `, or use "all" for the whole desktop`
}
