//go:build linux

package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/xproto"
	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/robin/internal/agent/audio"
	"github.com/yasinmiran/robin/internal/agent/yuv"
	"github.com/yasinmiran/robin/internal/protocol"
)

// silenceForTest is robin-agent's SilenceFunc.
func silenceForTest(ctx context.Context, hold Hold, log *slog.Logger) (Room, error) {
	r, err := audio.Silence(ctx, audio.RoomOptions{Hold: func(u audio.RoomUndo) error { return hold(audio.RoomStep, u) }, Log: log})
	if err != nil {
		return nil, err
	}
	return r, nil
}

type stillEncoder struct{}

func (stillEncoder) Encode(*yuv.I420, time.Duration, bool) ([]byte, error) {
	return []byte{0, 0, 0, 1, 0x65, 0x88, 0x84, 0x21}, nil
}
func (stillEncoder) SetBitrate(int) error { return nil }
func (stillEncoder) Close() error         { return nil }

// serveForTest is an agent on stdio, as robin-agent serve is, whose keepers
// are this test binary too.
func serveForTest() int {
	signal.Ignore(syscall.SIGPIPE)
	dir := os.Getenv(agentEnv)
	os.Unsetenv(agentEnv)
	os.Setenv(keeperEnv, dir)
	exe, _ := os.Executable()
	err := Run(context.Background(), os.Stdin, os.Stdout, Options{
		RunRoot: filepath.Join(dir, "run"), Build: "test",
		Encoders: map[string]NewEncoderFunc{protocol.EncoderSoftware: func(int, int, int, int) (Encoder, error) { return stillEncoder{}, nil }},
		Keeper:   []string{exe, "-test.run=^$"},
		Silence:  silenceForTest,
		Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		return 1
	}
	return 0
}

// roomSound starts the sound server of the desktop a session attaches to:
// a pulseaudio that plays into a null sink called desk, found the way
// libpulse finds the user's own.
func roomSound(t *testing.T) {
	t.Helper()
	for _, c := range []string{"pulseaudio", "pactl", "paplay"} {
		if _, err := exec.LookPath(c); err != nil {
			t.Skip(c + " not on PATH; run internal/agent/x11/testdata/xtest.sh")
		}
	}
	dir := t.TempDir()
	cmd := exec.Command("pulseaudio", "-n", "--daemonize=no", "--exit-idle-time=-1", "--use-pid-file=no", "--log-target=stderr",
		"--load=module-null-sink sink_name=desk rate=48000 channels=2 norewinds=1",
		"--load=module-native-protocol-unix auth-anonymous=1")
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+dir, "HOME="+dir, "DBUS_SESSION_BUS_ADDRESS=unix:path="+filepath.Join(dir, "no-bus"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("PULSE_COOKIE", filepath.Join(dir, "no-cookie"))
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "pulse", "native")); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the room's pulseaudio never opened its socket")
		}
	}
}

func pactl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("pactl", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("pactl %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// sinks lists the server's sinks by name.
func sinks(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(pactl(t, "list", "short", "sinks")), "\n") {
		if f := strings.Fields(line); len(f) > 1 {
			names = append(names, f[1])
		}
	}
	return names
}

var sinkInput = regexp.MustCompile(`(?s)Sink Input #\d+.*?\n\tSink: (\d+).*?application\.name = "([^"]*)"`)

// sinkOf is the name of the sink app's stream plays on, or "".
func sinkOf(t *testing.T, app string) string {
	t.Helper()
	byIndex := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(pactl(t, "list", "short", "sinks")), "\n") {
		if f := strings.Fields(line); len(f) > 1 {
			byIndex[f[0]] = f[1]
		}
	}
	for _, m := range sinkInput.FindAllStringSubmatch(pactl(t, "list", "sink-inputs"), -1) {
		if m[2] == app {
			return byIndex[m[1]]
		}
	}
	return ""
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
	}
}

