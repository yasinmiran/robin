//go:build linux

package x11

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/xproto"
)

// Ramp is the gamma ramp of one CRTC.
type Ramp struct {
	CRTC  uint32   `json:"crtc"`
	Red   []uint16 `json:"red"`
	Green []uint16 `json:"green"`
	Blue  []uint16 `json:"blue"`
}

// GammaUndo puts a display's screens back as a curtain found them: the
// display, the authority file the agent got in with ("" for host-based
// access) and the ramp of every CRTC it darkened.
type GammaUndo struct {
	Display    string `json:"display"`
	XAuthority string `json:"xauthority,omitempty"`
	Ramps      []Ramp `json:"ramps"`
}

// GammaStep is the keeper's kind for a GammaUndo.
const GammaStep = "x11-gamma"

// ErrNoGamma means the display drives no screen whose gamma a client can
// set, as with an X server that has no CRTCs or keeps their ramps to itself.
var ErrNoGamma = errors.New("x11: no screen on this display has a gamma ramp a client can set")

// curtainCheck is how often a drawn curtain makes sure its ramps are still
// zero. RandR reports no gamma changes, and night light or a colour manager
// may set the ramps again at any moment.
const curtainCheck = time.Second

// Curtain blacks out a display's screens by zeroing the gamma ramp of every
// CRTC that drives one. Gamma applies on the way to the monitor, after the
// framebuffer that capture reads, so the stream is unaffected. It works over
// a connection of its own.
type Curtain struct {
	x    *xgb.Conn
	root xproto.Window
	at   GammaUndo // Display and XAuthority
	hold func(GammaUndo) error
	log  *slog.Logger

	mu    sync.Mutex
	saved map[randr.Crtc]Ramp // each CRTC's ramp as found
	drawn bool
	stop  chan struct{}
	done  chan struct{}
}

// OpenCurtain readies a curtain for display, connecting the way the agent's
// own Conn did: with the cookie in xauthority, or "" for host-based access.
// Open that Conn first; see useRandR. Before every change to the ramps the
// curtain hands hold what would undo it, and makes no change hold refuses.
// It returns ErrNoGamma for a display it cannot darken.
func OpenCurtain(display, xauthority string, hold func(GammaUndo) error, log *slog.Logger) (*Curtain, error) {
	x, err := dialAs(display, xauthority)
	if err != nil {
		return nil, fmt.Errorf("x11: connect %s: %w", display, err)
	}
	c := &Curtain{
		x: x, root: xproto.Setup(x).DefaultScreen(x).Root, at: GammaUndo{Display: display, XAuthority: xauthority},
		hold: hold, log: log, saved: map[randr.Crtc]Ramp{},
	}
	if err := useRandR(x); err != nil {
		x.Close()
		return nil, fmt.Errorf("x11: %s: RandR: %w", display, err)
	}
	crtcs, err := gammaCrtcs(x, c.root)
	if err == nil && len(crtcs) == 0 {
		err = ErrNoGamma
	}
	if err != nil {
		x.Close()
		return nil, err
	}
	return c, nil
}

// Draw darkens every screen and keeps them dark until Lift.
func (c *Curtain) Draw() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.drawn {
		return nil
	}
	if err := c.darken(); err != nil {
		c.restore()
		return err
	}
	c.drawn = true
	c.stop, c.done = make(chan struct{}), make(chan struct{})
	go c.watch(c.stop, c.done)
	return nil
}

// Lift gives every screen back the ramp it had, or the last one something
// else set while the curtain was drawn.
func (c *Curtain) Lift() error {
	c.mu.Lock()
	if !c.drawn {
		c.mu.Unlock()
		return nil
	}
	c.drawn = false
	close(c.stop)
	done := c.done
	c.mu.Unlock()
	<-done
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.restore()
	if err == nil {
		clear(c.saved)
	}
	return err
}

// Close lifts the curtain and disconnects.
func (c *Curtain) Close() error {
	err := c.Lift()
	c.x.Close()
	return err
}

// watch keeps the screens dark until stop closes.
func (c *Curtain) watch(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(curtainCheck)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		c.mu.Lock()
		if err := c.darken(); err != nil {
			c.log.Warn("curtain: darken the screens again", "err", err)
		}
		c.mu.Unlock()
	}
}

// darken zeroes the ramp of every CRTC that drives a screen. A ramp it has
// not seen, or one something else set since, is kept first and handed to
// hold, so lifting puts back the newest. c.mu must be held.
func (c *Curtain) darken() error {
	crtcs, err := gammaCrtcs(c.x, c.root)
	if err != nil {
		return err
	}
	var lit []randr.Crtc
	for _, crtc := range crtcs {
		g, err := randr.GetCrtcGamma(c.x, crtc).Reply()
		if err != nil {
			return fmt.Errorf("x11: read the gamma of CRTC %d: %w", crtc, err)
		}
		_, seen := c.saved[crtc]
		dark := isZero(g.Red) && isZero(g.Green) && isZero(g.Blue)
		if seen && dark {
			continue
		}
		if !dark || !seen {
			c.saved[crtc] = Ramp{CRTC: uint32(crtc), Red: g.Red, Green: g.Green, Blue: g.Blue}
		}
		lit = append(lit, crtc)
	}
	if len(lit) == 0 {
		return nil
	}
	if err := c.hold(c.undo()); err != nil {
		return err
	}
	for _, crtc := range lit {
		zero := make([]uint16, len(c.saved[crtc].Red))
		if err := randr.SetCrtcGammaChecked(c.x, crtc, uint16(len(zero)), zero, zero, zero).Check(); err != nil {
			return fmt.Errorf("x11: darken CRTC %d: %w", crtc, err)
		}
	}
	return nil
}

