//go:build linux

package wayland

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/yasinmiran/robin/internal/agent/gamma"
	"github.com/yasinmiran/robin/internal/agent/wayland/wltest"
)

// privateBus starts a session bus of the test's own, with no services to
// activate, and points sessionBus at it and nowhere else: these tests set
// gamma ramps, which on the user's own bus would darken the user's screens.
func privateBus(t *testing.T) *dbus.Conn {
	t.Helper()
	bin, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("needs dbus-daemon for a private bus")
	}
	dir := t.TempDir()
	sock := filepath.Join(dir, "bus")
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+sock)
	config := filepath.Join(dir, "bus.conf")
	os.WriteFile(config, []byte(`<busconfig><type>session</type><listen>unix:path=`+sock+`</listen>
<policy context="default"><allow send_destination="*" eavesdrop="true"/><allow eavesdrop="true"/><allow own="*"/></policy></busconfig>`), 0o600)
	cmd := exec.Command(bin, "--config-file="+config, "--nofork", "--nopidfile")
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if fi, err := os.Stat(sock); err == nil && fi.Mode().Type() == os.ModeSocket {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the private bus never came up")
		}
	}
	conn, err := sessionBus()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	var names []string
	if err := conn.BusObject().Call("org.freedesktop.DBus.ListNames", 0).Store(&names); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if !strings.HasPrefix(n, ":") && n != "org.freedesktop.DBus" {
			t.Fatalf("the bus the tests reached serves %s: it is not their private one", n)
		}
	}
	return conn
}

// checkWait bounds a wait for the curtain's once-a-second check.
const checkWait = 3 * time.Second

// fakeMutter serves the part of org.gnome.Mutter.DisplayConfig a curtain
// uses, with mutter's signatures, and refuses a stale serial as mutter does.
// Each CRTC drives an output DP-<n>, where n counts from 1, unless its mode
// is -1.
type fakeMutter struct {
	mu      sync.Mutex
	serial  uint32
	crtcs   []mutterCrtc
	outputs []mutterOutput
	ramps   map[uint32][3][]uint16
}

func newFakeMutter(t *testing.T, bus *dbus.Conn, modes ...int32) *fakeMutter {
	t.Helper()
	f := &fakeMutter{serial: 1, ramps: map[uint32][3][]uint16{}}
	for i, mode := range modes {
		f.crtcs = append(f.crtcs, mutterCrtc{ID: uint32(i), Mode: mode, Width: 1280, Height: 720, Properties: map[string]dbus.Variant{}})
		f.ramps[uint32(i)] = linear(256, 65535)
		on := int32(i)
		if mode < 0 {
			on = -1
		}
		f.outputs = append(f.outputs, mutterOutput{ID: uint32(i), CRTC: on, Name: fmt.Sprintf("DP-%d", i+1), Properties: map[string]dbus.Variant{}})
	}
	if err := bus.Export(f, displayConfigPath, displayConfigIface); err != nil {
		t.Fatal(err)
	}
	if reply, err := bus.RequestName(displayConfigName, dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("own %s: %v %v", displayConfigName, reply, err)
	}
	return f
}

func linear(n, top int) [3][]uint16 {
	r := make([]uint16, n)
	for i := range r {
		r[i] = uint16(i * top / (n - 1))
	}
	return [3][]uint16{r, slices.Clone(r), slices.Clone(r)}
}

var errStale = dbus.NewError("org.freedesktop.DBus.Error.AccessDenied", []any{"The requested configuration is based on stale information"})

func (f *fakeMutter) GetResources() (uint32, []mutterCrtc, []mutterOutput, []mutterMode, int32, int32, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.serial, f.crtcs, f.outputs, []mutterMode{{Width: 1280, Height: 720, Rate: 60}}, 8192, 8192, nil
}

func (f *fakeMutter) GetCrtcGamma(serial, crtc uint32) ([]uint16, []uint16, []uint16, *dbus.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if serial != f.serial {
		return nil, nil, nil, errStale
	}
	r, ok := f.ramps[crtc]
	if !ok {
		return nil, nil, nil, dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", []any{"Invalid crtc id"})
	}
	return r[0], r[1], r[2], nil
}

func (f *fakeMutter) SetCrtcGamma(serial, crtc uint32, red, green, blue []uint16) *dbus.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if serial != f.serial {
		return errStale
	}
	if _, ok := f.ramps[crtc]; !ok {
		return dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", []any{"Invalid crtc id"})
	}
	f.ramps[crtc] = [3][]uint16{red, green, blue}
	return nil
}