// playTone plays into the room's sound server as a program on the desktop
// would, until the test ends.
func playTone(t *testing.T, app string) {
	t.Helper()
	cmd := exec.Command("paplay", "--raw", "--format=s16le", "--rate=48000", "--channels=2", "--client-name="+app, "/dev/zero")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	waitFor(t, app+"'s stream", func() bool { return sinkOf(t, app) != "" })
}

// gammaPeer watches the attached display's ramps. It joins RandR before any
// agent connects: randr.Init writes xgb's global tables, which an agent's
// connection already reading events would race.
type gammaPeer struct {
	t     *testing.T
	x     *xgb.Conn
	root  xproto.Window
	crtcs []randr.Crtc
}

func newGammaPeer(t *testing.T, x *xgb.Conn, root xproto.Window) *gammaPeer {
	t.Helper()
	if err := randr.Init(x); err != nil {
		t.Fatal(err)
	}
	res, err := randr.GetScreenResourcesCurrent(x, root).Reply()
	if err != nil {
		t.Fatal(err)
	}
	g := &gammaPeer{t: t, x: x, root: root}
	for _, c := range res.Crtcs {
		if info, err := randr.GetCrtcInfo(x, c, res.ConfigTimestamp).Reply(); err == nil && info.Mode != 0 {
			g.crtcs = append(g.crtcs, c)
		}
	}
	if len(g.crtcs) == 0 {
		t.Fatal("Xvfb drives no CRTC")
	}
	return g
}

// lighten gives every CRTC a linear ramp: Xvfb's start as all zeros, which
// looks the same as a curtain.
func (g *gammaPeer) lighten() {
	g.t.Helper()
	for _, c := range g.crtcs {
		size, err := randr.GetCrtcGammaSize(g.x, c).Reply()
		if err != nil {
			g.t.Fatal(err)
		}
		r := make([]uint16, size.Size)
		for i := range r {
			r[i] = uint16(i * 65535 / (len(r) - 1))
		}
		if err := randr.SetCrtcGammaChecked(g.x, c, size.Size, r, r, r).Check(); err != nil {
			g.t.Fatal(err)
		}
	}
}

// lit reports whether every CRTC's ramp reaches full brightness, dark
// whether every one is zero.
func (g *gammaPeer) top() []uint16 {
	g.t.Helper()
	var tops []uint16
	for _, c := range g.crtcs {
		r, err := randr.GetCrtcGamma(g.x, c).Reply()
		if err != nil {
			g.t.Fatal(err)
		}
		tops = append(tops, slices.Max(r.Red))
	}
	return tops
}

func (g *gammaPeer) lit() bool  { return slices.Max(g.top()) == 65535 && slices.Min(g.top()) == 65535 }
func (g *gammaPeer) dark() bool { return slices.Max(g.top()) == 0 }

