//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agent"
	"github.com/yasinmiran/robin/internal/agent/agenttest"
	"github.com/yasinmiran/robin/internal/protocol"
)

// TestMain lets this test binary be robin-agent itself when ROBIN_AGENT_MAIN
// is set, so the tests below run serve, daemon and end as real processes.
func TestMain(m *testing.M) {
	if os.Getenv("ROBIN_AGENT_MAIN") != "" {
		main()
		return
	}
	os.Exit(m.Run())
}

// agentCopy puts the test binary in a writable directory of its own, since
// the agent keeps its sessions beside itself and the test binary may live
// on a read-only mount.
func agentCopy(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("Xvfb"); err != nil {
		t.Skip("Xvfb not installed; run via internal/agent/x11/testdata/xtest.sh")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(t.TempDir(), "robin-agent-test")
	if err := os.WriteFile(exe, b, 0o700); err != nil {
		t.Fatal(err)
	}
	return exe
}

// proxy is a running `robin-agent serve`, driven as the CLI drives it.
type proxy struct {
	t    *testing.T
	cmd  *exec.Cmd
	conn *protocol.Conn
	in   io.WriteCloser
	log  *syncBuffer
	// done closes once the proxy exited; exitErr is cmd.Wait's result.
	done    chan struct{}
	exitErr error
	msgs    chan protocol.Message
}

func serveAgent(t *testing.T, exe string, args ...string) *proxy {
	t.Helper()
	cmd := exec.Command(exe, append([]string{"serve", "-v"}, args...)...)
	cmd.Env = append(os.Environ(), "ROBIN_AGENT_MAIN=1")
	// Its own process group, as an sshd session gives it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{t: t, cmd: cmd, in: in, log: &syncBuffer{}, done: make(chan struct{}), msgs: make(chan protocol.Message, 256)}
	cmd.Stderr = p.log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		p.exitErr = cmd.Wait()
		close(p.done)
	}()
	p.conn = protocol.NewConn(out, in, protocol.DefaultPingInterval, 0)
	go func() {
		defer close(p.msgs)
		for {
			m, err := p.conn.Recv(context.Background())
			if err != nil {
				return
			}
			p.msgs <- m
		}
	}()
	t.Cleanup(func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-p.done
	})
	return p
}

func (p *proxy) recv(want protocol.Type) protocol.Message {
	p.t.Helper()
	for {
		select {
		case m, ok := <-p.msgs:
			if !ok {
				p.t.Fatalf("the proxy closed while waiting for %s; its stderr:\n%s", want, p.log)
			}
			if m.Type == want {
				return m
			}
			if m.Type == protocol.TypeError {
				p.t.Fatalf("agent error while waiting for %s: %s\n%s", want, m.Error, p.log)
			}
		case <-time.After(30 * time.Second):
			p.t.Fatalf("timed out waiting for %s; the proxy's stderr:\n%s", want, p.log)
		}
	}
}

// killGroup ends the proxy the way a dying sshd does: everything in the
// exec channel's process group, at once.
func (p *proxy) killGroup() {
	p.t.Helper()
	syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		p.t.Fatal("the proxy did not die")
	}
}

func (p *proxy) exited() error {
	p.t.Helper()
	select {
	case <-p.done:
		return p.exitErr
	case <-time.After(10 * time.Second):
		p.t.Fatalf("the proxy did not exit; its stderr:\n%s", p.log)
		return nil
	}
}

// syncBuffer collects the proxy's stderr while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var alive, xvfbPID = agenttest.Alive, agenttest.XvfbPID

// start begins a session through p and returns its Ready and run
// directory. The daemon runs in a session of its own, so no cleanup of
// the proxy's process group reaches it: a test that fails before ending
// the session would leave a desktop on this machine for the keep window.
// The daemon is killed when the test ends instead.
func start(p *proxy, exe string) (*protocol.Ready, string) {
	p.t.Helper()
	p.conn.Send(protocol.Message{Type: protocol.TypeStart, Start: &protocol.Start{
		Version: protocol.Version, Width: 640, Height: 480, MaxWidth: 1280, MaxHeight: 720, FPS: 10, Encoder: protocol.EncoderSoftware,
	}})
	ready := p.recv(protocol.TypeReady).Ready
	dir := filepath.Join(filepath.Dir(exe), "run", ready.ID)
	st, err := agent.ReadSession(dir)
	if err != nil || st.PID <= 0 {
		p.t.Fatalf("session.json after ready: %+v, %v", st, err)
	}
	p.t.Cleanup(func() {
		syscall.Kill(st.PID, syscall.SIGKILL)
		os.RemoveAll(dir)
	})
	return ready, dir
}

