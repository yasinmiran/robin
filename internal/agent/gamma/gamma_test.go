//go:build linux

package gamma

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"
)

// fakeScreens is a desktop whose CRTCs the test sets, counting the curtain's
// checks.
type fakeScreens struct {
	mu     sync.Mutex
	crtcs  []CRTC
	ramps  map[uint32]Ramp
	checks int // calls to CRTCs
	err    error
	closed bool
}

func newScreens(n int) *fakeScreens {
	f := &fakeScreens{ramps: map[uint32]Ramp{}}
	for i := range uint32(n) {
		f.crtcs = append(f.crtcs, CRTC{ID: i, Output: fmt.Sprintf("DP-%d", i+1)})
		f.ramps[i] = rising(i, 256, 65535)
	}
	return f
}

func rising(crtc uint32, n, top int) Ramp {
	r := make([]uint16, n)
	for i := range r {
		r[i] = uint16(i * top / (n - 1))
	}
	return Ramp{CRTC: crtc, Red: r, Green: slices.Clone(r), Blue: slices.Clone(r)}
}

func (f *fakeScreens) CRTCs() ([]CRTC, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	return slices.Clone(f.crtcs), f.err
}

func (f *fakeScreens) Gamma(crtc uint32) (Ramp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ramps[crtc], f.err
}

func (f *fakeScreens) SetGamma(r Ramp) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if len(f.ramps[r.CRTC].Red) != len(r.Red) {
		return fmt.Errorf("CRTC %d takes a ramp of %d", r.CRTC, len(f.ramps[r.CRTC].Red))
	}
	f.ramps[r.CRTC] = r
	return nil
}

func (f *fakeScreens) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// set changes a CRTC's ramp as night light would.
func (f *fakeScreens) set(r Ramp) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ramps[r.CRTC] = r
}

func (f *fakeScreens) red(crtc uint32) []uint16 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ramps[crtc].Red
}

func (f *fakeScreens) checked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks
}

// holds collects what a curtain hands its keeper.
type holds struct {
	mu   sync.Mutex
	list [][]Ramp
	err  error
}

func (h *holds) hold(r []Ramp) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.list = append(h.list, r)
	return h.err
}

func (h *holds) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.list)
}

func (h *holds) last(t *testing.T) []Ramp {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.list) == 0 {
		t.Fatal("the curtain handed its keeper nothing")
	}
	return h.list[len(h.list)-1]
}

func (h *holds) refuse(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.err = err
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fast shortens the curtain's checks for one test.
func fast(t *testing.T) {
	old := check
	check = 10 * time.Millisecond
	t.Cleanup(func() { check = old })
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
	}
}

func dark(f *fakeScreens, crtc uint32) func() bool {
	return func() bool { return slices.Max(f.red(crtc)) == 0 }
}

func TestCurtainDarkensAndGivesTheScreensBack(t *testing.T) {
	fast(t)
	f := newScreens(2)
	lit := rising(0, 256, 65535)
	var h holds
	c, err := Open(f, h.hold, discard())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Draw(); err != nil {
		t.Fatal(err)
	}
	if !dark(f, 0)() || !dark(f, 1)() {
		t.Fatal("Draw left a screen lit")
	}
	if u := h.last(t); len(u) != 2 || u[0].CRTC != 0 || u[1].CRTC != 1 || !slices.Equal(u[0].Red, lit.Red) {
		t.Fatalf("the keeper holds %+v, want the lit ramps of CRTCs 0 and 1", u)
	}
	if err := c.Draw(); err != nil {
		t.Fatal(err)
	}

	// Night light sets a warmer ramp while the curtain is drawn: the screen
	// goes dark again, and the new ramp is the one lifting puts back.
	night := rising(1, 256, 40000)
	f.set(night)
	waitFor(t, "night light's ramp to go dark", dark(f, 1))
	if got := h.last(t)[1].Red; !slices.Equal(got, night.Red) {
		t.Fatalf("the keeper holds a ramp ending %v, want night light's", got[250:])
	}

	if err := c.Lift(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.red(0), lit.Red) || !slices.Equal(f.red(1), night.Red) {
		t.Fatal("Lift did not put back the newest ramps")
	}
	if err := c.Lift(); err != nil {
		t.Fatal(err)
	}
	if err := c.Draw(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.red(0), lit.Red) || !f.closed {
		t.Fatal("Close did not light the screens and close them")
	}
}

// While nothing else touches the ramps, the curtain's checks change nothing
// and tell the keeper nothing new.
func TestCurtainLeavesADarkScreenAlone(t *testing.T) {
	fast(t)
	f := newScreens(1)
	var h holds
	c, err := Open(f, h.hold, discard())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Draw(); err != nil {
		t.Fatal(err)
	}
	from := f.checked()
	waitFor(t, "two checks", func() bool { return f.checked() >= from+2 })
	if !dark(f, 0)() {
		t.Fatal("the checks lit the screen")
	}
	if n := h.count(); n != 1 {
		t.Errorf("the keeper was told %d times, want once", n)
	}
}

