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
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agent"
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
	log  *bytes.Buffer
	done chan error
	msgs chan protocol.Message
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
	p := &proxy{t: t, cmd: cmd, in: in, log: &bytes.Buffer{}, done: make(chan error, 1), msgs: make(chan protocol.Message, 256)}
	cmd.Stderr = p.log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.done <- cmd.Wait() }()
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
	case err := <-p.done:
		return err
	case <-time.After(10 * time.Second):
		p.t.Fatalf("the proxy did not exit; its stderr:\n%s", p.log)
		return nil
	}
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) != syscall.ESRCH }

func xvfbPID(display string) int {
	b, err := os.ReadFile("/tmp/.X" + strings.TrimPrefix(display, ":") + "-lock")
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

func start(p *proxy) *protocol.Ready {
	p.t.Helper()
	p.conn.Send(protocol.Message{Type: protocol.TypeStart, Start: &protocol.Start{
		Version: protocol.Version, Width: 640, Height: 480, MaxWidth: 1280, MaxHeight: 720, FPS: 10, Encoder: protocol.EncoderSoftware,
	}})
	return p.recv(protocol.TypeReady).Ready
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
	ready := start(p1)
	if ready.ID == "" || ready.Keep != 4 || ready.Mode != protocol.ModeSpawn {
		t.Fatalf("ready %+v", ready)
	}
	dir := filepath.Join(filepath.Dir(exe), "run", ready.ID)
	st, err := agent.ReadSession(dir)
	if err != nil || !st.Connected || st.Agent != filepath.Base(exe) || st.Build != version {
		t.Fatalf("session.json %+v, %v", st, err)
	}
	if st.PID == p1.cmd.Process.Pid || !alive(st.PID) {
		t.Fatalf("daemon pid %d (proxy %d)", st.PID, p1.cmd.Process.Pid)
	}
	xvfb := xvfbPID(ready.Display)
	if !alive(xvfb) {
		t.Fatalf("no Xvfb for %s", ready.Display)
	}
	if !strings.Contains(p1.log.String(), "desktop up") {
		t.Errorf("the daemon's log did not reach the proxy's stderr:\n%s", p1.log)
	}

	// sshd goes: the proxy's whole process group dies at once.
	p1.killGroup()
	time.Sleep(time.Second)
	if !alive(st.PID) || !alive(xvfb) || xvfbPID(ready.Display) != xvfb {
		t.Fatal("the daemon or its display died with the proxy")
	}
	if st2, err := agent.ReadSession(dir); err != nil || st2.Connected || st2.Until == nil {
		t.Fatalf("session.json while kept: %+v, %v", st2, err)
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
	ready := start(p)
	dir := filepath.Join(filepath.Dir(exe), "run", ready.ID)
	st, _ := agent.ReadSession(dir)
	xvfb := xvfbPID(ready.Display)
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
	if out, err := end.CombinedOutput(); err == nil || !strings.Contains(string(out), "no session") {
		t.Fatalf("end of nothing: %v\n%s", err, out)
	}
}

func TestServeWithoutKeepEndsWithItsController(t *testing.T) {
	exe := agentCopy(t)
	p := serveAgent(t, exe)
	ready := start(p)
	dir := filepath.Join(filepath.Dir(exe), "run", ready.ID)
	xvfb := xvfbPID(ready.Display)
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