// TestSessionPrivateOnAnAttachedDesktop starts an attached session with the
// curtain drawn and the room silent, switches them from the page, and
// checks the session leaves the room as it found it.
func TestSessionPrivateOnAnAttachedDesktop(t *testing.T) {
	live := liveDisplay(t, 1280, 800)
	x, root := xconn(t, live)
	g := newGammaPeer(t, x, root)
	g.lighten()
	roomSound(t)
	playTone(t, "robin-test-room")
	before := sinks(t)
	argv, kdir := testKeeper(t)

	h := startAgent(t, func(o *Options) { o.Keeper, o.Silence = argv, silenceForTest })
	ready := h.start(func(s *protocol.Start) { s.Attach, s.Curtain, s.Silent = live.Name, true, true })
	if p := ready.Private; p == nil || !p.Curtain.On || !p.Silent.On {
		t.Fatalf("ready reports the private session %+v, want both on", ready.Private)
	}
	silent := fmt.Sprintf("robin_silent_%d", os.Getpid())
	if !g.dark() || sinkOf(t, "robin-test-room") != silent {
		t.Fatalf("with both on: gamma tops %v, the room's stream on %q", g.top(), sinkOf(t, "robin-test-room"))
	}

	_, dc, _ := connectBrowser(t, h)
	states := make(chan protocol.Private, 16)
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		var c protocol.Control
		if json.Unmarshal(m.Data, &c) == nil && c.T == protocol.ControlPrivate && c.Private != nil {
			states <- *c.Private
		}
	})
	opened := make(chan struct{})
	dc.OnOpen(func() { close(opened) })
	select {
	case <-opened:
	case <-time.After(15 * time.Second):
		t.Fatal("the control channel never opened")
	}
	next := func(what string) protocol.Private {
		t.Helper()
		select {
		case st := <-states:
			return st
		case <-time.After(10 * time.Second):
			t.Fatalf("the page heard nothing after %s", what)
			return protocol.Private{}
		}
	}
	if st := next("opening"); !st.Curtain.On || !st.Silent.On {
		t.Fatalf("the page was first told %+v", st)
	}
	send := func(c protocol.Control) {
		b, _ := json.Marshal(c)
		if err := dc.SendText(string(b)); err != nil {
			t.Fatal(err)
		}
	}

	send(protocol.Control{T: protocol.ControlCurtain})
	if st := next("curtain off"); st.Curtain.On || !st.Silent.On || !g.lit() {
		t.Fatalf("after curtain off: %+v, gamma tops %v", st, g.top())
	}
	send(protocol.Control{T: protocol.ControlSilent})
	if st := next("silent off"); st.Silent.On {
		t.Fatalf("after silent off: %+v", st)
	}
	if got := sinkOf(t, "robin-test-room"); got != "desk" || slices.Contains(sinks(t), silent) {
		t.Fatalf("after silent off the room's stream plays on %q, sinks %v", got, sinks(t))
	}
	send(protocol.Control{T: protocol.ControlCurtain, On: true})
	if st := next("curtain on"); !st.Curtain.On || !g.dark() {
		t.Fatalf("after curtain on: %+v, gamma tops %v", st, g.top())
	}
	send(protocol.Control{T: protocol.ControlSilent, On: true})
	if st := next("silent on"); !st.Silent.On || sinkOf(t, "robin-test-room") != silent {
		t.Fatalf("after silent on: %+v, the room's stream on %q", st, sinkOf(t, "robin-test-room"))
	}

	h.cli.Send(protocol.Message{Type: protocol.TypeBye})
	select {
	case <-h.exited:
	case <-time.After(15 * time.Second):
		t.Fatal("the agent did not end")
	}
	if !g.lit() || sinkOf(t, "robin-test-room") != "desk" || !slices.Equal(sinks(t), before) {
		t.Fatalf("after the session: gamma tops %v, the room's stream on %q, sinks %v (were %v)",
			g.top(), sinkOf(t, "robin-test-room"), sinks(t), before)
	}
	if u := undone(t, kdir); u != "" {
		t.Errorf("the keeper had to undo %q after a clean end", u)
	}
}

