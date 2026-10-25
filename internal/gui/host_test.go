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
	"syscall"
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

// A redial runs the agent's serve with -resume over a fresh connection and
// goes on with the session it answers for, with the agent's verbose log on
// the user's terminal, escaped. A tunnel it cannot forward again costs
// only the tunnel.
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

// An agent that has no such session ends the redialing at once; it would
// say so every time.
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

// A host that stays out of reach is redialed with backoff until the next
// try would come after the host let the desktop go.
func TestReconnectStopsWhenTheKeepWindowCloses(t *testing.T) {
	h, s, _ := sshHost(t)
	var logged lockedBuffer
	h.log = slog.New(slog.NewTextHandler(&logged, nil))
	h.close()
	s.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Room for a second try after the first backoff, and none for the
	// backoff after that, however long each try takes to fail.
	until := time.Now().Add(reconnectFirst + 1500*time.Millisecond)
	ag, _, err := h.reconnect(ctx, "/nowhere", "ab12", until, &tunnel{log: h.log})
	if ag != nil || err == nil || !strings.Contains(err.Error(), "before the host let the desktop go") {
		t.Fatalf("reconnect to a host out of reach: %v, %v", ag, err)
	}
	if n := strings.Count(logged.String(), "reconnect failed"); n != 2 {
		t.Fatalf("%d tries, want a second after a backoff of %s and no third:\n%s", n, reconnectFirst, logged.String())
	}
}

// The tunnel's loopback port leads through the SSH forward to the agent's
// ICE-TCP listener, and stops doing so once the forward or the tunnel is
// gone.
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
	if echoed, err := roundTrip(t, tun); !echoed || err != nil {
		t.Fatalf("through the tunnel: echoed %v, %v", echoed, err)
	}

	direct, err := openTunnel(ctx, h.m, remote, relay.Direct, h.log)
	if err != nil || direct.Port() != 0 || direct.Close() != nil || direct.forward(ctx, h.m, remote) != nil {
		t.Fatalf("a direct session's tunnel: %+v, %v", direct, err)
	}

	// The master gone, the forward's socket goes with it: the tunnel
	// still accepts, and hangs up.
	h.m.Close()
	if echoed, err := roundTrip(t, tun); echoed || !hungUp(err) {
		t.Fatalf("through a tunnel whose forward is gone: echoed %v, %v, want a hang-up", echoed, err)
	}
	if _, err := openTunnel(ctx, h.m, remote, relay.Tunnel, h.log); err == nil || !strings.Contains(err.Error(), "forward ICE-TCP over SSH") {
		t.Fatalf("a tunnel-only session without its forward: %v", err)
	}
	auto, err := openTunnel(ctx, h.m, remote, relay.Auto, h.log)
	if err != nil {
		t.Fatalf("an auto session without its forward: %v", err)
	}
	defer auto.Close()
	if echoed, err := roundTrip(t, auto); echoed || !hungUp(err) {
		t.Fatalf("through a tunnel that never had a forward: echoed %v, %v, want a hang-up", echoed, err)
	}

	port := tun.Port()
	tun.Close()
	if c, err := net.Dial("tcp4", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
		c.Close()
		t.Fatal("the closed tunnel still accepts")
	}
}

// tunnelUfrag and tunnelPwd stand in for the ICE credentials of the
// agent's answer.
const tunnelUfrag, tunnelPwd = "agentufrag", "agentpasswordagentpasswo"

