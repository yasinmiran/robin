//go:build linux

package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agent/peer"
	"github.com/yasinmiran/robin/internal/protocol"
)

func TestStdioYieldsOneController(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	cs := Stdio(r, w)
	ctx := context.Background()
	conn, err := cs.Next(ctx)
	if err != nil || conn == nil {
		t.Fatalf("first Next: %v, %v", conn, err)
	}
	conn.Close()
	if _, err := cs.Next(ctx); !errors.Is(err, ErrNoControllers) {
		t.Fatalf("second Next: %v, want ErrNoControllers", err)
	}
	cs.Changed(protocol.Session{}) // nothing to record
}

// pair is a controller connection and the CLI's end of it.
func pair(t *testing.T) (agent, cli *protocol.Conn) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	agent = protocol.NewConn(inR, outW, 0, 0)
	cli = protocol.NewConn(outR, inW, 0, 0)
	t.Cleanup(func() {
		inW.Close()
		outW.Close()
	})
	return agent, cli
}

func TestAwaitResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name string
		m    protocol.Message
		want string
	}{
		{"start instead", protocol.Message{Type: protocol.TypeStart, Start: &protocol.Start{Version: protocol.Version}}, `expected resume, got "start"`},
		{"no body", protocol.Message{Type: protocol.TypeResume}, `expected resume`},
		{"wrong version", protocol.Message{Type: protocol.TypeResume, Resume: &protocol.Resume{Version: protocol.Version + 1, ID: "ab12"}},
			"the CLI speaks protocol"},
		{"wrong id", protocol.Message{Type: protocol.TypeResume, Resume: &protocol.Resume{Version: protocol.Version, ID: "other"}}, `no session "other" here`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent, cli := pair(t)
			go cli.Send(tc.m)
			_, err := awaitResume(ctx, agent, "ab12")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			// The CLI hears why.
			if m, err := cli.Recv(ctx); err != nil || m.Type != protocol.TypeError || !strings.Contains(m.Error, tc.want) {
				t.Fatalf("CLI got %+v, %v", m, err)
			}
		})
	}
	agent, cli := pair(t)
	go cli.Send(protocol.Message{Type: protocol.TypeResume, Resume: &protocol.Resume{Version: protocol.Version, ID: "ab12", Curtain: true}})
	if r, err := awaitResume(ctx, agent, "ab12"); err != nil || !r.Curtain {
		t.Fatalf("resume %+v, %v; want the CLI's, curtain and all", r, err)
	}
}

func TestAwaitResumeGivesUpOnAClosedController(t *testing.T) {
	inR, inW := io.Pipe()
	_, outW := io.Pipe()
	agent := protocol.NewConn(inR, outW, 0, 0)
	inW.Close()
	if _, err := awaitResume(context.Background(), agent, "x"); !errors.Is(err, io.EOF) {
		t.Fatalf("got %v, want EOF", err)
	}
}

// deadPID is a pid no process has: a child that has already been reaped.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func TestSweepRunRootRemovesOnlyTheDead(t *testing.T) {
	root := t.TempDir()
	dead := deadPID(t)
	mk := func(name string) string {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	deadByName := mk(strconv.Itoa(dead))
	liveByName := mk(strconv.Itoa(os.Getpid()))
	deadByFile := mk("ab12")
	WriteSession(deadByFile, protocol.Session{ID: "ab12", PID: dead})
	liveByFile := mk("cd34")
	WriteSession(liveByFile, protocol.Session{ID: "cd34", PID: fakeDaemon(t, "cd34")})
	// A live process of this user's, but not the session's daemon.
	notTheDaemon := mk("cd35")
	WriteSession(notTheDaemon, protocol.Session{ID: "cd35", PID: os.Getppid()})
	unknown := mk("ef56") // no pid anywhere: a daemon still starting
	keep := mk("0011")
	WriteSession(keep, protocol.Session{ID: "0011", PID: dead})
	os.WriteFile(filepath.Join(root, "stray"), nil, 0o600)

	sweepRunRoot(root, keep)

	for _, gone := range []string{deadByName, deadByFile, notTheDaemon} {
		if _, err := os.Lstat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the sweep", filepath.Base(gone))
		}
	}
	for _, kept := range []string{liveByName, liveByFile, unknown, keep, filepath.Join(root, "stray")} {
		if _, err := os.Lstat(kept); err != nil {
			t.Errorf("%s was swept: %v", filepath.Base(kept), err)
		}
	}
}

func TestPrepareRunDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "run")
	dir, err := prepareRunDir(Options{RunRoot: root})
	if err != nil || dir != filepath.Join(root, strconv.Itoa(os.Getpid())) {
		t.Fatalf("by pid: %q, %v", dir, err)
	}
	if fi, _ := os.Lstat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("mode %o", fi.Mode().Perm())
	}

	own := filepath.Join(root, "ab12")
	os.Mkdir(own, 0o700)
	stale := filepath.Join(root, strconv.Itoa(deadPID(t)))
	os.Mkdir(stale, 0o700)
	got, err := prepareRunDir(Options{RunRoot: root, RunDir: own})
	if err != nil || got != own {
		t.Fatalf("own dir: %q, %v", got, err)
	}
	if _, err := os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Error("a dead session's directory survived")
	}
	if _, err := os.Lstat(own); err != nil {
		t.Error("the caller's own directory was swept")
	}
}

func TestSessionFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	want := protocol.Session{
		ID: "ab12", PID: 7, Agent: "robin-agent-0123", Build: "v1", Mode: protocol.ModeSpawn, Display: ":7",
		Desktop: "openbox + xterm", Keep: 3600, Started: until.Add(-time.Hour), Until: &until,
	}
	if err := WriteSession(dir, want); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, SessionFile))
	if strings.Count(strings.TrimSpace(string(b)), "\n") != 0 {
		t.Errorf("session.json is not one line:\n%s", b)
	}
	got, err := ReadSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Times come back in UTC; compare the rest field for field.
	got.Started, want.Started = got.Started.UTC(), want.Started.UTC()
	*got.Until, *want.Until = got.Until.UTC(), want.Until.UTC()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v; want %+v", got, want)
	}
	if _, err := ReadSession(t.TempDir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ReadSession of nothing: %v", err)
	}
	if sessionPID(t.TempDir()) != 0 {
		t.Error("sessionPID of nothing is not 0")
	}
	if err := WriteSession(filepath.Join(t.TempDir(), "missing"), want); err == nil {
		t.Error("WriteSession into a missing directory succeeded")
	}
	os.WriteFile(filepath.Join(dir, SessionFile), []byte("{not json"), 0o600)
	if _, err := ReadSession(dir); err == nil {
		t.Error("ReadSession took malformed JSON")
	}
}