// A desktop reached by host-based access, with no cookie, gets its curtain
// over the same way in.
func TestCurtainWithHostBasedAccess(t *testing.T) {
	live := liveDisplay(t, 1280, 800)
	x, root := xconn(t, live)
	g := newGammaPeer(t, x, root)
	g.lighten()
	if _, err := exec.LookPath("xhost"); err != nil {
		t.Skip("no xhost")
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	grant := exec.Command("xhost", "+si:localuser:"+me.Username)
	grant.Env = append(os.Environ(), "DISPLAY="+live.Name)
	if out, err := grant.CombinedOutput(); err != nil {
		t.Fatalf("xhost: %v: %s", err, out)
	}
	t.Setenv("XAUTHORITY", "")
	t.Setenv("HOME", t.TempDir())
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	d, err := openDesktop(context.Background(), protocol.Start{Attach: live.Name}, t.TempDir(), "", nil, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	argv, _ := testKeeper(t)
	p, err := openPrivate(d, protocol.Start{Curtain: true}, Options{Keeper: argv}, nil, func() {}, log)
	if err != nil {
		t.Fatal(err)
	}
	if !g.dark() {
		t.Fatalf("gamma tops %v with the curtain drawn", g.top())
	}
	p.close()
	if !g.lit() {
		t.Fatalf("gamma tops %v after the curtain", g.top())
	}
}

// TestSessionPrivateWithoutAKeeper: a session asked for a curtain it cannot
// undo ends before Ready, saying why.
func TestSessionPrivateWithoutAKeeper(t *testing.T) {
	live := liveDisplay(t, 1280, 800)
	h := startAgent(t)
	if err := h.cli.Send(protocol.Message{Type: protocol.TypeStart, Start: &protocol.Start{
		Version: protocol.Version, MaxWidth: 1280, MaxHeight: 720, FPS: 30, Attach: live.Name, Curtain: true,
	}}); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case m, ok := <-h.msgs:
			if !ok {
				t.Fatal("the agent ended without saying why")
			}
			if m.Type == protocol.TypeReady {
				t.Fatal("a session without a keeper drew a curtain")
			}
			if m.Type == protocol.TypeError {
				if !strings.Contains(m.Error, "--curtain: ") {
					t.Fatalf("the agent said %q", m.Error)
				}
				return
			}
		case <-time.After(20 * time.Second):
			t.Fatal("no answer to a start with --curtain")
		}
	}
}

// The point of the keeper: an agent killed with SIGKILL, which runs none of
// its own cleanup, still gives the room its screens and its speakers back.
func TestSessionKilledGivesTheRoomBack(t *testing.T) {
	live := liveDisplay(t, 1280, 800)
	x, root := xconn(t, live)
	g := newGammaPeer(t, x, root)
	g.lighten()
	roomSound(t)
	playTone(t, "robin-test-kill")

	exe, _ := os.Executable()
	dir := t.TempDir()
	agent := exec.Command(exe, "-test.run=^$")
	agent.Env = append(os.Environ(), agentEnv+"="+dir)
	agent.Stderr = os.Stderr
	stdin, err := agent.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := agent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	defer agent.Process.Kill()
	conn := protocol.NewConn(bufio.NewReader(stdout), stdin, protocol.DefaultPingInterval, 0)
	if err := conn.Send(protocol.Message{Type: protocol.TypeStart, Start: &protocol.Start{
		Version: protocol.Version, MaxWidth: 1280, MaxHeight: 720, FPS: 30, Attach: live.Name, Curtain: true, Silent: true,
	}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for {
		m, err := conn.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if m.Type == protocol.TypeError {
			t.Fatalf("the agent: %s", m.Error)
		}
		if m.Type == protocol.TypeReady {
			break
		}
	}
	silent := fmt.Sprintf("robin_silent_%d", agent.Process.Pid)
	if !g.dark() || sinkOf(t, "robin-test-kill") != silent {
		t.Fatalf("before the kill: gamma tops %v, the stream on %q", g.top(), sinkOf(t, "robin-test-kill"))
	}

	agent.Process.Kill()
	agent.Wait()
	waitFor(t, "the screens to light up", g.lit)
	waitFor(t, "the stream to go back to desk", func() bool { return sinkOf(t, "robin-test-kill") == "desk" })
	waitFor(t, "the silent sink to go", func() bool { return !slices.Contains(sinks(t), silent) })
	waitFor(t, "the keeper to exit", func() bool { return len(keepersOf(dir)) == 0 })
}

// keepersOf lists the live processes that keep for the agent of dir.
func keepersOf(dir string) []string {
	var pids []string
	procs, _ := filepath.Glob("/proc/[0-9]*/environ")
	for _, p := range procs {
		env, err := os.ReadFile(p)
		if err != nil || !slices.Contains(strings.Split(string(env), "\x00"), keeperEnv+"="+dir) {
			continue
		}
		stat, _ := os.ReadFile(filepath.Join(filepath.Dir(p), "stat"))
		if _, rest, _ := strings.Cut(string(stat), ") "); !strings.HasPrefix(rest, "Z") {
			pids = append(pids, filepath.Base(filepath.Dir(p)))
		}
	}
	return pids
}
