//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agent/audio"
	"github.com/yasinmiran/robin/internal/agent/keeper"
	"github.com/yasinmiran/robin/internal/agent/x11"
	"github.com/yasinmiran/robin/internal/protocol"
)

// keeperEnv makes the test binary a keeper, noting each undo of the fakes
// below in the directory it names; the real undos it runs as robin-agent
// keeper does. agentEnv makes it an agent; see TestSessionKilledGivesTheRoomBack.
const (
	keeperEnv = "ROBIN_AGENT_TEST_KEEPER"
	agentEnv  = "ROBIN_AGENT_TEST_AGENT"
)

func TestMain(m *testing.M) {
	if os.Getenv(agentEnv) != "" {
		os.Exit(serveForTest())
	}
	if dir := os.Getenv(keeperEnv); dir != "" {
		note := func(kind string) func(context.Context, json.RawMessage) error {
			return func(_ context.Context, data json.RawMessage) error {
				f, err := os.OpenFile(filepath.Join(dir, "undone"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
				if err != nil {
					return err
				}
				defer f.Close()
				_, err = f.WriteString(kind + " " + string(data) + "\n")
				return err
			}
		}
		keeper.Main(keeper.Undo{
			"fake-curtain": note("fake-curtain"),
			"fake-room":    note("fake-room"),
			x11.GammaStep:  x11.UndoGamma,
			audio.RoomStep: audio.UndoRoom,
		}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		return
	}
	os.Exit(m.Run())
}

// testKeeper is a keeper made of this test binary, whose undos of the
// fakes land in dir/undone.
func testKeeper(t *testing.T) (argv []string, dir string) {
	t.Helper()
	dir = t.TempDir()
	t.Setenv(keeperEnv, dir)
	exe, _ := os.Executable()
	return []string{exe, "-test.run=^$"}, dir
}

func undone(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "undone"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

type fakeCurtain struct {
	hold Hold

	mu               sync.Mutex
	drawn, closed    bool
	drawErr, liftErr error
}

func (c *fakeCurtain) Draw() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.hold("fake-curtain", "ramps"); err != nil {
		return err
	}
	if c.drawErr != nil {
		return c.drawErr
	}
	c.drawn = true
	return nil
}

func (c *fakeCurtain) Lift() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.liftErr != nil {
		return c.liftErr
	}
	c.drawn = false
	return nil
}

func (c *fakeCurtain) Close() error {
	err := c.Lift()
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return err
}

func (c *fakeCurtain) isDrawn() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.drawn
}

type fakeRoom struct {
	gone   chan struct{}
	err    error
	mu     sync.Mutex
	closed bool
}

func (r *fakeRoom) Monitor() string       { return "fake.monitor" }
func (r *fakeRoom) Gone() <-chan struct{} { return r.gone }
func (r *fakeRoom) Err() error            { return r.err }
func (r *fakeRoom) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

// rooms is a SilenceFunc whose rooms the test can reach.
type rooms struct {
	mu   sync.Mutex
	list []*fakeRoom
	err  error
}

func (rs *rooms) silence(_ context.Context, hold Hold, _ *slog.Logger) (Room, error) {
	if err := hold("fake-room", "moves"); err != nil {
		return nil, err
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.err != nil {
		return nil, rs.err
	}
	r := &fakeRoom{gone: make(chan struct{})}
	rs.list = append(rs.list, r)
	return r, nil
}

func (rs *rooms) last(t *testing.T) *fakeRoom {
	t.Helper()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.list) == 0 {
		t.Fatal("no room was made silent")
	}
	return rs.list[len(rs.list)-1]
}

// capture is an AudioSource that follows where it is told.
type capture struct {
	mu      sync.Mutex
	sources []string
}

func (c *capture) Packets() <-chan []byte { return nil }
func (c *capture) Close() error           { return nil }
func (c *capture) Follow(source string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sources = append(c.sources, source)
	return nil
}

func (c *capture) followed() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.sources, ",")
}

// privateRig is a private session over fakes, with a real keeper.
type privateRig struct {
	t       *testing.T
	p       *private
	curtain *fakeCurtain
	rooms   *rooms
	capture *capture
	sent    chan protocol.Private
	keeper  string // the keeper's directory
}

