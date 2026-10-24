//go:build linux

package x11

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jezek/xgb/randr"

	"github.com/yasinmiran/robin/internal/agent/gamma"
)

// Xvfb keeps a 256-entry ramp per CRTC and hands back what was set, though
// it shows nothing. Its ramps start as all zeros, the curtain's own state,
// so each test sets a ramp of its own first. The curtain's state machine is
// tested in package gamma; these test it over RandR.

// checkWait bounds a wait for the curtain's once-a-second check.
const checkWait = 3 * time.Second

// gammaPeer reads and writes the display's ramps from the tests' own
// connection, as night light or xrandr --gamma would.
type gammaPeer struct {
	t  *testing.T
	tx *testX
}

func (p gammaPeer) crtcs() []randr.Crtc {
	p.t.Helper()
	list, err := (&screens{x: p.tx.peer, root: p.tx.root}).CRTCs()
	if err != nil {
		p.t.Fatal(err)
	}
	if len(list) == 0 {
		p.t.Fatal("Xvfb lists no CRTC that drives a screen")
	}
	var out []randr.Crtc
	for _, c := range list {
		out = append(out, randr.Crtc(c.ID))
	}
	return out
}

// set gives every CRTC a ramp that rises to top, and returns it.
func (p gammaPeer) set(top int) []uint16 {
	p.t.Helper()
	var ramp []uint16
	for _, crtc := range p.crtcs() {
		size, err := randr.GetCrtcGammaSize(p.tx.peer, crtc).Reply()
		if err != nil {
			p.t.Fatal(err)
		}
		ramp = make([]uint16, size.Size)
		for i := range ramp {
			ramp[i] = uint16(i * top / (len(ramp) - 1))
		}
		if err := randr.SetCrtcGammaChecked(p.tx.peer, crtc, size.Size, ramp, ramp, ramp).Check(); err != nil {
			p.t.Fatal(err)
		}
	}
	return ramp
}

// red is the red ramp of every CRTC.
func (p gammaPeer) red() [][]uint16 {
	p.t.Helper()
	var out [][]uint16
	for _, crtc := range p.crtcs() {
		g, err := randr.GetCrtcGamma(p.tx.peer, crtc).Reply()
		if err != nil {
			p.t.Fatal(err)
		}
		out = append(out, g.Red)
	}
	return out
}

func (p gammaPeer) want(what string, ramp []uint16) {
	p.t.Helper()
	for i, got := range p.red() {
		if !slices.Equal(got, ramp) {
			p.t.Fatalf("CRTC %d after %s: red ramp ends %v, want %v", i, what, tail(got), tail(ramp))
		}
	}
}