func TestCurtainStaysOpenWhenTheKeeperRefuses(t *testing.T) {
	f := newScreens(1)
	h := holds{err: errors.New("no keeper")}
	c, err := Open(f, h.hold, discard())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Draw(); err == nil {
		t.Fatal("Draw succeeded with nothing to undo it")
	}
	if dark(f, 0)() {
		t.Fatal("a refused Draw darkened the screen")
	}
}

// A screen without a ramp a program can set would stay lit behind the
// curtain, so there is no curtain.
func TestCurtainRefusesAScreenItCannotCover(t *testing.T) {
	f := newScreens(2)
	f.ramps[1] = Ramp{CRTC: 1}
	var h holds
	_, err := Open(f, h.hold, discard())
	var u Uncovered
	if !errors.As(err, &u) || !slices.Equal(u, Uncovered{"DP-2"}) {
		t.Fatalf("Open over a screen without a ramp: %v", err)
	}
	if h.count() != 0 || dark(f, 0)() {
		t.Fatal("Open changed the screens")
	}
	f.crtcs[1].Output = ""
	if _, err := Open(f, h.hold, discard()); err == nil || err.Error() != "the screen of CRTC 1 has no gamma ramp a program can set, so the curtain cannot black it out" {
		t.Fatalf("Open over an unnamed screen without a ramp: %v", err)
	}
	if _, err := Open(&fakeScreens{}, h.hold, discard()); !errors.Is(err, ErrNoScreen) {
		t.Fatalf("Open over no screens: %v", err)
	}
	f.err = errors.New("the server went away")
	if _, err := Open(f, h.hold, discard()); !errors.Is(err, f.err) {
		t.Fatalf("Open over screens it cannot read: %v", err)
	}
}

// A screen that appears without a ramp while the curtain is drawn is
// reported, and the others stay dark; so is a keeper that refuses, or
// screens that stop answering. Each clears once it passes.
func TestCurtainReportsWhatTheRoomCanSee(t *testing.T) {
	fast(t)
	f := newScreens(1)
	var h holds
	c, err := Open(f, h.hold, discard())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Draw(); err != nil {
		t.Fatal(err)
	}
	leak := func(what string, want func(error) bool) {
		t.Helper()
		select {
		case <-c.Changed():
		case <-time.After(5 * time.Second):
			t.Fatalf("the curtain said nothing of %s", what)
		}
		if err := c.Leak(); !want(err) {
			t.Fatalf("with %s the curtain reports %v", what, err)
		}
	}
	is := func(target error) func(error) bool { return func(err error) bool { return errors.Is(err, target) } }
	none := func(err error) bool { return err == nil }

	f.mu.Lock()
	f.crtcs = append(f.crtcs, CRTC{ID: 1, Output: "DVI-I-1"})
	f.ramps[1] = Ramp{CRTC: 1}
	f.mu.Unlock()
	leak("a monitor plugged in without a ramp", func(err error) bool {
		var u Uncovered
		return errors.As(err, &u) && slices.Equal(u, Uncovered{"DVI-I-1"})
	})
	f.set(rising(0, 256, 40000))
	waitFor(t, "night light's ramp to go dark beside it", dark(f, 0))
	f.mu.Lock()
	f.crtcs = f.crtcs[:1]
	f.mu.Unlock()
	leak("the monitor unplugged", none)

	refused := errors.New("the keeper is gone")
	h.refuse(refused)
	f.set(rising(0, 256, 30000))
	leak("a keeper that refuses night light's ramp", is(refused))
	if dark(f, 0)() {
		t.Fatal("the curtain darkened a ramp nothing would undo")
	}
	h.refuse(nil)
	leak("the keeper back", none)

	gone := errors.New("the X server went away")
	f.mu.Lock()
	f.err = gone
	f.mu.Unlock()
	leak("screens that stop answering", is(gone))
	f.mu.Lock()
	f.err = nil
	f.mu.Unlock()
	leak("screens that answer again", none)

	if err := c.Lift(); err != nil {
		t.Fatal(err)
	}
	if c.Leak() != nil {
		t.Errorf("a lifted curtain reports %v", c.Leak())
	}
}

// Ramps for a CRTC that went away, or that changed its ramp's size, are
// passed over rather than forced onto whatever is there now.
func TestRestoreSkipsWhatNoLongerFits(t *testing.T) {
	f := newScreens(1)
	dim := rising(0, 128, 30000)
	gone := rising(7, 256, 30000)
	if err := Restore(f, []Ramp{dim, gone}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.red(0), rising(0, 256, 65535).Red) {
		t.Fatal("Restore forced a ramp of another size")
	}
	if err := Restore(f, nil); err != nil {
		t.Fatal(err)
	}
	f.err = errors.New("the X server went away")
	if err := Restore(f, []Ramp{dim}); err == nil {
		t.Fatal("Restore over screens it cannot read succeeded")
	}
}
