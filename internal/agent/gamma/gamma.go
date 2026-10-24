//go:build linux

// Package gamma draws a curtain over a desktop's screens by zeroing the gamma
// ramp of every CRTC that drives one. Gamma applies on the way to the
// monitor, after the frames a capture reads, so the stream is unaffected.
// x11 and wayland reach the CRTCs each in their own way, as Screens.
package gamma

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

// Ramp is the gamma ramp of one CRTC.
type Ramp struct {
	CRTC  uint32   `json:"crtc"`
	Red   []uint16 `json:"red"`
	Green []uint16 `json:"green"`
	Blue  []uint16 `json:"blue"`
}

// CRTC is a CRTC that drives a screen someone can see.
type CRTC struct {
	ID uint32
	// Output names the screen it drives, to tell the user which one.
	Output string
}

// Screens reaches a desktop's CRTCs.
type Screens interface {
	// CRTCs lists the CRTCs that drive a screen someone can see.
	CRTCs() ([]CRTC, error)
	// Gamma is the ramp crtc has now, empty when nothing may set it.
	Gamma(crtc uint32) (Ramp, error)
	SetGamma(Ramp) error
	Close() error
}

// ErrNoScreen means no CRTC drives a screen anyone can see.
var ErrNoScreen = errors.New("gamma: no screen to darken")

// Uncovered is the error of a curtain that cannot black out every screen:
// it names the screens whose ramp nothing may set.
type Uncovered []string

func (u Uncovered) Error() string {
	if len(u) == 1 {
		return u[0] + " has no gamma ramp a program can set, so the curtain cannot black it out"
	}
	return strings.Join(u, ", ") + " have no gamma ramp a program can set, so the curtain cannot black them out"
}

// check is how often a drawn curtain makes sure its ramps are still zero.
// Nothing reports a gamma change, and night light or a colour manager may
// set the ramps again at any moment.
var check = time.Second

// Curtain blacks out a desktop's screens while it is drawn.
type Curtain struct {
	s       Screens
	hold    func([]Ramp) error
	log     *slog.Logger
	changed chan struct{}

	mu    sync.Mutex
	saved map[uint32]Ramp // each CRTC's ramp as found
	drawn bool
	leak  error
	stop  chan struct{}
	done  chan struct{}
}

// Open readies a curtain over s, which closing the curtain closes. Before
// every change to the ramps the curtain hands hold what undoes it, the ramps
// as it found them, and it makes no change hold refuses. Open returns
// ErrNoScreen, or Uncovered, for screens it cannot black out.
func Open(s Screens, hold func([]Ramp) error, log *slog.Logger) (*Curtain, error) {
	if _, err := survey(s); err != nil {
		return nil, err
	}
	return &Curtain{s: s, hold: hold, log: log, changed: make(chan struct{}, 1), saved: map[uint32]Ramp{}}, nil
}

// Draw darkens every screen and keeps them dark until Lift. It changes
// nothing when some screen cannot be darkened.
func (c *Curtain) Draw() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.drawn {
		return nil
	}
	ramps, err := survey(c.s)
	if err != nil {
		return err
	}
	if err := c.zero(ramps); err != nil {
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
	c.setLeak(nil)
	err := c.restore()
	if err == nil {
		clear(c.saved)
	}
	return err
}

// Close lifts the curtain and closes its Screens.
func (c *Curtain) Close() error {
	err := c.Lift()
	c.s.Close()
	return err
}

// Leak says what the room can still see while the curtain is drawn, as of
// its last check: a screen it could not darken again, or nil.
func (c *Curtain) Leak() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.leak
}

// Changed is signalled whenever Leak changes.
func (c *Curtain) Changed() <-chan struct{} { return c.changed }

// watch keeps the screens dark until stop closes.
func (c *Curtain) watch(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(check)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		c.mu.Lock()
		ramps, err := survey(c.s)
		var uncovered Uncovered
		if err == nil || errors.As(err, &uncovered) {
			// A screen plugged in without a ramp leaves the others dark.
			err = errors.Join(c.zero(ramps), err)
		}
		c.setLeak(err)
		c.mu.Unlock()
	}
}

// setLeak keeps err as the curtain's leak, and says so when that changes.
// c.mu must be held.
func (c *Curtain) setLeak(err error) {
	if errText(err) == errText(c.leak) {
		return
	}
	c.leak = err
	if err != nil {
		c.log.Warn("curtain: the room can see a screen", "err", err)
	} else {
		c.log.Info("curtain: every screen is dark again")
	}
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// survey reads the ramp of every CRTC that drives a screen someone can see.
// With screens that have none it returns the ramps of the others and
// Uncovered.
func survey(s Screens) ([]Ramp, error) {
	crtcs, err := s.CRTCs()
	if err != nil {
		return nil, err
	}
	if len(crtcs) == 0 {
		return nil, ErrNoScreen
	}
	var ramps []Ramp
	var uncovered Uncovered
	for _, crtc := range crtcs {
		r, err := s.Gamma(crtc.ID)
		if err != nil {
			return nil, err
		}
		if len(r.Red) == 0 {
			name := crtc.Output
			if name == "" {
				name = fmt.Sprintf("the screen of CRTC %d", crtc.ID)
			}
			uncovered = append(uncovered, name)
			continue
		}
		ramps = append(ramps, r)
	}
	if uncovered != nil {
		return ramps, uncovered
	}
	return ramps, nil
}

// zero darkens each ramp's CRTC, unless the curtain darkened it already. A
// ramp it has not seen, or one something else set since, is kept first and
// handed to hold, so lifting puts back the newest. c.mu must be held.
func (c *Curtain) zero(ramps []Ramp) error {
	var lit []Ramp
	for _, r := range ramps {
		_, seen := c.saved[r.CRTC]
		if seen && isZero(r.Red) && isZero(r.Green) && isZero(r.Blue) {
			continue
		}
		c.saved[r.CRTC] = r
		lit = append(lit, r)
	}
	if len(lit) == 0 {
		return nil
	}
	if err := c.hold(c.kept()); err != nil {
		return err
	}
	for _, r := range lit {
		zero := make([]uint16, len(r.Red))
		if err := c.s.SetGamma(Ramp{CRTC: r.CRTC, Red: zero, Green: zero, Blue: zero}); err != nil {
			return err
		}
	}
	return nil
}

// kept is every ramp the curtain keeps, by CRTC.
func (c *Curtain) kept() []Ramp {
	var out []Ramp
	for _, r := range c.saved {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Ramp) int { return int(a.CRTC) - int(b.CRTC) })
	return out
}

// restore puts back every ramp kept. c.mu must be held.
func (c *Curtain) restore() error {
	return Restore(c.s, c.kept())
}

// Restore sets each ramp on its CRTC, where that CRTC still drives a screen
// with a ramp of the same size.
func Restore(s Screens, ramps []Ramp) error {
	if len(ramps) == 0 {
		return nil
	}
	crtcs, err := s.CRTCs()
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range ramps {
		if !slices.ContainsFunc(crtcs, func(c CRTC) bool { return c.ID == r.CRTC }) {
			continue
		}
		now, err := s.Gamma(r.CRTC)
		if err != nil || len(now.Red) != len(r.Red) || len(r.Green) != len(r.Red) || len(r.Blue) != len(r.Red) {
			continue
		}
		if err := s.SetGamma(r); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func isZero(ramp []uint16) bool {
	for _, v := range ramp {
		if v != 0 {
			return false
		}
	}
	return true
}