func (p gammaPeer) waitFor(what string, ramp []uint16) {
	p.t.Helper()
	deadline := time.Now().Add(checkWait)
	for {
		ok := true
		for _, got := range p.red() {
			ok = ok && slices.Equal(got, ramp)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			p.want(what, ramp)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func tail(r []uint16) []uint16 { return r[max(0, len(r)-3):] }

// holds collects what a curtain hands its keeper.
type holds struct {
	mu   sync.Mutex
	list []GammaUndo
	err  error
}

func (h *holds) hold(kind string, undo any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	u, ok := undo.(GammaUndo)
	if kind != GammaStep || !ok {
		return errors.New("not a GammaUndo for " + GammaStep)
	}
	h.list = append(h.list, u)
	return h.err
}

func (h *holds) last(t *testing.T) GammaUndo {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.list) == 0 {
		t.Fatal("the curtain handed its keeper nothing")
	}
	return h.list[len(h.list)-1]
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestCurtainDarkensAndGivesTheScreensBack(t *testing.T) {
	tx := startX(t, "sleep 1000")
	p := gammaPeer{t, tx}
	linear := p.set(65535)
	zero := make([]uint16, len(linear))
	var h holds
	c, err := OpenCurtain(tx.d.Name, tx.d.XAuthority, h.hold, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Draw(); err != nil {
		t.Fatal(err)
	}
	p.want("Draw", zero)
	u := h.last(t)
	if u.Display != tx.d.Name || u.XAuthority != tx.d.XAuthority || len(u.Ramps) != len(p.crtcs()) || !slices.Equal(u.Ramps[0].Red, linear) {
		t.Fatalf("the keeper holds %s %s with %d ramps, want the linear ramp of each of %d CRTCs", u.Display, u.XAuthority, len(u.Ramps), len(p.crtcs()))
	}

	// Night light sets a warmer ramp while the curtain is drawn: the screens
	// go dark again, and the new ramp is the one lifting puts back.
	night := p.set(40000)
	p.waitFor("night light's ramp", zero)
	if got := h.last(t).Ramps[0].Red; !slices.Equal(got, night) {
		t.Fatalf("the keeper holds a ramp ending %v, want night light's, ending %v", tail(got), tail(night))
	}

	if err := c.Lift(); err != nil {
		t.Fatal(err)
	}
	p.want("Lift", night)

	linear = p.set(65535)
	if err := c.Draw(); err != nil {
		t.Fatal(err)
	}
	p.want("Draw again", zero)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	p.want("Close", linear)
}

// An X server that goes away under a drawn curtain leaves it nothing to
// lift: its next check says so, and so does Close.
func TestCurtainWhenTheServerGoes(t *testing.T) {
	tx := startX(t, "sleep 1000")
	gammaPeer{t, tx}.set(65535)
	c, err := OpenCurtain(tx.d.Name, tx.d.XAuthority, (&holds{}).hold, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Draw(); err != nil {
		t.Fatal(err)
	}
	tx.d.Close()
	select {
	case <-c.Changed():
		if c.Leak() == nil {
			t.Error("the curtain's check over a display that is gone found nothing amiss")
		}
	case <-time.After(checkWait):
		t.Fatal("the curtain's check said nothing of a display that is gone")
	}
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case err := <-closed:
		if err == nil {
			t.Error("lifting a curtain over a display that is gone succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung on a display that is gone")
	}
}

func TestCurtainStaysOpenWhenTheKeeperRefuses(t *testing.T) {
	tx := startX(t, "sleep 1000")
	p := gammaPeer{t, tx}
	linear := p.set(65535)
	h := holds{err: errors.New("no keeper")}
	c, err := OpenCurtain(tx.d.Name, tx.d.XAuthority, h.hold, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Draw(); err == nil {
		t.Fatal("Draw succeeded with nothing to undo it")
	}
	p.want("a refused Draw", linear)
}

// The keeper's undo, on its own connection and with what a curtain handed
// it, puts back the ramps a curtain left dark, as when the agent is killed.
func TestUndoGammaAfterTheCurtainIsGone(t *testing.T) {
	tx := startX(t, "sleep 1000")
	p := gammaPeer{t, tx}
	linear := p.set(65535)
	var h holds
	c, err := OpenCurtain(tx.d.Name, tx.d.XAuthority, h.hold, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Draw(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	p.set(0) // as the agent's death leaves them
	data, err := json.Marshal(h.last(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := UndoGamma(ctx, data); err != nil {
		t.Fatal(err)
	}
	p.want("the keeper's undo", linear)

	if err := UndoGamma(ctx, []byte("{")); err == nil {
		t.Error("UndoGamma took broken data")
	}
	if err := UndoGamma(ctx, []byte(`{"display":":999"}`)); err == nil {
		t.Error("UndoGamma reached a display that is not there")
	}
}

// The undo's deadline holds on the connection, so a server that stops
// answering cannot keep the keeper waiting past it.
func TestUndoGammaKeepsToItsDeadline(t *testing.T) {
	tx := startX(t, "sleep 1000")
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	data, _ := json.Marshal(GammaUndo{Display: tx.d.Name, XAuthority: tx.d.XAuthority})
	start := time.Now()
	if err := UndoGamma(ctx, data); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Errorf("UndoGamma past its deadline: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("UndoGamma past its deadline took %v", d)
	}
}

// Ramps for a CRTC that went away, or that changed its ramp's size, are
// passed over rather than forced onto whatever is there now.
func TestUndoGammaSkipsWhatNoLongerFits(t *testing.T) {
	tx := startX(t, "sleep 1000")
	p := gammaPeer{t, tx}
	linear := p.set(65535)
	crtc := uint32(p.crtcs()[0])
	short := make([]uint16, len(linear)/2)
	data, _ := json.Marshal(GammaUndo{Display: tx.d.Name, XAuthority: tx.d.XAuthority, Ramps: []gamma.Ramp{
		{CRTC: crtc, Red: short, Green: short, Blue: short},
		{CRTC: crtc + 1000, Red: make([]uint16, len(linear)), Green: make([]uint16, len(linear)), Blue: make([]uint16, len(linear))},
	}})
	if err := UndoGamma(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	p.want("an undo that fits nothing", linear)
}

// Each screen is named by its output, for a curtain that cannot cover it.
func TestScreensNameTheirOutputs(t *testing.T) {
	tx := startX(t, "sleep 1000")
	list, err := (&screens{x: tx.peer, root: tx.root}).CRTCs()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range list {
		if c.Output == "" {
			t.Errorf("CRTC %d drives an output with no name", c.ID)
		}
	}
}

func TestOpenCurtainNeedsTheDisplay(t *testing.T) {
	tx := startX(t, "sleep 1000")
	if _, err := OpenCurtain(":999", "", nil, quietLog()); err == nil {
		t.Error("OpenCurtain reached a display that is not there")
	}
	if _, err := OpenCurtain("remote:0", "", nil, quietLog()); err == nil {
		t.Error("OpenCurtain took a display on another machine")
	}
	if err := UndoGamma(context.Background(), []byte(`{"display":"remote:0"}`)); err == nil {
		t.Error("UndoGamma took a display on another machine")
	}
	if err := UndoGamma(context.Background(), []byte(`{"display":"`+tx.d.Name+`","xauthority":"/dev/null"}`)); err == nil {
		t.Error("UndoGamma got in with an empty authority file")
	}
	if _, err := OpenCurtain(tx.d.Name, "/dev/null", nil, quietLog()); err == nil {
		t.Error("OpenCurtain got in with an empty authority file")
	}
}
