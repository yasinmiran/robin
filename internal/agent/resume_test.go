//go:build linux

package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/robin/internal/protocol"
	"github.com/yasinmiran/robin/internal/relay"
)

// feed is a Controllers the test hands connections to, as a daemon's
// socket would.
type feed struct {
	conns   chan *protocol.Conn
	changes chan protocol.Session
}

func (f *feed) Next(ctx context.Context) (*protocol.Conn, error) {
	select {
	case c := <-f.conns:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *feed) Changed(s protocol.Session) {
	select {
	case f.changes <- s:
	default:
	}
}

// kept runs Serve the way a daemon does: a run directory of its own, a keep
// window, controllers arriving one after another.
type kept struct {
	t      *testing.T
	feed   *feed
	dir    string
	cancel context.CancelFunc
	exited chan struct{}
	runErr error
}

func startKept(t *testing.T, keep time.Duration) *kept {
	t.Helper()
	if _, err := exec.LookPath("Xvfb"); err != nil {
		t.Skip("Xvfb not installed; run via internal/agent/x11/testdata/xtest.sh")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "ab12")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	k := &kept{
		t: t, dir: dir, exited: make(chan struct{}),
		feed: &feed{conns: make(chan *protocol.Conn), changes: make(chan protocol.Session, 64)},
	}
	enc := &fakeEncoder{sizes: make(chan [2]int, 64)}
	ctx, cancel := context.WithCancel(context.Background())
	k.cancel = cancel
	go func() {
		defer close(k.exited)
		k.runErr = Serve(ctx, k.feed, Options{
			RunRoot: root, RunDir: dir, Keep: keep, Build: "test",
			Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
			Encoders: map[string]NewEncoderFunc{protocol.EncoderSoftware: func(w, h, fps, bps int) (Encoder, error) { return enc, nil }},
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-k.exited:
		case <-time.After(10 * time.Second):
		}
	})
	return k
}

// ctl is one controller: the CLI's end of a connection to the session.
type ctl struct {
	t     *testing.T
	cli   *protocol.Conn
	stdin io.Closer
	msgs  chan protocol.Message
}

// connect hands the session a new controller and returns the CLI's end.
func (k *kept) connect() *ctl {
	k.t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &ctl{t: k.t, stdin: inW, msgs: make(chan protocol.Message, 256),
		cli: protocol.NewConn(outR, inW, protocol.DefaultPingInterval, 0)}
	go func() {
		defer close(c.msgs)
		for {
			m, err := c.cli.Recv(context.Background())
			if err != nil {
				return
			}
			if m.Type != protocol.TypeCandidate {
				c.msgs <- m
			}
		}
	}()
	conn := protocol.NewConn(inR, outW, protocol.DefaultPingInterval, protocol.DefaultIdleTimeout)
	// A daemon's socket closes with its protocol connection; so do these pipes.
	go func() {
		<-conn.Done()
		outW.Close()
	}()
	select {
	case k.feed.conns <- conn:
	case <-time.After(10 * time.Second):
		k.t.Fatal("the session never asked for the next controller")
	}
	return c
}

func (c *ctl) send(m protocol.Message) {
	c.t.Helper()
	if err := c.cli.Send(m); err != nil {
		c.t.Fatal(err)
	}
}

// recv returns the next message of type want; an error message fails the
// test unless it is the one wanted.
func (c *ctl) recv(want protocol.Type) protocol.Message {
	c.t.Helper()
	for {
		select {
		case m, ok := <-c.msgs:
			if !ok {
				c.t.Fatalf("the controller's connection closed while waiting for %s", want)
			}
			if m.Type == want {
				return m
			}
			if m.Type == protocol.TypeError {
				c.t.Fatalf("agent error while waiting for %s: %s", want, m.Error)
			}
		case <-time.After(20 * time.Second):
			c.t.Fatalf("timed out waiting for %s", want)
		}
	}
}

// closed waits for the agent to close this controller's connection.
func (c *ctl) closed() {
	c.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-c.msgs:
			if !ok {
				return
			}
		case <-deadline:
			c.t.Fatal("the agent kept the controller's connection open")
		}
	}
}