// roundTrip tells tun the agent's answer, sends a check signed with it
// through tun, and reports whether it came back whole, with the error that
// ended the read: none once all of it came, EOF or a reset when the tunnel
// hung up, a timeout when it held on.
func roundTrip(t *testing.T, tun *tunnel) (bool, error) {
	t.Helper()
	tun.answered(iceAnswer(tunnelUfrag, tunnelPwd))
	check := framedCheck(t, tunnelUfrag+":brws", tunnelPwd)
	c, err := net.Dial("tcp4", fmt.Sprintf("127.0.0.1:%d", tun.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	c.Write(check)
	b := make([]byte, len(check))
	n, err := io.ReadFull(c, b)
	return bytes.Equal(b[:n], check), err
}

// hungUp reports whether a read's err says the other end closed the
// connection, at once or with what was sent to it still unread.
func hungUp(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET)
}

// The probe's answer is checked before anything lands on the host: Run
// with an encoder no host has deploys no agent into the directory the
// probe chose, here one of the test's own.
func TestRunRefusesAnUnknownEncoderBeforeDeploying(t *testing.T) {
	cache := t.TempDir()
	o, _, _ := localhost(t, "XDG_CACHE_HOME="+cache)
	o.Attach, o.Encoder = ":99", "h265"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := Run(ctx, o)
	if err == nil || !strings.Contains(err.Error(), o.Dest+`: unknown encoder "h265"`) {
		t.Fatalf("Run with an unknown encoder: %v", err)
	}
	agentDir := filepath.Join(cache, "robin")
	entries, err := os.ReadDir(agentDir)
	if err != nil {
		t.Fatalf("the probe did not choose %s: %v", agentDir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), agentbin.NamePrefix) {
			t.Fatalf("Run deployed %s before refusing the encoder", e.Name())
		}
	}
}

// The probe's failure says it was the probe.
func TestProbeOverADeadConnection(t *testing.T) {
	h, _, _ := sshHost(t)
	h.m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := runProbe(ctx, h.m); err == nil || !strings.HasPrefix(err.Error(), "probe host:") {
		t.Fatalf("runProbe over a closed master: %v", err)
	}
}

// The agent lands by its content's name in the directory the probe chose,
// and a deploy into a file fails as the deploy's.
func TestDeployAgent(t *testing.T) {
	h, _, _ := sshHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a, err := agentbin.For(runtime.GOARCH)
	if err != nil {
		t.Skipf("no agent for %s in this build (make builds them): %v", runtime.GOARCH, err)
	}
	h.report = &probe.Report{Arch: runtime.GOARCH, AgentDir: t.TempDir(), SHA256Tool: "sha256sum"}
	path, err := h.deployAgent(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(h.report.AgentDir, a.Name()) {
		t.Fatalf("deployed %s to %s", a.Name(), path)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() != a.Size || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("the agent on the host: %v, %v", fi, err)
	}
	h.report.AgentDir = filepath.Join(path, "not-a-dir")
	if _, err := h.deployAgent(ctx, a); err == nil || !strings.Contains(err.Error(), "deploy agent") {
		t.Fatalf("deploy into a file: %v", err)
	}
}

// Options left empty write to the process's own stdout and stderr and log
// to slog's default.
func TestFillDefaults(t *testing.T) {
	o, log := Options{}.fill()
	if o.Stdout != os.Stdout || o.Stderr == nil || log != slog.Default() {
		t.Errorf("fill: %+v, %v", o, log)
	}
}

// A desktop that goes with its controller has no keep window; a kept one
// lasts as long as the host said from now.
func TestKeepUntil(t *testing.T) {
	if !keepUntil(&protocol.Ready{}).IsZero() {
		t.Error("keepUntil of a desktop that goes with its controller")
	}
	if until := keepUntil(&protocol.Ready{Keep: 60}); time.Until(until) < 59*time.Second {
		t.Errorf("keepUntil of a minute: %s", until)
	}
}

// No error stays no error when the agent's stderr is added to it.
func TestExplainNothing(t *testing.T) {
	if (&agentProc{}).explain(nil) != nil {
		t.Error("explain made an error of nothing")
	}
}

// The page reads its ICE servers as a JSON array, never null.
func TestICEServersAreAnArray(t *testing.T) {
	for _, tc := range []struct {
		name string
		stun []string
		want string
	}{
		{"none", nil, `[]`},
		{"one", []string{"stun:example.net:3478"}, `[{"urls":["stun:example.net:3478"]}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if b, _ := json.Marshal(iceServers(tc.stun)); string(b) != tc.want {
				t.Errorf("iceServers(%q) = %s, want %s", tc.stun, b, tc.want)
			}
		})
	}
}