// set changes a CRTC's ramp as night light would, and the serial as a
// monitor change would.
func (f *fakeMutter) set(crtc uint32, r [3][]uint16) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ramps[crtc] = r
	f.serial++
}

func (f *fakeMutter) red(crtc uint32) []uint16 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ramps[crtc][0]
}

func (f *fakeMutter) waitRed(t *testing.T, crtc uint32, want []uint16) {
	t.Helper()
	deadline := time.Now().Add(checkWait)
	for !slices.Equal(f.red(crtc), want) {
		if time.Now().After(deadline) {
			t.Fatalf("CRTC %d's red ramp ends %v, want %v", crtc, f.red(crtc)[250:], want[250:])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type mutterHolds struct {
	mu   sync.Mutex
	list []MutterUndo
	err  error
}

func (h *mutterHolds) hold(kind string, undo any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	u, ok := undo.(MutterUndo)
	if kind != MutterStep || !ok {
		return errors.New("not a MutterUndo for " + MutterStep)
	}
	h.list = append(h.list, u)
	return h.err
}

func (h *mutterHolds) last(t *testing.T) MutterUndo {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.list) == 0 {
		t.Fatal("the curtain handed its keeper nothing")
	}
	return h.list[len(h.list)-1]
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestMutterCurtainDarkensAndGivesTheScreensBack(t *testing.T) {
	f := newFakeMutter(t, privateBus(t), 0, -1, 1)
	lin := linear(256, 65535)
	zero := make([]uint16, 256)
	var h mutterHolds
	c, err := OpenCurtain(h.hold, discard())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Draw(); err != nil {
		t.Fatal(err)
	}
	for crtc, want := range map[uint32][]uint16{0: zero, 1: lin[0], 2: zero} {
		if got := f.red(crtc); !slices.Equal(got, want) {
			t.Fatalf("after Draw CRTC %d's red ramp ends %v, want %v", crtc, got[250:], want[250:])
		}
	}
	if u := h.last(t); len(u.Ramps) != 2 || u.Ramps[0].CRTC != 0 || u.Ramps[1].CRTC != 2 || !slices.Equal(u.Ramps[0].Red, lin[0]) {
		t.Fatalf("the keeper holds %d ramps, want the linear ramps of CRTCs 0 and 2", len(u.Ramps))
	}

	// Night light, with a new serial: the screen goes dark again, and the
	// new ramp is the one lifting puts back.
	night := linear(256, 40000)
	f.set(2, night)
	f.waitRed(t, 2, zero)
	if got := h.last(t).Ramps[1].Red; !slices.Equal(got, night[0]) {
		t.Fatalf("the keeper holds a ramp ending %v, want night light's", got[250:])
	}

	if err := c.Lift(); err != nil {
		t.Fatal(err)
	}
	f.waitRed(t, 0, lin[0])
	f.waitRed(t, 2, night[0])
	if err := c.Draw(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	f.waitRed(t, 0, lin[0])
}

func TestMutterCurtainStaysOpenWhenTheKeeperRefuses(t *testing.T) {
	f := newFakeMutter(t, privateBus(t), 0)
	h := mutterHolds{err: errors.New("no keeper")}
	c, err := OpenCurtain(h.hold, discard())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Draw(); err == nil {
		t.Fatal("Draw succeeded with nothing to undo it")
	}
	if got := f.red(0); !slices.Equal(got, linear(256, 65535)[0]) {
		t.Fatalf("a refused Draw left a ramp ending %v", got[250:])
	}
}

// The keeper's undo, on its own connection and with what a curtain handed
// it, puts back the ramps a curtain left dark, as when the agent is killed.
func TestUndoMutterAfterTheCurtainIsGone(t *testing.T) {
	f := newFakeMutter(t, privateBus(t), 0)
	var h mutterHolds
	c, err := OpenCurtain(h.hold, discard())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Draw(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	f.set(0, [3][]uint16{make([]uint16, 256), make([]uint16, 256), make([]uint16, 256)}) // dark, as the agent's death leaves it, with a new serial

	data, _ := json.Marshal(h.last(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := UndoMutter(ctx, data); err != nil {
		t.Fatal(err)
	}
	f.waitRed(t, 0, linear(256, 65535)[0])
	if err := UndoMutter(ctx, []byte("[")); err == nil {
		t.Error("UndoMutter took broken data")
	}
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := UndoMutter(expired, data); err == nil {
		t.Error("UndoMutter ran past its deadline")
	}
}

// A screen whose ramp mutter keeps to itself would stay lit behind the
// curtain, so there is no curtain, and the other screens are left alone.
func TestMutterCurtainRefusesAScreenWithoutGamma(t *testing.T) {
	f := newFakeMutter(t, privateBus(t), 0, 0)
	f.set(1, [3][]uint16{{}, {}, {}})
	var h mutterHolds
	_, err := OpenCurtain(h.hold, discard())
	var u gamma.Uncovered
	if !errors.As(err, &u) || len(u) != 1 || u[0] != "DP-2" {
		t.Fatalf("OpenCurtain over a screen without gamma: %v", err)
	}
	if !strings.Contains(err.Error(), "DP-2") {
		t.Errorf("the error %q does not name the screen", err)
	}
	if got := f.red(0); !slices.Equal(got, linear(256, 65535)[0]) || len(h.list) != 0 {
		t.Fatal("OpenCurtain changed the screens")
	}
}

func TestMutterCurtainSaysWhyNot(t *testing.T) {
	bus := privateBus(t)
	if _, err := OpenCurtain(nil, discard()); !errors.Is(err, ErrNoDisplayConfig) {
		t.Fatalf("with nobody serving DisplayConfig, OpenCurtain = %v, want ErrNoDisplayConfig", err)
	}
	newFakeMutter(t, bus, -1)
	if _, err := OpenCurtain(nil, discard()); !errors.Is(err, ErrNoGamma) {
		t.Fatalf("with no screen lit, OpenCurtain = %v, want ErrNoGamma", err)
	}
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	if _, err := OpenCurtain(nil, discard()); err == nil {
		t.Fatal("OpenCurtain without a session bus succeeded")
	}
	if err := UndoMutter(context.Background(), []byte(`{"ramps":[{"crtc":0}]}`)); err == nil {
		t.Fatal("UndoMutter without a session bus succeeded")
	}
}

// TestCurtainOnTheTestDesktop checks the real compositor: GNOME's mutter has
// the gamma calls with the signatures the fake serves but, headless, no
// screen to darken; KDE Plasma has no such interface.
func TestCurtainOnTheTestDesktop(t *testing.T) {
	needSession(t)
	_, err := OpenCurtain(nil, discard())
	if wltest.KDE() {
		if !errors.Is(err, ErrNoDisplayConfig) {
			t.Fatalf("on KDE, OpenCurtain = %v, want ErrNoDisplayConfig", err)
		}
		return
	}
	if !errors.Is(err, ErrNoGamma) {
		t.Fatalf("on headless mutter, OpenCurtain = %v, want ErrNoGamma", err)
	}
	conn, err := sessionBus()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var xml string
	if err := conn.Object(displayConfigName, displayConfigPath).Call("org.freedesktop.DBus.Introspectable.Introspect", 0).Store(&xml); err != nil {
		t.Fatal(err)
	}
	flat := strings.Join(strings.Fields(xml), " ")
	for _, want := range []string{
		`<method name="GetResources"> <arg type="u" name="serial" direction="out"/> <arg type="a(uxiiiiiuaua{sv})" name="crtcs" direction="out"/> <arg type="a(uxiausauaua{sv})" name="outputs" direction="out"/> <arg type="a(uxuudu)" name="modes" direction="out"/> <arg type="i" name="max_screen_width" direction="out"/> <arg type="i" name="max_screen_height" direction="out"/>`,
		`<method name="GetCrtcGamma"> <arg type="u" name="serial" direction="in"/> <arg type="u" name="crtc" direction="in"/> <arg type="aq" name="red" direction="out"/> <arg type="aq" name="green" direction="out"/> <arg type="aq" name="blue" direction="out"/>`,
		`<method name="SetCrtcGamma"> <arg type="u" name="serial" direction="in"/> <arg type="u" name="crtc" direction="in"/> <arg type="aq" name="red" direction="in"/> <arg type="aq" name="green" direction="in"/> <arg type="aq" name="blue" direction="in"/>`,
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("mutter's DisplayConfig lacks\n%s\nin\n%s", want, flat)
		}
	}
}