func newPrivate(t *testing.T, start protocol.Start, configure ...func(*openedDesktop, *Options)) (*privateRig, error) {
	t.Helper()
	argv, dir := testKeeper(t)
	r := &privateRig{t: t, rooms: &rooms{}, capture: &capture{}, sent: make(chan protocol.Private, 16), keeper: dir}
	d := &openedDesktop{mode: protocol.ModeAttach, openCurtain: func(h Hold) (curtain, error) {
		r.curtain = &fakeCurtain{hold: h}
		return r.curtain, nil
	}}
	o := Options{Keeper: argv, Silence: r.rooms.silence}
	for _, c := range configure {
		c(d, &o)
	}
	var err error
	r.p, err = openPrivate(d, start, o, r.capture, func() { r.sent <- *r.p.snapshot() }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil && r.p != nil {
		t.Cleanup(r.p.close)
	}
	return r, err
}

// set flips a switch from the page and returns the state the page is sent.
func (r *privateRig) set(c protocol.ControlType, on bool) protocol.Private {
	r.t.Helper()
	r.p.set(c, on)
	select {
	case st := <-r.sent:
		return st
	case <-time.After(5 * time.Second):
		r.t.Fatal("the page heard nothing")
		return protocol.Private{}
	}
}

func TestPrivateIsForAttachedDesktops(t *testing.T) {
	p, err := openPrivate(&openedDesktop{mode: protocol.ModeSpawn}, protocol.Start{}, Options{}, nil, nil, slog.Default())
	if p != nil || err != nil {
		t.Fatalf("a spawned desktop got a private session %v, %v", p, err)
	}
	if p.snapshot() != nil {
		t.Error("a spawned desktop reports a private session")
	}
	p.set(protocol.ControlCurtain, true)
	p.close()
}

func TestPrivateSaysWhatCannotBe(t *testing.T) {
	r, err := newPrivate(t, protocol.Start{}, func(_ *openedDesktop, o *Options) { o.Keeper = nil })
	if err != nil {
		t.Fatal(err)
	}
	if st := r.p.snapshot(); !strings.Contains(st.Curtain.Unsupported, "no keeper") || !strings.Contains(st.Silent.Unsupported, "no keeper") {
		t.Errorf("without a keeper: %+v", st)
	}
	if _, err := newPrivate(t, protocol.Start{Curtain: true}, func(_ *openedDesktop, o *Options) { o.Keeper = nil }); err == nil || !strings.HasPrefix(err.Error(), "--curtain: ") {
		t.Errorf("--curtain without a keeper: %v", err)
	}

	r, err = newPrivate(t, protocol.Start{}, func(d *openedDesktop, o *Options) {
		d.openCurtain = func(Hold) (curtain, error) { return nil, x11.ErrNoGamma }
		o.Silence = nil
	})
	if err != nil {
		t.Fatal(err)
	}
	st := r.p.snapshot()
	if st.Curtain.Unsupported != x11.ErrNoGamma.Error() || !strings.Contains(st.Silent.Unsupported, "without sound") {
		t.Errorf("with no gamma and no sound: %+v", st)
	}
	r.set(protocol.ControlCurtain, true)
	if st := r.set(protocol.ControlSilent, true); st.Curtain.On || st.Silent.On {
		t.Errorf("switched on what cannot be: %+v", st)
	}

	r, err = newPrivate(t, protocol.Start{}, func(d *openedDesktop, _ *Options) { d.openCurtain = nil })
	if err != nil {
		t.Fatal(err)
	}
	if st := r.p.snapshot(); st.Curtain.Unsupported == "" {
		t.Errorf("a desktop without a curtain: %+v", st)
	}
	if _, err := newPrivate(t, protocol.Start{Silent: true}, func(_ *openedDesktop, o *Options) { o.Silence = nil }); err == nil || !strings.HasPrefix(err.Error(), "--silent: ") {
		t.Errorf("--silent without sound: %v", err)
	}
}

func TestPrivateStartsWithWhatStartAsks(t *testing.T) {
	r, err := newPrivate(t, protocol.Start{Curtain: true, Silent: true})
	if err != nil {
		t.Fatal(err)
	}
	if st := r.p.snapshot(); !st.Curtain.On || !st.Silent.On {
		t.Fatalf("state %+v, want both on", st)
	}
	if !r.curtain.isDrawn() || r.capture.followed() != "fake.monitor" {
		t.Fatalf("curtain drawn %v, capture followed %q", r.curtain.isDrawn(), r.capture.followed())
	}
	r.p.close()
	room := r.rooms.last(t)
	if r.curtain.isDrawn() || !r.curtain.closed || !room.closed || r.capture.followed() != "fake.monitor," {
		t.Fatalf("after close: curtain drawn %v closed %v, room closed %v, capture followed %q",
			r.curtain.isDrawn(), r.curtain.closed, room.closed, r.capture.followed())
	}
	if u := undone(t, r.keeper); u != "" {
		t.Errorf("the keeper undid %q after a clean close", u)
	}
	r.p.close()
}

func TestPrivateStartFailsWhenItCannot(t *testing.T) {
	_, err := newPrivate(t, protocol.Start{Curtain: true}, func(d *openedDesktop, _ *Options) {
		d.openCurtain = func(h Hold) (curtain, error) { return &fakeCurtain{hold: h, drawErr: errors.New("BadMatch")}, nil }
	})
	if err == nil || err.Error() != "--curtain: BadMatch" {
		t.Fatalf("a curtain that cannot be drawn: %v", err)
	}
}

func TestPrivateFromThePage(t *testing.T) {
	r, err := newPrivate(t, protocol.Start{})
	if err != nil {
		t.Fatal(err)
	}
	if st := r.set(protocol.ControlCurtain, true); !st.Curtain.On || !r.curtain.isDrawn() {
		t.Fatalf("curtain on: %+v", st)
	}
	if st := r.set(protocol.ControlSilent, true); !st.Silent.On || r.capture.followed() != "fake.monitor" {
		t.Fatalf("silent on: %+v, followed %q", st, r.capture.followed())
	}
	if st := r.set(protocol.ControlCurtain, false); st.Curtain.On || r.curtain.isDrawn() {
		t.Fatalf("curtain off: %+v", st)
	}
	if st := r.set(protocol.ControlSilent, false); st.Silent.On || !r.rooms.last(t).closed {
		t.Fatalf("silent off: %+v", st)
	}

	r.curtain.drawErr = errors.New("BadMatch")
	if st := r.set(protocol.ControlCurtain, true); st.Curtain.On || st.Curtain.Error != "BadMatch" {
		t.Fatalf("a curtain that fails: %+v", st)
	}
	r.curtain.drawErr = nil
	if st := r.set(protocol.ControlCurtain, true); !st.Curtain.On || st.Curtain.Error != "" {
		t.Fatalf("the curtain again: %+v", st)
	}
	r.rooms.err = errors.New("module-null-sink refused")
	if st := r.set(protocol.ControlSilent, true); st.Silent.On || st.Silent.Error != "module-null-sink refused" {
		t.Fatalf("a silent room that fails: %+v", st)
	}
}

func TestPrivateRoomGoneUnderneath(t *testing.T) {
	r, err := newPrivate(t, protocol.Start{Silent: true})
	if err != nil {
		t.Fatal(err)
	}
	room := r.rooms.last(t)
	room.err = errors.New("pw-record exited")
	close(room.gone)
	select {
	case st := <-r.sent:
		if st.Silent.On || st.Silent.Error != "pw-record exited" {
			t.Fatalf("after the room went: %+v", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the page heard nothing of the room going")
	}
	if r.capture.followed() != "fake.monitor," || !room.closed {
		t.Fatalf("capture followed %q, room closed %v", r.capture.followed(), room.closed)
	}
}

// A curtain that cannot be lifted is left to the keeper.
func TestPrivateLeavesTheKeeperWhatItCannotUndo(t *testing.T) {
	r, err := newPrivate(t, protocol.Start{Curtain: true})
	if err != nil {
		t.Fatal(err)
	}
	r.curtain.liftErr = errors.New("the X server stopped answering")
	r.p.close()
	if u := undone(t, r.keeper); u != `fake-curtain "ramps"` {
		t.Fatalf("the keeper undid %q, want the curtain", u)
	}
}
