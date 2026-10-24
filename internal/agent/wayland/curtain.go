//go:build linux

package wayland

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// Mutter's own display configuration interface, which still carries the
// gamma calls gnome-settings-daemon used for night light. No portal offers
// anything like them.
const (
	displayConfigName  = "org.gnome.Mutter.DisplayConfig"
	displayConfigPath  = "/org/gnome/Mutter/DisplayConfig"
	displayConfigIface = displayConfigName
)

// MutterStep is the keeper's kind for a MutterUndo.
const MutterStep = "mutter-gamma"

// MutterUndo puts a GNOME session's screens back as a curtain found them.
type MutterUndo struct {
	Ramps []MutterRamp `json:"ramps"`
}

// MutterRamp is the gamma ramp of one of mutter's CRTCs, by its ID.
type MutterRamp struct {
	CRTC  uint32   `json:"crtc"`
	Red   []uint16 `json:"red"`
	Green []uint16 `json:"green"`
	Blue  []uint16 `json:"blue"`
}

// curtainCheck is how often a drawn curtain makes sure its ramps are still
// zero: mutter applies night light's ramps whenever the temperature changes.
const curtainCheck = time.Second

var (
	// ErrNoDisplayConfig means the compositor offers no way to set the
	// screens' gamma, as on KDE Plasma.
	ErrNoDisplayConfig = errors.New("wayland: the desktop offers no way to darken its screens: only GNOME's mutter lets a program set their gamma")
	// ErrNoGamma means mutter lists no screen with a gamma ramp, as in a
	// headless session.
	ErrNoGamma = errors.New("wayland: mutter lists no screen whose gamma a program can set")
)

// Curtain blacks out a GNOME Wayland session's screens by zeroing the gamma
// ramp of every CRTC that drives one, through mutter's DisplayConfig.
// Gamma applies on the way to the monitor, after the frames the screencast
// reads, so the stream is unaffected.
type Curtain struct {
	conn *dbus.Conn
	obj  dbus.BusObject
	hold func(MutterUndo) error
	log  *slog.Logger

	mu    sync.Mutex
	saved map[uint32]MutterRamp // each CRTC's ramp as found
	drawn bool
	stop  chan struct{}
	done  chan struct{}
}

// OpenCurtain readies a curtain for the user's session. Before every change
// to the ramps the curtain hands hold what would undo it, and makes no
// change hold refuses. It returns ErrNoDisplayConfig or ErrNoGamma for a
// session it cannot darken.
func OpenCurtain(hold func(MutterUndo) error, log *slog.Logger) (*Curtain, error) {
	conn, err := sessionBus()
	if err != nil {
		return nil, err
	}
	c := &Curtain{conn: conn, obj: conn.Object(displayConfigName, displayConfigPath), hold: hold, log: log, saved: map[uint32]MutterRamp{}}
	if err := c.check(); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *Curtain) check() error {
	var owned bool
	if err := c.conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, displayConfigName).Store(&owned); err != nil {
		return fmt.Errorf("wayland: ask the session bus for mutter: %w", err)
	}
	if !owned {
		return ErrNoDisplayConfig
	}
	serial, crtcs, err := resources(c.obj)
	if err != nil {
		return err
	}
	for _, crtc := range crtcs {
		if r, err := gamma(c.obj, serial, crtc); err == nil && len(r.Red) > 0 {
			return nil
		}
	}
	return ErrNoGamma
}

// Draw darkens every screen and keeps them dark until Lift.
func (c *Curtain) Draw() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.drawn {
		return nil
	}
	if err := c.darken(); err != nil {
		setRamps(c.obj, c.undo().Ramps)
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
	err := setRamps(c.obj, c.undo().Ramps)
	if err == nil {
		clear(c.saved)
	}
	return err
}