// waitSession polls session.json until ok says it is as expected.
func waitSession(t *testing.T, dir string, ok func(protocol.Session) bool) protocol.Session {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := agent.ReadSession(dir)
		if err == nil && ok(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("session.json never became what was wanted: %+v, %v", st, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitRunDirGone waits for the daemon to remove its directory.
func waitRunDirGone(t *testing.T, dir string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s is still there after %s", dir, within)
}

func TestDaemonOutlivesTheProxyAndEndsWithTheKeepWindow(t *testing.T) {
	exe := agentCopy(t)
	p1 := serveAgent(t, exe, "-keep", "4s")
	ready, dir := start(p1, exe)
	if ready.ID == "" || ready.Keep != 4 || ready.Mode != protocol.ModeSpawn {
		t.Fatalf("ready %+v", ready)
	}
	st, err := agent.ReadSession(dir)
	if err != nil || !st.Connected || st.Agent != filepath.Base(exe) || st.Build != version {
		t.Fatalf("session.json %+v, %v", st, err)
	}
	if st.PID == p1.cmd.Process.Pid || !alive(st.PID) {
		t.Fatalf("daemon pid %d (proxy %d)", st.PID, p1.cmd.Process.Pid)
	}
	xvfb := xvfbPID(dir)
	if xvfb == 0 {
		t.Fatalf("no Xvfb for %s", ready.Display)
	}
	// The proxy tails the daemon's log a few times a second.
	logDeadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(p1.log.String(), "desktop up") {
		if time.Now().After(logDeadline) {
			t.Errorf("the daemon's log did not reach the proxy's stderr:\n%s", p1.log)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// sshd goes: the proxy's whole process group dies at once. The daemon
	// notices the socket's end and records the keep window.
	p1.killGroup()
	waitSession(t, dir, func(s protocol.Session) bool { return !s.Connected && s.Until != nil })
	if !alive(st.PID) || xvfbPID(dir) != xvfb {
		t.Fatal("the daemon or its display died with the proxy")
	}

	// A new proxy resumes.
	p2 := serveAgent(t, exe, "-resume", ready.ID)
	p2.conn.Send(protocol.Message{Type: protocol.TypeResume, Resume: &protocol.Resume{Version: protocol.Version, ID: ready.ID}})
	again := p2.recv(protocol.TypeReady).Ready
	if !again.Resumed || again.Display != ready.Display || again.TunnelAddr != ready.TunnelAddr {
		t.Fatalf("resumed ready %+v", again)
	}
	// It leaves by closing stdin; the proxy exits cleanly and the keep
	// window starts over.
	left := time.Now()
	p2.in.Close()
	if err := p2.exited(); err != nil {
		t.Fatalf("proxy exit: %v\n%s", err, p2.log)
	}
	waitRunDirGone(t, dir, 20*time.Second)
	if kept := time.Since(left); kept < 4*time.Second {
		t.Fatalf("the session ended %s after its controller left, before the 4s keep window", kept)
	}
	deadline := time.Now().Add(5 * time.Second)
	for alive(st.PID) || alive(xvfb) {
		if time.Now().After(deadline) {
			t.Fatalf("after the keep window: daemon alive %v, Xvfb alive %v", alive(st.PID), alive(xvfb))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if out, _ := exec.Command("pgrep", "-f", "robin-display-watchdog|Xvfb "+ready.Display+" ").Output(); len(out) > 0 {
		t.Fatalf("processes left behind:\n%s", out)
	}
}

func TestEndStopsAKeptSession(t *testing.T) {
	exe := agentCopy(t)
	p := serveAgent(t, exe, "-keep", "1h")
	ready, dir := start(p, exe)
	st, _ := agent.ReadSession(dir)
	xvfb := xvfbPID(dir)
	if xvfb == 0 {
		t.Fatalf("no Xvfb for %s", ready.Display)
	}
	p.killGroup()

	end := exec.Command(exe, "end", ready.ID)
	end.Env = append(os.Environ(), "ROBIN_AGENT_MAIN=1")
	if out, err := end.CombinedOutput(); err != nil {
		t.Fatalf("end: %v\n%s", err, out)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("end returned with the run dir still there")
	}
	deadline := time.Now().Add(5 * time.Second)
	for alive(st.PID) || alive(xvfb) {
		if time.Now().After(deadline) {
			t.Fatalf("after end: daemon alive %v, Xvfb alive %v", alive(st.PID), alive(xvfb))
		}
		time.Sleep(50 * time.Millisecond)
	}
	end = exec.Command(exe, "end", ready.ID)
	end.Env = append(os.Environ(), "ROBIN_AGENT_MAIN=1")
	out, err := end.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != protocol.ExitNoSession || !strings.Contains(string(out), "no session") {
		t.Fatalf("end of nothing: %v\n%s", err, out)
	}
}

func TestEndRefusesWhatIsNoSessionID(t *testing.T) {
	exe := agentCopy(t)
	root := filepath.Join(filepath.Dir(exe), "run")
	live := filepath.Join(root, "abcd1234")
	os.MkdirAll(live, 0o700)
	os.WriteFile(filepath.Join(live, "Xauthority"), nil, 0o600)
	for _, id := range []string{"", ".", "..", "/", "../" + filepath.Base(exe)} {
		end := exec.Command(exe, "end", id)
		end.Env = append(os.Environ(), "ROBIN_AGENT_MAIN=1")
		out, err := end.CombinedOutput()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(string(out), "bad session id") {
			t.Errorf("end %q: %v\n%s", id, err, out)
		}
	}
	for _, path := range []string{exe, root, filepath.Join(live, "Xauthority")} {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("%s is gone: %v", path, err)
		}
	}
}

func TestServeWithoutKeepEndsWithItsController(t *testing.T) {
	exe := agentCopy(t)
	p := serveAgent(t, exe)
	ready, dir := start(p, exe)
	xvfb := xvfbPID(dir)
	if xvfb == 0 {
		t.Fatalf("no Xvfb for %s", ready.Display)
	}
	p.killGroup()
	waitRunDirGone(t, dir, 10*time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for alive(xvfb) {
		if time.Now().After(deadline) {
			t.Fatal("Xvfb outlived a session with no keep window")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestResumeOfAnUnknownSessionFailsFast(t *testing.T) {
	exe := agentCopy(t)
	p := serveAgent(t, exe, "-resume", "zz99")
	if err := p.exited(); err == nil || !strings.Contains(p.log.String(), "no session zz99") {
		t.Fatalf("exit %v, stderr:\n%s", err, p.log)
	}
}