func (c *ctl) start() *protocol.Ready {
	c.t.Helper()
	c.send(protocol.Message{Type: protocol.TypeStart, Start: &protocol.Start{
		Version: protocol.Version, Width: 800, Height: 600, MaxWidth: 1280, MaxHeight: 720, FPS: 30}})
	return c.recv(protocol.TypeReady).Ready
}

func (c *ctl) resume(id string) *protocol.Ready {
	c.t.Helper()
	c.send(protocol.Message{Type: protocol.TypeResume, Resume: &protocol.Resume{Version: protocol.Version, ID: id}})
	return c.recv(protocol.TypeReady).Ready
}

// fingerprint offers from a browser's stand-in and returns the DTLS
// fingerprint of the agent's answer.
func (c *ctl) fingerprint() string {
	c.t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		c.t.Fatal(err)
	}
	defer pc.Close()
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		c.t.Fatal(err)
	}
	pc.SetLocalDescription(offer)
	c.send(protocol.Message{Type: protocol.TypeOffer, SDP: offer.SDP})
	fp, err := relay.Fingerprint(c.recv(protocol.TypeAnswer).SDP)
	if err != nil {
		c.t.Fatal(err)
	}
	return fp
}

// state waits for the session to report connected or not.
func (k *kept) state(connected bool) protocol.Session {
	k.t.Helper()
	for {
		select {
		case s := <-k.feed.changes:
			if s.Connected == connected {
				return s
			}
		case <-time.After(10 * time.Second):
			k.t.Fatalf("the session never reported connected=%v", connected)
		}
	}
}

func (k *kept) running() {
	k.t.Helper()
	select {
	case <-k.exited:
		k.t.Fatalf("the session ended (%v)", k.runErr)
	default:
	}
}

// waitGone fails unless Serve returned without error and left nothing of
// display behind.
func (k *kept) waitGone(display string) {
	k.t.Helper()
	select {
	case <-k.exited:
		if k.runErr != nil {
			k.t.Fatalf("Serve: %v", k.runErr)
		}
	case <-time.After(15 * time.Second):
		k.t.Fatal("the session did not end")
	}
	if _, err := os.Lstat(k.dir); !errors.Is(err, os.ErrNotExist) {
		k.t.Fatalf("run dir still there: %v", err)
	}
	if k.xvfbPID() != 0 {
		k.t.Fatalf("Xvfb for %s is still running", display)
	}
}

// xvfbPID finds the session's Xvfb, the one whose cookie file is in the run
// directory, or 0 when it is gone.
func (k *kept) xvfbPID() int {
	return findProcess("Xvfb", "-auth", filepath.Join(k.dir, "Xauthority"))
}

// findProcess returns the pid of a live process whose command line holds
// every word given, or 0.
func findProcess(words ...string) int {
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		argv := strings.Split(string(b), "\x00")
		found := true
		for _, w := range words {
			if !slices.Contains(argv, w) {
				found = false
				break
			}
		}
		if found && syscall.Kill(pid, 0) != syscall.ESRCH {
			return pid
		}
	}
	return 0
}

func TestKeptSessionSurvivesItsControllerAndResumes(t *testing.T) {
	k := startKept(t, 30*time.Second)
	c1 := k.connect()
	ready := c1.start()
	if ready.ID != "ab12" || ready.Keep != 30 || ready.Resumed {
		t.Fatalf("ready %+v", ready)
	}
	st := k.state(true)
	if st.ID != "ab12" || st.PID != os.Getpid() || st.Mode != protocol.ModeSpawn || st.Display != ready.Display || st.Keep != 30 || st.Until != nil {
		t.Fatalf("session state %+v", st)
	}
	fp1 := c1.fingerprint()
	xvfb := k.xvfbPID()
	if xvfb == 0 {
		t.Fatalf("no Xvfb for %s", ready.Display)
	}

	// The controller goes without bye: the desktop stays, the keep window
	// opens.
	c1.stdin.Close()
	st = k.state(false)
	if st.Until == nil || time.Until(*st.Until) > 30*time.Second || time.Until(*st.Until) < 20*time.Second {
		t.Fatalf("keep window %+v", st.Until)
	}
	k.running()
	if k.xvfbPID() != xvfb {
		t.Fatal("the display did not survive its controller")
	}

	// The next controller resumes: same display, same tunnel, same
	// certificate behind the answer, so a pinned fingerprint still holds.
	c2 := k.connect()
	again := c2.resume("ab12")
	if !again.Resumed || again.ID != ready.ID || again.Display != ready.Display || again.TunnelAddr != ready.TunnelAddr {
		t.Fatalf("resumed ready %+v, first %+v", again, ready)
	}
	if st := k.state(true); st.Until != nil {
		t.Fatalf("state after resume %+v", st)
	}
	if fp2 := c2.fingerprint(); fp2 != fp1 {
		t.Fatalf("fingerprint changed across the resume: %s then %s", fp1, fp2)
	}
	if k.xvfbPID() != xvfb {
		t.Fatal("a different Xvfb answers after the resume")
	}

	c2.send(protocol.Message{Type: protocol.TypeBye})
	if m := c2.recv(protocol.TypeEnded); !strings.Contains(m.Error, "CLI ended") {
		t.Fatalf("ended says %q", m.Error)
	}
	k.waitGone(ready.Display)
}

