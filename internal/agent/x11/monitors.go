//go:build linux

package x11

import (
	"fmt"

	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/xproto"
)

// Rect is a rectangle of the root window, in pixels.
type Rect struct{ X, Y, W, H int }

// Monitor is a RandR monitor: a named rectangle of the root window, usually
// one physical screen.
type Monitor struct {
	Name string
	Rect
	Primary bool
}

// Monitors returns the display's active monitors in the order RandR lists
// them, or nil when the server predates RandR 1.5 and cannot say.
func (c *Conn) Monitors() ([]Monitor, error) {
	if !c.randr15 {
		return nil, nil
	}
	r, err := randr.GetMonitors(c.x, c.root, true).Reply()
	if err != nil {
		return nil, fmt.Errorf("x11: monitors: %w", err)
	}
	out := make([]Monitor, 0, len(r.Monitors))
	for _, m := range r.Monitors {
		name, err := c.atomName(m.Name)
		if err != nil {
			return nil, fmt.Errorf("x11: monitor name: %w", err)
		}
		out = append(out, Monitor{
			Name:    name,
			Rect:    Rect{X: int(m.X), Y: int(m.Y), W: int(m.Width), H: int(m.Height)},
			Primary: m.Primary,
		})
	}
	return out, nil
}

// MonitorsChanged is signalled when RandR reports a change to the screen,
// its CRTCs or outputs. Monitors defined with RRSetMonitor change without
// any event, so a caller that follows them also has to ask again now and
// then.
func (c *Conn) MonitorsChanged() <-chan struct{} { return c.randrChanged }

func (c *Conn) signalRandR() {
	select {
	case c.randrChanged <- struct{}{}:
	default: // one pending signal is enough
	}
}

// SetViewport limits capture and pointer positions to r, a rectangle of the
// root window; the zero Rect means the whole root. Frames then have r's size,
// and an input position x,y lands on r.X+x, r.Y+y.
func (c *Conn) SetViewport(r Rect) {
	// Under the capture lock, so no Capture compares a frame of the old
	// viewport with one of the new and takes the difference for damage
	// DAMAGE missed.
	c.cap.mu.Lock()
	defer c.cap.mu.Unlock()
	c.viewMu.Lock()
	c.view = r
	c.viewMu.Unlock()
	c.cap.summed = false
	c.dirty.Store(true)
}

// ViewSize returns the size frames are captured at.
func (c *Conn) ViewSize() (w, h int) {
	v := c.viewport()
	return v.W, v.H
}

// viewport is the part of the root that capture and pointer positions refer
// to: the rectangle SetViewport set, cut to the root, or the whole root when
// none is set or nothing of it is left.
func (c *Conn) viewport() Rect {
	w, h := c.Size()
	c.viewMu.Lock()
	v := c.view
	c.viewMu.Unlock()
	whole := Rect{W: w, H: h}
	if v.W <= 0 || v.H <= 0 {
		return whole
	}
	x0, y0 := max(v.X, 0), max(v.Y, 0)
	x1, y1 := min(v.X+v.W, w), min(v.Y+v.H, h)
	if x1 <= x0 || y1 <= y0 {
		return whole
	}
	return Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

// inView reports whether a damaged area overlaps the viewport.
func (c *Conn) inView(a xproto.Rectangle) bool {
	c.viewMu.Lock()
	v := c.view
	c.viewMu.Unlock()
	if v.W <= 0 || v.H <= 0 {
		return true
	}
	ax, ay, aw, ah := int(a.X), int(a.Y), int(a.Width), int(a.Height)
	return ax < v.X+v.W && v.X < ax+aw && ay < v.Y+v.H && v.Y < ay+ah
}

// atomName returns an atom's name, asking the server only the first time.
func (c *Conn) atomName(a xproto.Atom) (string, error) {
	c.atomMu.Lock()
	defer c.atomMu.Unlock()
	if name, ok := c.atomNames[a]; ok {
		return name, nil
	}
	r, err := xproto.GetAtomName(c.x, a).Reply()
	if err != nil {
		return "", err
	}
	if c.atomNames == nil {
		c.atomNames = map[xproto.Atom]string{}
	}
	c.atomNames[a] = r.Name
	return r.Name, nil
}