func TestSweepRunRootTrustsSessionJSONOverTheName(t *testing.T) {
	root := t.TempDir()
	mk := func(name string) string {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	// An all-digit id names a live daemon in its session.json; the name is
	// no pid.
	live := mk("12345678")
	WriteSession(live, protocol.Session{ID: "12345678", PID: fakeDaemon(t, "12345678")})
	// A daemon still starting has a socket and no session.json yet.
	starting := mk("23456789")
	ln, err := net.Listen("unix", filepath.Join(starting, SocketFile))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fakeDaemon(t, "23456789")
	// A dead daemon's session.json wins over a name that is a live pid.
	dead := mk(strconv.Itoa(os.Getpid()))
	WriteSession(dead, protocol.Session{ID: "x", PID: deadPID(t)})

	sweepRunRoot(root, filepath.Join(root, "other"))

	for _, kept := range []string{live, starting} {
		if _, err := os.Lstat(kept); err != nil {
			t.Errorf("%s was swept: %v", filepath.Base(kept), err)
		}
	}
	if _, err := os.Lstat(dead); !errors.Is(err, os.ErrNotExist) {
		t.Error("a dead daemon's directory survived because its name is a live pid")
	}
}

func TestServeGivesUpWithoutAFirstController(t *testing.T) {
	none := &feed{conns: make(chan *protocol.Conn), changes: make(chan protocol.Session, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := Serve(ctx, none, Options{RunRoot: t.TempDir(), FirstController: 200 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "no controller connected within") || time.Since(start) > 3*time.Second {
		t.Fatalf("Serve with no controller: %v after %s", err, time.Since(start))
	}
}

func TestTakeResumeRefusesDevicesTheSessionLacks(t *testing.T) {
	withMic := &session{start: protocol.Start{Mic: true}}
	if err := withMic.takeResume(protocol.Resume{ID: "ab12", Mic: true}); err != nil {
		t.Fatalf("a session with a microphone refused --mic: %v", err)
	}
	for _, tc := range []struct {
		name string
		s    *session
		r    protocol.Resume
		want []string
	}{
		{"camera", withMic, protocol.Resume{ID: "ab12", Mic: true, Camera: true},
			[]string{"session ab12 started without a camera", "resume it without --camera,"}},
		{"both", &session{}, protocol.Resume{ID: "cd34", Mic: true, Camera: true},
			[]string{"without a microphone or a camera", "resume it without --mic and --camera,", "robin end"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.s.takeResume(tc.r)
			for _, want := range tc.want {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %v, want it to say %q", err, want)
				}
			}
		})
	}
}

// TestTheTransportIsTheAttachedCLIs: the owner's page gets a TCP-only peer
// while the controller attached now runs through the tunnel alone,
// whatever the session started with, and a guest's page while its own CLI
// does. A refused resume leaves the transport as it was. The session here
// has no ICE-TCP listener, so a TCP-only peer cannot be made at all: that
// failure is how the test sees which one was asked for.
func TestTheTransportIsTheAttachedCLIs(t *testing.T) {
	s := testSession(&richDesktop{})
	tcpOnly := func(g *guest) bool {
		t.Helper()
		s.mu.Lock()
		defer s.mu.Unlock()
		v, err := s.openViewer(g)
		if err != nil {
			if !errors.Is(err, peer.ErrTCPOnlyNeedsMux) {
				t.Fatal(err)
			}
			return true
		}
		s.stream.remove(v.sub)
		v.peer.Close()
		return false
	}
	if tcpOnly(nil) {
		t.Fatal("a session started without the tunnel made a TCP-only peer")
	}
	if err := s.takeResume(protocol.Resume{ID: "ab12", Tunnel: true}); err != nil {
		t.Fatal(err)
	}
	if !tcpOnly(nil) {
		t.Error("resumed through the tunnel, the owner's page gets UDP")
	}
	if tcpOnly(&guest{}) {
		t.Error("a guest whose CLI did not ask for the tunnel got a TCP-only peer")
	}
	if !tcpOnly(&guest{tunnel: true}) {
		t.Error("a guest whose CLI asked for the tunnel gets UDP")
	}
	if err := s.takeResume(protocol.Resume{ID: "ab12", Mic: true}); err == nil {
		t.Fatal("a resume with a microphone the session lacks passed")
	}
	if !tcpOnly(nil) {
		t.Error("a refused resume took the tunnel away")
	}
	if err := s.takeResume(protocol.Resume{ID: "ab12"}); err != nil {
		t.Fatal(err)
	}
	if tcpOnly(nil) {
		t.Error("resumed without the tunnel, the owner's page still gets a TCP-only peer")
	}
}

func TestTakeResumeRefusesPrivateOnASpawnedDesktop(t *testing.T) {
	spawned := &session{}
	if err := spawned.takeResume(protocol.Resume{ID: "ab12"}); err != nil {
		t.Fatalf("a plain resume: %v", err)
	}
	for _, r := range []protocol.Resume{{Curtain: true}, {Silent: true}} {
		if err := spawned.takeResume(r); err == nil || !strings.Contains(err.Error(), "for a desktop you attach to") {
			t.Errorf("resume %+v of a spawned desktop: %v", r, err)
		}
	}
}

// TestLingerGivesTheResumeWhatItAsks: a kept attached desktop resumed with
// --mic it never had is refused and goes on waiting; resumed with
// --curtain, it draws the curtain before Ready, which says so.
func TestLingerGivesTheResumeWhatItAsks(t *testing.T) {
	rig, err := newPrivate(t, protocol.Start{})
	if err != nil {
		t.Fatal(err)
	}
	s := &session{log: slog.New(slog.NewTextHandler(io.Discard, nil)), private: rig.p, conn: newController(nil),
		encoder: &videoEncoder{name: protocol.EncoderSoftware}}
	f := &feed{conns: make(chan *protocol.Conn, 2), changes: make(chan protocol.Session, 4)}
	ready := &protocol.Ready{Version: protocol.Version, ID: "ab12", Mode: protocol.ModeAttach}

	refused, refusedCLI := pair(t)
	resumed, resumedCLI := pair(t)
	f.conns <- refused
	f.conns <- resumed
	type lingered struct {
		conn *protocol.Conn
		err  error
	}
	got := make(chan lingered, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	lingering := make(chan struct{})
	// A failure below returns before linger does; it ends before the rig.
	t.Cleanup(func() {
		cancel()
		<-lingering
	})
	go func() {
		defer close(lingering)
		c, err := s.linger(ctx, f, ready, nil, time.Now().Add(time.Minute))
		got <- lingered{c, err}
	}()

	go refusedCLI.Send(protocol.Message{Type: protocol.TypeResume, Resume: &protocol.Resume{Version: protocol.Version, ID: "ab12", Mic: true}})
	if m, err := refusedCLI.Recv(ctx); err != nil || m.Type != protocol.TypeError || !strings.Contains(m.Error, "--mic") {
		t.Fatalf("the CLI that asked for --mic got %+v, %v", m, err)
	}
	go resumedCLI.Send(protocol.Message{Type: protocol.TypeResume, Resume: &protocol.Resume{Version: protocol.Version, ID: "ab12", Curtain: true}})
	m, err := resumedCLI.Recv(ctx)
	if err != nil || m.Type != protocol.TypeReady || !m.Ready.Resumed || m.Ready.Private == nil || !m.Ready.Private.Curtain.On {
		t.Fatalf("the CLI that asked for --curtain got %+v, %v", m, err)
	}
	if !rig.curtain.isDrawn() {
		t.Error("Ready says the curtain is on, and it is not drawn")
	}
	if l := <-got; l.err != nil || l.conn != resumed {
		t.Errorf("linger resumed %p, %v; want the second controller %p", l.conn, l.err, resumed)
	}
}
