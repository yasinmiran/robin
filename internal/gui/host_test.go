package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agentbin"
	"github.com/yasinmiran/robin/internal/probe"
	"github.com/yasinmiran/robin/internal/protocol"
	"github.com/yasinmiran/robin/internal/relay"
	"github.com/yasinmiran/robin/internal/sshmux"
	"github.com/yasinmiran/robin/internal/sshmux/sshtest"
)

// sshHost is a host over sshtest's throwaway sshd on this machine, with a
// live master, the way connect leaves one.
func sshHost(t *testing.T) (*host, *sshtest.Server, *lockedBuffer) {
	t.Helper()
	s := sshtest.Start(t)
	stderr := &lockedBuffer{}
	o, log := Options{Dest: s.Dest, SSHArgs: s.Args, Stderr: stderr, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}.fill()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m, err := sshmux.Dial(ctx, o.Dest, sshmux.Options{Args: o.SSHArgs, Stderr: o.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	h := &host{o: o, log: log, m: m}
	t.Cleanup(h.close)
	return h, s, stderr
}

// lockedBuffer is a terminal the agent's and ssh's output goes to while
// the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// fakeServe stands in for robin-agent serve on the host: a script that
// records its arguments, writes stderr to its stderr, answers each of the
// CLI's messages with the next of answers, and holds the channel open
// until the CLI closes it.
func fakeServe(t *testing.T, stderr string, answers ...protocol.Message) (path, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	path = filepath.Join(dir, "robin-agent")
	script := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %s\nprintf '%%s' '%s' >&2\n", calls, stderr)
	for _, a := range answers {
		b, err := json.Marshal(a)
		if err != nil || strings.ContainsRune(string(b), '\'') {
			t.Fatalf("answer %+v: %s, %v", a, b, err)
		}
		script += fmt.Sprintf("read -r line || exit 0\nprintf '%%s\\n' '%s'\n", b)
	}
	script += "exec cat >/dev/null\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path, calls
}

func resumedReady(id string) protocol.Message {
	return protocol.Message{Type: protocol.TypeReady, Ready: &protocol.Ready{Version: protocol.Version, ID: id, Resumed: true, Keep: 60}}
}

// TestReconnectResumesTheKeptSession: a redial runs the agent's serve
// with -resume over a fresh connection and goes on with the session it
// answers for, with the agent's verbose log on the user's terminal,
// escaped. A tunnel it cannot forward again costs only the tunnel.
func TestReconnectResumesTheKeptSession(t *testing.T) {
	h, _, stderr := sshHost(t)
	h.o.Verbose = true
	path, calls := fakeServe(t, "level=INFO msg=resumed \x1b[2Jdone\n", resumedReady("ab12"))
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ag, ready, err := h.reconnect(ctx, path, "ab12", time.Now().Add(time.Minute), &tunnel{ln: ln, log: h.log})
	if err != nil {
		t.Fatal(err)
	}
	defer ag.stop()
	if ready.ID != "ab12" || !ready.Resumed {
		t.Fatalf("ready %+v", ready)
	}
	if b, _ := os.ReadFile(calls); strings.TrimSpace(string(b)) != "serve -v -resume ab12" {
		t.Fatalf("the host ran %q", b)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(stderr.String(), `agent: level=INFO msg=resumed \x1b[2Jdone`) {
		if time.Now().After(deadline) {
			t.Fatalf("the agent's log on the terminal: %q", stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestReconnectGivesUpWhenTheSessionIsGone: an agent that has no such
// session ends the redialing at once; it would say so every time.
func TestReconnectGivesUpWhenTheSessionIsGone(t *testing.T) {
	h, _, _ := sshHost(t)
	path, calls := fakeServe(t, "", protocol.Message{Type: protocol.TypeError, Error: "no session ab12 on this host"})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ag, _, err := h.reconnect(ctx, path, "ab12", time.Now().Add(time.Minute), &tunnel{log: h.log})
	var final *finalError
	if ag != nil || !errors.As(err, &final) || !strings.Contains(err.Error(), "no session ab12") {
		t.Fatalf("reconnect to a gone session: %v, %v", ag, err)
	}
	if b, _ := os.ReadFile(calls); strings.Count(string(b), "serve") != 1 {
		t.Fatalf("the host ran %q, want one try", b)
	}
}

// TestReconnectStopsWhenTheKeepWindowCloses: a host that stays out of
// reach is redialed with backoff until the next try would come after the
// host let the desktop go.
func TestReconnectStopsWhenTheKeepWindowCloses(t *testing.T) {
	h, s, _ := sshHost(t)
	h.close()
	s.Stop()
	start := time.Now()
	ag, _, err := h.reconnect(context.Background(), "/nowhere", "ab12", start.Add(1500*time.Millisecond), &tunnel{log: h.log})
	if ag != nil || err == nil || !strings.Contains(err.Error(), "before the host let the desktop go") {
		t.Fatalf("reconnect to a host out of reach: %v, %v", ag, err)
	}
	if took := time.Since(start); took < reconnectFirst || took > 20*time.Second {
		t.Fatalf("gave up after %s, want one backoff of %s and a second try", took, reconnectFirst)
	}
}

// TestTunnelCarriesTheBrowsersTCPToTheHost: the tunnel's loopback port
// leads through the SSH forward to the agent's ICE-TCP listener, and
// stops doing so once the forward or the tunnel is gone.
func TestTunnelCarriesTheBrowsersTCPToTheHost(t *testing.T) {
	h, _, _ := sshHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	agentLn, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer agentLn.Close()
	go func() {
		for {
			c, err := agentLn.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	remote := agentLn.Addr().String()

	tun, err := openTunnel(ctx, h.m, remote, relay.Auto, h.log)
	if err != nil {
		t.Fatal(err)
	}
	if got := roundTrip(t, tun.Port()); got != "ice" {
		t.Fatalf("through the tunnel: %q", got)
	}

	direct, err := openTunnel(ctx, h.m, remote, relay.Direct, h.log)
	if err != nil || direct.Port() != 0 || direct.Close() != nil || direct.forward(ctx, h.m, remote) != nil {
		t.Fatalf("a direct session's tunnel: %+v, %v", direct, err)
	}

	// The master gone, the forward's socket goes with it: the tunnel
	// still accepts, and hangs up.
	h.m.Close()
	if got := roundTrip(t, tun.Port()); got != "" {
		t.Fatalf("through a tunnel whose forward is gone: %q", got)
	}
	if _, err := openTunnel(ctx, h.m, remote, relay.Tunnel, h.log); err == nil || !strings.Contains(err.Error(), "forward ICE-TCP over SSH") {
		t.Fatalf("a tunnel-only session without its forward: %v", err)
	}
	auto, err := openTunnel(ctx, h.m, remote, relay.Auto, h.log)
	if err != nil {
		t.Fatalf("an auto session without its forward: %v", err)
	}
	defer auto.Close()
	if got := roundTrip(t, auto.Port()); got != "" {
		t.Fatalf("through a tunnel that never had a forward: %q", got)
	}

	port := tun.Port()
	tun.Close()
	if c, err := net.Dial("tcp4", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
		c.Close()
		t.Fatal("the closed tunnel still accepts")
	}
}

// roundTrip sends "ice" through the tunnel at port and returns what came
// back before it hung up.
func roundTrip(t *testing.T, port int) string {
	t.Helper()
	c, err := net.Dial("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	c.Write([]byte("ice"))
	b := make([]byte, 3)
	n, _ := io.ReadFull(c, b)
	return string(b[:n])
}

// TestRunRefusesAnUnknownEncoderBeforeDeploying: the probe's answer is
// checked before anything lands on the host.
func TestRunRefusesAnUnknownEncoderBeforeDeploying(t *testing.T) {
	o, _, _ := localhost(t)
	o.Attach, o.Encoder = ":99", "h265"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := Run(ctx, o)
	if err == nil || !strings.Contains(err.Error(), o.Dest+`: unknown encoder "h265"`) {
		t.Fatalf("Run with an unknown encoder: %v", err)
	}
}

// TestProbeOverADeadConnection: the probe's failure says it was the probe.
func TestProbeOverADeadConnection(t *testing.T) {
	h, _, _ := sshHost(t)
	h.m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := runProbe(ctx, h.m); err == nil || !strings.HasPrefix(err.Error(), "probe host:") {
		t.Fatalf("runProbe over a closed master: %v", err)
	}
}

// TestDeployAgent: a host of an architecture this CLI carries no agent
// for is refused; one it does gets the agent, by its content's name, in
// the directory the probe chose.
func TestDeployAgent(t *testing.T) {
	h, _, _ := sshHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h.report = &probe.Report{Arch: "sparc64", AgentDir: t.TempDir()}
	if _, _, err := h.deployAgent(ctx); err == nil {
		t.Fatal("deployed an agent for sparc64")
	}
	a, err := agentbin.For(runtime.GOARCH)
	if err != nil {
		t.Skipf("no agent for %s in this build (make builds them): %v", runtime.GOARCH, err)
	}
	h.report = &probe.Report{Arch: runtime.GOARCH, AgentDir: t.TempDir(), SHA256Tool: "sha256sum"}
	got, path, err := h.deployAgent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name() != a.Name() || path != filepath.Join(h.report.AgentDir, a.Name()) {
		t.Fatalf("deployed %s to %s", got.Name(), path)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() != a.Size || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("the agent on the host: %v, %v", fi, err)
	}
	h.report.AgentDir = filepath.Join(path, "not-a-dir")
	if _, _, err := h.deployAgent(ctx); err == nil || !strings.Contains(err.Error(), "deploy agent") {
		t.Fatalf("deploy into a file: %v", err)
	}
}

func TestHostDefaults(t *testing.T) {
	o, log := Options{}.fill()
	if o.Stdout != os.Stdout || o.Stderr == nil || log != slog.Default() {
		t.Errorf("fill: %+v, %v", o, log)
	}
	if !keepUntil(&protocol.Ready{}).IsZero() {
		t.Error("keepUntil of a desktop that goes with its controller")
	}
	if until := keepUntil(&protocol.Ready{Keep: 60}); time.Until(until) < 59*time.Second {
		t.Errorf("keepUntil of a minute: %s", until)
	}
	if (&agentProc{}).explain(nil) != nil {
		t.Error("explain made an error of nothing")
	}
	// The page reads its ICE servers as a JSON array, never null.
	for stun, want := range map[string]string{"": `[]`, "stun:example.net:3478": `[{"urls":["stun:example.net:3478"]}]`} {
		var list []string
		if stun != "" {
			list = []string{stun}
		}
		if b, _ := json.Marshal(iceServers(list)); string(b) != want {
			t.Errorf("iceServers(%q) = %s, want %s", list, b, want)
		}
	}
	s, _ := startMessage(Options{Camera: true}, &probe.Report{V4L2Loopback: []probe.VideoDevice{{Path: "/dev/video3", Label: "Dummy"}, {Path: "/dev/video7", Label: "Robin camera"}}})
	if s.CameraDevice != "/dev/video7" {
		t.Errorf("the camera robin's loopback offers: %q", s.CameraDevice)
	}
}