func TestKeptSessionRefusesTheWrongResume(t *testing.T) {
	k := startKept(t, 30*time.Second)
	c1 := k.connect()
	ready := c1.start()
	c1.stdin.Close()
	k.state(false)

	wrong := k.connect()
	wrong.send(protocol.Message{Type: protocol.TypeResume, Resume: &protocol.Resume{Version: protocol.Version, ID: "zz99"}})
	if m := wrong.recv(protocol.TypeError); !strings.Contains(m.Error, `no session "zz99"`) {
		t.Fatalf("got %q", m.Error)
	}
	wrong.closed()
	starter := k.connect()
	starter.send(protocol.Message{Type: protocol.TypeStart, Start: &protocol.Start{Version: protocol.Version}})
	if m := starter.recv(protocol.TypeError); !strings.Contains(m.Error, "expected resume") {
		t.Fatalf("got %q", m.Error)
	}
	starter.closed()
	k.running()

	right := k.connect()
	if r := right.resume("ab12"); !r.Resumed {
		t.Fatalf("ready %+v", r)
	}
	right.send(protocol.Message{Type: protocol.TypeBye})
	k.waitGone(ready.Display)
}

func TestKeptSessionEndsWhenTheKeepWindowRunsOut(t *testing.T) {
	k := startKept(t, 2*time.Second)
	c1 := k.connect()
	ready := c1.start()
	start := time.Now()
	c1.stdin.Close()
	k.state(false)
	k.waitGone(ready.Display)
	if took := time.Since(start); took < 2*time.Second {
		t.Fatalf("the session ended after %s, before the keep window", took)
	}
}

func TestKeptSessionEndsWhenTheDesktopDies(t *testing.T) {
	k := startKept(t, time.Minute)
	c1 := k.connect()
	ready := c1.start()
	c1.stdin.Close()
	k.state(false)
	xvfb := k.xvfbPID()
	if xvfb == 0 {
		t.Fatalf("no Xvfb for %s", ready.Display)
	}
	if err := syscall.Kill(xvfb, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case <-k.exited:
		if k.runErr == nil || !strings.Contains(k.runErr.Error(), "desktop went away") {
			t.Fatalf("Serve: %v", k.runErr)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the session lingered on without its desktop")
	}
	if _, err := os.Lstat(k.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run dir still there: %v", err)
	}
}

func TestKeptSessionToldToStopEndsItsController(t *testing.T) {
	k := startKept(t, time.Minute)
	c1 := k.connect()
	ready := c1.start()
	k.cancel()
	if m := c1.recv(protocol.TypeEnded); !strings.Contains(m.Error, "told to stop") {
		t.Fatalf("ended says %q", m.Error)
	}
	select {
	case <-k.exited:
		if !errors.Is(k.runErr, context.Canceled) {
			t.Fatalf("Serve: %v", k.runErr)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the session did not stop")
	}
	if k.xvfbPID() != 0 {
		t.Fatalf("Xvfb for %s outlived the session", ready.Display)
	}
}

// TestStdioSessionIgnoresKeep: on stdio nobody can come back, so the keep
// window is not waited out.
func TestStdioSessionIgnoresKeep(t *testing.T) {
	h := startAgent(t, func(o *Options) { o.Keep = time.Hour })
	ready := h.start()
	if ready.Keep != 3600 {
		t.Fatalf("ready.Keep %d", ready.Keep)
	}
	h.stdin.Close()
	h.waitGone(ready.Display)
}