// Close lifts the curtain and leaves the bus.
func (c *Curtain) Close() error {
	err := c.Lift()
	c.conn.Close()
	return err
}

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
	serial, crtcs, err := resources(c.obj)
	if err != nil {
		return err
	}
	var lit []uint32
	for _, crtc := range crtcs {
		r, err := gamma(c.obj, serial, crtc)
		if err != nil {
			return err
		}
		if len(r.Red) == 0 {
			continue
		}
		_, seen := c.saved[crtc]
		dark := isZero(r.Red) && isZero(r.Green) && isZero(r.Blue)
		if seen && dark {
			continue
		}
		if !dark || !seen {
			c.saved[crtc] = r
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
		if err := c.obj.Call(displayConfigIface+".SetCrtcGamma", 0, serial, crtc, zero, zero, zero).Err; err != nil {
			return fmt.Errorf("wayland: darken CRTC %d: %w", crtc, err)
		}
	}
	return nil
}

func (c *Curtain) undo() MutterUndo {
	var u MutterUndo
	for _, r := range c.saved {
		u.Ramps = append(u.Ramps, r)
	}
	slices.SortFunc(u.Ramps, func(a, b MutterRamp) int { return int(a.CRTC) - int(b.CRTC) })
	return u
}

// UndoMutter is the keeper's undo for a MutterUndo.
func UndoMutter(ctx context.Context, data json.RawMessage) error {
	var u MutterUndo
	if err := json.Unmarshal(data, &u); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		conn, err := sessionBus()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		done <- setRamps(conn.Object(displayConfigName, displayConfigPath), u.Ramps)
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
func setRamps(obj dbus.BusObject, ramps []MutterRamp) error {
	if len(ramps) == 0 {
		return nil
	}
	serial, crtcs, err := resources(obj)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range ramps {
		if !slices.Contains(crtcs, r.CRTC) {
			continue
		}
		now, err := gamma(obj, serial, r.CRTC)
		if err != nil || len(now.Red) != len(r.Red) || len(r.Green) != len(r.Red) || len(r.Blue) != len(r.Red) {
			continue
		}
		if err := obj.Call(displayConfigIface+".SetCrtcGamma", 0, serial, r.CRTC, r.Red, r.Green, r.Blue).Err; err != nil {
			errs = append(errs, fmt.Errorf("wayland: restore the gamma of CRTC %d: %w", r.CRTC, err))
		}
	}
	return errors.Join(errs...)
}

// The parts of GetResources' reply, in its D-Bus signature
// (ua(uxiiiiiuaua{sv})a(uxiausauaua{sv})a(uxuudu)ii).
type (
	mutterCrtc struct {
		ID                  uint32
		WinsysID            int64
		X, Y, Width, Height int32
		Mode                int32 // -1 when the CRTC drives nothing
		Transform           uint32
		Transforms          []uint32
		Properties          map[string]dbus.Variant
	}
	mutterOutput struct {
		ID         uint32
		WinsysID   int64
		CRTC       int32
		CRTCs      []uint32
		Name       string
		Modes      []uint32
		Clones     []uint32
		Properties map[string]dbus.Variant
	}
	mutterMode struct {
		ID            uint32
		WinsysID      int64
		Width, Height uint32
		Rate          float64
		Flags         uint32
	}
)

// resources lists the CRTCs that drive a screen, with the serial the gamma
// calls must quote.
func resources(obj dbus.BusObject) (uint32, []uint32, error) {
	var (
		serial          uint32
		crtcs           []mutterCrtc
		outputs         []mutterOutput
		modes           []mutterMode
		maxW, maxHeight int32
	)
	if err := obj.Call(displayConfigIface+".GetResources", 0).Store(&serial, &crtcs, &outputs, &modes, &maxW, &maxHeight); err != nil {
		return 0, nil, fmt.Errorf("wayland: ask mutter for its screens: %w", err)
	}
	var active []uint32
	for _, c := range crtcs {
		if c.Mode >= 0 {
			active = append(active, c.ID)
		}
	}
	return serial, active, nil
}

func gamma(obj dbus.BusObject, serial, crtc uint32) (MutterRamp, error) {
	r := MutterRamp{CRTC: crtc}
	if err := obj.Call(displayConfigIface+".GetCrtcGamma", 0, serial, crtc).Store(&r.Red, &r.Green, &r.Blue); err != nil {
		return r, fmt.Errorf("wayland: read the gamma of CRTC %d: %w", crtc, err)
	}
	return r, nil
}

func isZero(ramp []uint16) bool {
	for _, v := range ramp {
		if v != 0 {
			return false
		}
	}
	return true
}
