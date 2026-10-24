//go:build linux

package daemon

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agent"
	"github.com/yasinmiran/robin/internal/protocol"
)

// TestMain lets the test binary stand in for robin-agent: run with
// ROBIN_TEST_DAEMON set and `daemon ...` arguments it is a fake daemon that
// listens as the real one does and echoes every message's type back.
func TestMain(m *testing.M) {
	if os.Getenv("ROBIN_TEST_DAEMON") != "" && len(os.Args) > 1 && os.Args[1] == "daemon" {
		os.Exit(fakeDaemon(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func fakeDaemon(args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	id := fs.String("id", "", "")
	dir := fs.String("dir", "", "")
	fs.Duration("keep", 0, "")
	fs.Bool("v", false, "")
	fs.Parse(args)
	if os.Getenv("ROBIN_TEST_DAEMON") == "die" {
		os.WriteFile(filepath.Join(*dir, LogFile), []byte("level=ERROR msg=\"no display for you\"\n"), 0o600)
		return 1
	}
	if os.Getenv("ROBIN_TEST_DAEMON") == "die-quiet" {
		return 1
	}
	logFile, _ := os.OpenFile(filepath.Join(*dir, LogFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	syscall.Dup3(int(logFile.Fd()), 2, 0)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ln, err := Listen(*dir, "fake-agent", log)
	if err != nil {
		log.Error("listen", "err", err)
		return 1
	}
	defer os.RemoveAll(*dir)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	ln.Changed(protocol.Session{ID: *id, PID: os.Getpid(), Connected: false})
	log.Info("fake daemon listening")
	for {
		conn, err := ln.Next(ctx)
		if err != nil {
			return 0
		}
		ln.Changed(protocol.Session{ID: *id, PID: os.Getpid(), Connected: true})
		for {
			m, err := conn.Recv(ctx)
			if err != nil {
				break
			}
			log.Info("echo", "type", m.Type)
			if m.Type == protocol.TypeBye {
				conn.Close()
				return 0
			}
			conn.Send(protocol.Message{Type: m.Type, Error: "echo"})
		}
		conn.Close()
		if ctx.Err() != nil {
			return 0
		}
	}
}

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func privateDir(t *testing.T) string {
	t.Helper()
	// t.TempDir is 0755 on some systems; the daemon wants 0700.
	dir := filepath.Join(t.TempDir(), "s1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func dialConn(t *testing.T, dir string) (*protocol.Conn, net.Conn) {
	t.Helper()
	c, err := net.Dial("unix", filepath.Join(dir, SocketFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return protocol.NewConn(c, c, 0, 0), c
}

func TestListenerHandsControllersToNext(t *testing.T) {
	dir := privateDir(t)
	ln, err := Listen(dir, "robin-agent-abc", testLog())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if fi, _ := os.Lstat(filepath.Join(dir, SocketFile)); fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode %o, want 0600", fi.Mode().Perm())
	}

	cli, raw := dialConn(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := ln.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cli.Send(protocol.Message{Type: protocol.TypeBye})
	if m, err := conn.Recv(ctx); err != nil || m.Type != protocol.TypeBye {
		t.Fatalf("daemon got %+v, %v", m, err)
	}
	// Closing the protocol connection closes the socket under it, so the
	// proxy on the other end sees EOF.
	conn.Close()
	raw.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := raw.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("read after the daemon closed: %v, want EOF", err)
	}

	ln.Changed(protocol.Session{ID: "s1", PID: 42})
	st, err := agent.ReadSession(dir)
	if err != nil || st.Agent != "robin-agent-abc" || st.PID != 42 {
		t.Fatalf("session.json %+v, %v", st, err)
	}
}

func TestListenerTurnsAwayASecondController(t *testing.T) {
	dir := privateDir(t)
	ln, err := Listen(dir, "a", testLog())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// Nobody calls Next: the session has its controller.
	cli, _ := dialConn(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), handoffWait+5*time.Second)
	defer cancel()
	m, err := cli.Recv(ctx)
	if err != nil || m.Type != protocol.TypeError || m.Error != errBusy.Error() || m.Code != protocol.CodeBusy {
		t.Fatalf("got %+v, %v; want error %q with code busy", m, err, errBusy)
	}
	if _, err := cli.Recv(ctx); err == nil {
		t.Fatal("the refused connection stayed open")
	}
}

func TestListenRefusesAnOpenDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "open")
	os.Mkdir(dir, 0o755)
	if _, err := Listen(dir, "a", testLog()); err == nil || !strings.Contains(err.Error(), "0700") {
		t.Fatalf("Listen on a 0755 dir: %v", err)
	}
	if _, err := Listen(filepath.Join(dir, "missing"), "a", testLog()); err == nil {
		t.Fatal("Listen on a missing dir succeeded")
	}
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o600)
	if _, err := Listen(file, "a", testLog()); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Listen on a file: %v", err)
	}
	deep := filepath.Join(t.TempDir(), strings.Repeat("d", 100))
	os.Mkdir(deep, 0o700)
	if _, err := Listen(deep, "a", testLog()); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("Listen with a long path: %v", err)
	}
}

func TestChangedWithoutItsDirectoryOnlyWarns(t *testing.T) {
	dir := privateDir(t)
	var logged bytes.Buffer
	ln, err := Listen(dir, "a", slog.New(slog.NewTextHandler(&logged, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	os.RemoveAll(dir)
	ln.Changed(protocol.Session{ID: "s1"})
	if !strings.Contains(logged.String(), "level=WARN") || !strings.Contains(logged.String(), "write session.json") {
		t.Fatalf("log after Changed without its directory:\n%s", logged.String())
	}
}

func TestCheckPeerWantsAUnixConnection(t *testing.T) {
	c, peer := net.Pipe()
	defer c.Close()
	defer peer.Close()
	if err := checkPeer(c); err == nil || !strings.Contains(err.Error(), "not a unix connection") {
		t.Fatalf("checkPeer on a pipe: %v", err)
	}
}

func TestCheckUID(t *testing.T) {
	if err := checkUID(&syscall.Ucred{Uid: 1000}, 1000); err != nil {
		t.Error(err)
	}
	if err := checkUID(&syscall.Ucred{Uid: 1001}, 1000); err == nil || !strings.Contains(err.Error(), "uid 1001") {
		t.Errorf("other uid: %v", err)
	}
}

// fakeSpawn starts this test binary as the daemon.
func fakeSpawn(t *testing.T, mode string) (Spawn, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROBIN_TEST_DAEMON", mode)
	root := filepath.Join(t.TempDir(), "run")
	return Spawn{Exe: exe, RunRoot: root, Keep: 3 * time.Second, Verbose: true}, root
}

func TestStartProxyAndEnd(t *testing.T) {
	sp, root := fakeSpawn(t, "1")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, dir, err := Start(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(root, id) || len(id) != 8 {
		t.Fatalf("id %q, dir %q", id, dir)
	}
	if found, err := Find(root, id); err != nil || found != dir {
		t.Fatalf("Find: %q, %v", found, err)
	}
	st, err := agent.ReadSession(dir)
	if err != nil || st.ID != id || st.Agent != "fake-agent" || st.PID == 0 {
		t.Fatalf("session.json %+v, %v", st, err)
	}
	killLater(t, st.PID)
	if os.Getppid() == st.PID || st.PID == os.Getpid() {
		t.Fatal("the daemon is not a process of its own")
	}
	// The daemon is in a session of its own, not ours.
	if sid, _, _ := syscall.Syscall(syscall.SYS_GETSID, uintptr(st.PID), 0, 0); int(sid) != st.PID {
		t.Errorf("daemon session id %d, want its own pid %d", sid, st.PID)
	}

	// A first controller, through the proxy: signaling both ways and the
	// daemon's log on stderr.
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var logOut bytes.Buffer
	proxyDone := make(chan error, 1)
	go func() { proxyDone <- Proxy(ctx, dir, inR, outW, &logOut, true) }()
	cli := protocol.NewConn(outR, inW, 0, 0)
	cli.Send(protocol.Message{Type: protocol.TypeStart})
	if m, err := cli.Recv(ctx); err != nil || m.Type != protocol.TypeStart || m.Error != "echo" {
		t.Fatalf("echo: %+v, %v", m, err)
	}
	if st, _ := agent.ReadSession(dir); !st.Connected {
		t.Error("session.json does not say a controller is connected")
	}
	// The controller leaves: stdin closes as when ssh goes.
	inW.Close()
	if err := <-proxyDone; err != nil {
		t.Fatalf("proxy after stdin closed: %v", err)
	}
	if !strings.Contains(logOut.String(), "fake daemon listening") || !strings.Contains(logOut.String(), "type=start") {
		t.Errorf("the daemon's log did not reach the proxy's stderr:\n%s", logOut.String())
	}
	if !isDaemon(st.PID, id) {
		t.Fatal("the daemon died with its controller")
	}

	// A second controller resumes and says bye; the daemon ends and the
	// proxy with it.
	inR, inW = io.Pipe()
	outR, outW = io.Pipe()
	logOut.Reset()
	go func() { proxyDone <- Proxy(ctx, dir, inR, outW, &logOut, false) }()
	cli = protocol.NewConn(outR, inW, 0, 0)
	cli.Send(protocol.Message{Type: protocol.TypeResume})
	if m, err := cli.Recv(ctx); err != nil || m.Type != protocol.TypeResume {
		t.Fatalf("echo: %+v, %v", m, err)
	}
	cli.Send(protocol.Message{Type: protocol.TypeBye})
	if err := <-proxyDone; err != nil {
		t.Fatalf("proxy after bye: %v", err)
	}
	if strings.Contains(logOut.String(), "listening") {
		t.Error("a resuming proxy replayed the log from its start")
	}
	waitGone(t, dir)
	if _, err := Find(root, id); err == nil || !strings.Contains(err.Error(), "no session") {
		t.Fatalf("Find after the end: %v", err)
	}
	if _, err := net.Dial("unix", filepath.Join(dir, SocketFile)); err == nil {
		t.Fatal("the socket is still accepting")
	}
}

func TestEndSignalsTheDaemon(t *testing.T) {
	sp, _ := fakeSpawn(t, "1")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, dir, err := Start(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := agent.ReadSession(dir)
	killLater(t, st.PID)
	if err := End(ctx, filepath.Dir(dir), id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run dir after End: %v", err)
	}
	// The daemon is our child here, so it is a zombie until Start's reaper
	// gets to it.
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(st.PID, 0) != syscall.ESRCH {
		if time.Now().After(deadline) {
			t.Fatal("the daemon is still running")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestEndRemovesAStaleDirectoryWithoutSignaling(t *testing.T) {
	ctx := context.Background()
	dir := privateDir(t)
	root := filepath.Dir(dir)
	// Our own pid is alive but is no daemon: End must not kill us.
	if err := agent.WriteSession(dir, protocol.Session{ID: "s1", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if err := End(ctx, root, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale dir after End: %v", err)
	}
	var noSession *NoSessionError
	if err := End(ctx, root, "s1"); !errors.As(err, &noSession) || !strings.Contains(err.Error(), "no session s1") {
		t.Fatalf("End of nothing: %v", err)
	}
	// A socket nobody listens on is what a killed daemon leaves.
	stale := filepath.Join(root, "s2")
	os.Mkdir(stale, 0o700)
	ln, err := net.Listen("unix", filepath.Join(stale, SocketFile))
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	os.WriteFile(filepath.Join(stale, "x"), nil, 0o600)
	if err := End(ctx, root, "s2"); err != nil {
		t.Fatalf("End of a dir with a dead socket: %v", err)
	}
	if _, err := os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dead socket's dir after End: %v", err)
	}
}

// TestEndLeavesWhatIsNoSession guards the agent directory and the run root:
// an id that names either, or a directory with no trace of a session, is
// refused and nothing is removed.
func TestEndLeavesWhatIsNoSession(t *testing.T) {
	ctx := context.Background()
	agentDir := t.TempDir()
	agentFile := filepath.Join(agentDir, "robin-agent-abc")
	os.WriteFile(agentFile, []byte("binary"), 0o700)
	root := filepath.Join(agentDir, "run")
	live := filepath.Join(root, "s1")
	os.MkdirAll(live, 0o700)
	os.WriteFile(filepath.Join(live, "Xauthority"), nil, 0o600)
	empty := filepath.Join(root, "s3")
	os.Mkdir(empty, 0o700)
	var noSession *NoSessionError
	for _, id := range []string{"", ".", "..", "/", "../robin-agent-abc", "s1/..", "S1"} {
		if err := End(ctx, root, id); err == nil || !strings.Contains(err.Error(), "bad session id") {
			t.Errorf("End(%q): %v", id, err)
		}
	}
	if err := End(ctx, root, "s1"); !errors.As(err, &noSession) {
		t.Errorf("End of a dir without session.json or socket: %v", err)
	}
	if err := End(ctx, root, "s3"); !errors.As(err, &noSession) {
		t.Errorf("End of an empty dir: %v", err)
	}
	for _, path := range []string{agentFile, root, live, filepath.Join(live, "Xauthority"), empty} {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("%s is gone: %v", path, err)
		}
	}
}

func TestProxyWithoutADaemon(t *testing.T) {
	dir := privateDir(t)
	err := Proxy(context.Background(), dir, strings.NewReader(""), io.Discard, io.Discard, false)
	if err == nil || !strings.Contains(err.Error(), "no session s1") {
		t.Fatalf("got %v", err)
	}
}

func TestStartReportsADaemonThatDies(t *testing.T) {
	sp, root := fakeSpawn(t, "die")
	_, _, err := Start(context.Background(), sp)
	if err == nil || !strings.Contains(err.Error(), "exited before listening") || !strings.Contains(err.Error(), "no display for you") {
		t.Fatalf("got %v", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("run dir left behind: %v", entries)
	}
}

func TestStartReportsADaemonThatDiesQuietly(t *testing.T) {
	sp, _ := fakeSpawn(t, "die-quiet")
	_, _, err := Start(context.Background(), sp)
	if err == nil || !strings.Contains(err.Error(), "exited before listening") || strings.Contains(err.Error(), "daemon log:") {
		t.Fatalf("got %v", err)
	}
}

func TestStartFailsWithoutAnExecutable(t *testing.T) {
	root := t.TempDir()
	_, _, err := Start(context.Background(), Spawn{Exe: filepath.Join(root, "missing"), RunRoot: root})
	if err == nil || !strings.Contains(err.Error(), "start daemon") {
		t.Fatalf("got %v", err)
	}
}

func TestCommandLines(t *testing.T) {
	got := daemonArgs("/a/robin-agent", "ab12", "/a/run/ab12", 8*time.Hour, true)
	want := "/a/robin-agent daemon -id ab12 -dir /a/run/ab12 -keep 8h0m0s -v"
	if strings.Join(got, " ") != want {
		t.Errorf("daemon args %q, want %q", strings.Join(got, " "), want)
	}
	got = systemdRunArgs("ab12", []string{"PATH=/bin", "GOCOVERDIR=/cover"}, []string{"/a/robin-agent", "daemon"})
	want = "--user --quiet --collect --unit=robin-ab12 --setenv=PATH=/bin --setenv=GOCOVERDIR=/cover -- /a/robin-agent daemon"
	if strings.Join(got, " ") != want {
		t.Errorf("systemd-run args %q, want %q", strings.Join(got, " "), want)
	}
	sp := Spawn{Exe: "/a/robin-agent", Detach: protocol.DetachSystemdRun}
	if cmd := sp.command("ab12", "/a/run/ab12"); filepath.Base(cmd.Path) != "systemd-run" && cmd.Args[0] != "systemd-run" {
		t.Errorf("systemd-run command runs %q", cmd.Args)
	}
	plain := Spawn{Exe: "/a/robin-agent"}
	if cmd := plain.command("ab12", "/a/run/ab12"); !cmd.SysProcAttr.Setsid {
		t.Error("a setsid daemon is not given its own session")
	}
}

func TestCommandIsDaemon(t *testing.T) {
	split := func(s string) [][]byte { return bytes.Split([]byte(s), []byte{0}) }
	if !commandIsDaemon(split("/x/robin-agent\x00daemon\x00-id\x00ab12\x00-dir\x00/d\x00"), "ab12") {
		t.Error("the daemon's own command line was not recognised")
	}
	for _, c := range []string{"/x/robin-agent\x00daemon\x00-id\x00other\x00", "/x/robin-agent\x00serve\x00-resume\x00ab12\x00", "daemon\x00-id\x00ab12\x00", ""} {
		if commandIsDaemon(split(c), "ab12") {
			t.Errorf("%q taken for the daemon of ab12", c)
		}
	}
	if isDaemon(os.Getpid(), "ab12") || isDaemon(0, "ab12") || isDaemon(1<<30, "ab12") {
		t.Error("isDaemon found a daemon where there is none")
	}
	if findDaemon("no-such-session-anywhere") != 0 {
		t.Error("findDaemon found a daemon where there is none")
	}
}

func TestEndFindsADaemonWithoutSessionJSON(t *testing.T) {
	sp, root := fakeSpawn(t, "1")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, dir, err := Start(ctx, sp)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := agent.ReadSession(dir)
	killLater(t, st.PID)
	os.Remove(filepath.Join(dir, agent.SessionFile))
	if got := findDaemon(id); got != st.PID {
		t.Fatalf("findDaemon = %d, want %d", got, st.PID)
	}
	if err := End(ctx, root, id); err != nil {
		t.Fatal(err)
	}
	waitGone(t, dir)
}

func TestFindRejectsOddIDs(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"", ".", "..", "../x", "a/b", "AB12", "ab-12"} {
		if _, err := Find(root, id); err == nil || !strings.Contains(err.Error(), "bad session id") {
			t.Errorf("Find(%q): %v", id, err)
		}
	}
	var noSession *NoSessionError
	if _, err := Find(root, "nope"); !errors.As(err, &noSession) || !strings.Contains(err.Error(), "no session nope") {
		t.Errorf("Find of a missing session: %v", err)
	}
	// A plain file where the socket should be is not a session either.
	os.MkdirAll(filepath.Join(root, "f"), 0o700)
	os.WriteFile(filepath.Join(root, "f", SocketFile), nil, 0o600)
	if _, err := Find(root, "f"); err == nil {
		t.Error("Find took a plain file for a socket")
	}
}

// killLater makes sure a daemon a test started does not outlive the test
// when the test fails before ending it: the daemon runs in a session of
// its own, which no cleanup of the test's process group reaches.
func killLater(t *testing.T, pid int) {
	t.Helper()
	if pid <= 0 {
		t.Fatal("no daemon pid to clean up")
	}
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
}

func waitGone(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s is still there", dir)
}

func TestNewIDNeverReadsAsAPid(t *testing.T) {
	for i := 0; i < 2000; i++ {
		id, err := newID()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := strconv.Atoi(id); err == nil || len(id) != 8 || strings.Trim(id, "0123456789abcdef") != "" {
			t.Fatalf("id %q", id)
		}
	}
}