func (c *Curtain) undo() GammaUndo {
	u := c.at
	for _, r := range c.saved {
		u.Ramps = append(u.Ramps, r)
	}
	slices.SortFunc(u.Ramps, func(a, b Ramp) int { return int(a.CRTC) - int(b.CRTC) })
	return u
}

// restore puts back every ramp kept. c.mu must be held.
func (c *Curtain) restore() error {
	return setRamps(c.x, c.root, c.undo().Ramps)
}

// UndoGamma is the keeper's undo for a GammaUndo: it connects to the display
// on its own and puts the ramps back.
func UndoGamma(ctx context.Context, data json.RawMessage) error {
	var u GammaUndo
	if err := json.Unmarshal(data, &u); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		x, err := dialAs(u.Display, u.XAuthority)
		if err != nil {
			done <- fmt.Errorf("x11: connect %s: %w", u.Display, err)
			return
		}
		defer x.Close()
		if err := useRandR(x); err != nil {
			done <- fmt.Errorf("x11: %s: RandR: %w", u.Display, err)
			return
		}
		done <- setRamps(x, xproto.Setup(x).DefaultScreen(x).Root, u.Ramps)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// setRamps sets each ramp on its CRTC, where that CRTC still drives a screen
// with a ramp of the same size.
func setRamps(x *xgb.Conn, root xproto.Window, ramps []Ramp) error {
	crtcs, err := gammaCrtcs(x, root)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range ramps {
		crtc := randr.Crtc(r.CRTC)
		if !slices.Contains(crtcs, crtc) {
			continue
		}
		size, err := randr.GetCrtcGammaSize(x, crtc).Reply()
		if err != nil || int(size.Size) != len(r.Red) || len(r.Green) != len(r.Red) || len(r.Blue) != len(r.Red) {
			continue
		}
		if err := randr.SetCrtcGammaChecked(x, crtc, size.Size, r.Red, r.Green, r.Blue).Check(); err != nil {
			errs = append(errs, fmt.Errorf("x11: restore the gamma of CRTC %d: %w", crtc, err))
		}
	}
	return errors.Join(errs...)
}

// gammaCrtcs lists the CRTCs that drive a screen and have a gamma ramp.
func gammaCrtcs(x *xgb.Conn, root xproto.Window) ([]randr.Crtc, error) {
	res, err := randr.GetScreenResourcesCurrent(x, root).Reply()
	if err != nil {
		return nil, fmt.Errorf("x11: list CRTCs: %w", err)
	}
	var out []randr.Crtc
	for _, crtc := range res.Crtcs {
		info, err := randr.GetCrtcInfo(x, crtc, res.ConfigTimestamp).Reply()
		if err != nil || info.Mode == 0 || len(info.Outputs) == 0 {
			continue
		}
		if size, err := randr.GetCrtcGammaSize(x, crtc).Reply(); err == nil && size.Size > 0 {
			out = append(out, crtc)
		}
	}
	return out, nil
}

func isZero(ramp []uint16) bool {
	for _, v := range ramp {
		if v != 0 {
			return false
		}
	}
	return true
}

// dialAs connects to display as Attach did: with the cookie from xauthority,
// or with none for host-based access.
func dialAs(display, xauthority string) (*xgb.Conn, error) {
	num, err := displayNumber(display)
	if err != nil {
		return nil, err
	}
	cookie := noCookie
	if xauthority != "" {
		if cookie, err = readCookie(xauthority, num); err != nil {
			return nil, err
		}
	}
	nc, err := dialSocket(num)
	if err != nil {
		return nil, err
	}
	x, err := xgb.NewConnNetWithCookieHex(nc, hex.EncodeToString(cookie))
	if err != nil {
		nc.Close()
		return nil, err
	}
	return x, nil
}

// useRandR readies x for RandR requests. randr.Init also writes xgb's global
// event and error tables, which every other connection's reader reads, so
// once a connection to this server has filled them (the agent's Conn), x
// only learns the extension's opcode.
func useRandR(x *xgb.Conn) error {
	ext, err := xproto.QueryExtension(x, uint16(len("RANDR")), "RANDR").Reply()
	if err != nil {
		return err
	}
	if !ext.Present {
		return errors.New("the server has no RandR")
	}
	if _, known := xgb.NewEventFuncs[int(ext.FirstEvent)]; !known {
		if err := randr.Init(x); err != nil {
			return err
		}
	} else {
		x.ExtLock.Lock()
		x.Extensions["RANDR"] = ext.MajorOpcode
		x.ExtLock.Unlock()
	}
	v, err := randr.QueryVersion(x, 1, 3).Reply()
	if err != nil {
		return err
	}
	if v.MajorVersion == 1 && v.MinorVersion < 3 {
		return fmt.Errorf("RandR %d.%d is too old for gamma ramps, want 1.3", v.MajorVersion, v.MinorVersion)
	}
	return nil
}
