package sshmux

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/sshmux/sshtest"
	"github.com/yasinmiran/robin/internal/tail"
)

func dial(t *testing.T) *Master {
	t.Helper()
	s := sshtest.Start(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	m, err := Dial(ctx, s.Dest, Options{Args: s.Args, Stderr: logWriter{t}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	return m
}

type logWriter struct{ t testing.TB }

func (w logWriter) Write(p []byte) (int, error) {
	w.t.Logf("ssh stderr: %s", bytes.TrimSpace(p))
	return len(p), nil
}

func TestOutput(t *testing.T) {
	m := dial(t)
	ctx := t.Context()

	out, err := m.Output(ctx, "echo hello; echo noise >&2", nil)
	if err != nil || string(out) != "hello\n" {
		t.Fatalf("Output = %q, %v", out, err)
	}

	out, err = m.Output(ctx, "cat", strings.NewReader("from stdin"))
	if err != nil || string(out) != "from stdin" {
		t.Fatalf("Output with stdin = %q, %v", out, err)
	}

	_, err = m.Output(ctx, "echo oops >&2; exit 3", nil)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 || !strings.Contains(err.Error(), "oops") {
		t.Fatalf("failing command: %v", err)
	}

	out, err = m.Output(ctx, printArgs(hostile...), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := splitArgs(out); !slices.Equal(got, hostile) {
		t.Fatalf("remote round trip mismatch:\n got %q\nwant %q", got, hostile)
	}
}

func TestCommandStreamsJSONLines(t *testing.T) {
	m := dial(t)
	cmd := m.Command(t.Context(), "cat")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	type msg struct {
		Type string `json:"type"`
		SDP  string `json:"sdp"`
	}
	enc, dec := json.NewEncoder(stdin), json.NewDecoder(bufio.NewReader(stdout))
	// One message at a time: each reply must arrive before the next send,
	// which fails if anything buffers the stream until EOF.
	for i, sdp := range []string{"v=0\r\no=- 1 2 IN IP4 127.0.0.1\r\n", "~.\n~?", "\x00\x01 binary-ish  "} {
		want := msg{Type: "offer", SDP: sdp}
		if err := enc.Encode(want); err != nil {
			t.Fatal(err)
		}
		var got msg
		if err := dec.Decode(&got); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if got != want {
			t.Fatalf("message %d: got %+v, want %+v", i, got, want)
		}
	}
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestCommandIsBinarySafe(t *testing.T) {
	m := dial(t)
	payload := make([]byte, 0, 1<<16)
	for len(payload) < cap(payload) {
		for b := range 256 {
			payload = append(payload, byte(b))
		}
		payload = append(payload, "\r\n~.\n~C\n"...)
	}
	out, err := m.Output(t.Context(), "cat", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, payload) {
		t.Fatalf("payload changed in transit: %d bytes in, %d out", len(payload), len(out))
	}
}

func TestCommandCancel(t *testing.T) {
	m := dial(t)
	ctx, cancel := context.WithCancel(t.Context())
	cmd := m.Command(ctx, "sleep 60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled command still running")
	}
}

func TestSFTP(t *testing.T) {
	m := dial(t)
	c, err := m.SFTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 3<<20)
	rand.Read(data)
	path := filepath.Join(t.TempDir(), "upload.bin")
	f, err := c.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadFrom(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.HasExtension("posix-rename@openssh.com"); !ok {
		t.Error("server lacks posix-rename")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("uploaded file differs (%d bytes, %v)", len(got), err)
	}
	waitNoProcesses(t, m.ctl, "\x00sftp\x00")
}

func TestForwardLocal(t *testing.T) {
	m := dial(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()

	sock := m.SocketPath("fwd")
	if err := m.ForwardLocal(t.Context(), sock, ln.Addr().String()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("forward socket mode %v is reachable by other users", fi.Mode().Perm())
	}

	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	data := make([]byte, 256<<10)
	rand.Read(data)
	go func() {
		c.Write(data)
		c.(*net.UnixConn).CloseWrite()
	}()
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("echo through forward: sent %d bytes, got %d", len(data), len(got))
	}

	// ssh treats a repeat of the same forward as a no-op.
	if err := m.ForwardLocal(t.Context(), sock, ln.Addr().String()); err != nil {
		t.Errorf("repeating a forward: %v", err)
	}
	err = m.ForwardLocal(t.Context(), sock, "127.0.0.1:1")
	t.Logf("forwarding a taken path elsewhere: %v", err)
	if err == nil {
		t.Error("second forward on the same path succeeded")
	}
	stale := m.SocketPath("stale")
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err = m.ForwardLocal(t.Context(), stale, ln.Addr().String())
	t.Logf("forwarding onto an existing file: %v", err)
	if err == nil {
		t.Error("forward replaced an existing file")
	}
	if err := m.ForwardLocal(t.Context(), "relative.sock", "127.0.0.1:1"); err == nil {
		t.Error("relative forward path accepted")
	}
}

func TestClose(t *testing.T) {
	m := dial(t)
	cat := m.Command(context.Background(), "cat")
	stdin, err := cat.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.Start(); err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()

	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.Done():
	default:
		t.Fatal("Done open after Close")
	}
	if _, err := os.Stat(m.dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("control dir still there: %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cat.Wait() }()
	select {
	case err := <-waited:
		t.Logf("session after Close: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("session still running after Close")
	}
	waitNoProcesses(t, m.dir)
	if err := m.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestDoneOnMasterExit(t *testing.T) {
	m := dial(t)
	if err := m.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done not closed after the master died")
	}

	// Without the master, sessions must fail rather than reconnect on their own.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := m.Output(ctx, "echo reconnected", nil)
	t.Logf("Output after master death: %v", err)
	if err == nil || ctx.Err() != nil {
		t.Fatalf("Output after master death: %v (ctx %v)", err, ctx.Err())
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("control dir still there: %v", err)
	}
}

// TestInterruptLeavesTheMasterRunning: Ctrl+C reaches the whole process
// group; a session over the master dies of it, the master does not, so the
// CLI can still run a command before closing it.
func TestInterruptLeavesTheMasterRunning(t *testing.T) {
	m := dial(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := m.Command(ctx, "cat")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	time.Sleep(200 * time.Millisecond)
	if err := m.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("the session survived SIGINT; the CLI assumes it does not")
	}
	if m.Dead() {
		t.Fatal("the master died of SIGINT")
	}
	if got, err := m.Output(ctx, "echo alive", nil); err != nil || string(got) != "alive\n" {
		t.Fatalf("Output after SIGINT: %q, %v", got, err)
	}
	if err := m.Close(); err != nil || !m.Dead() {
		t.Fatalf("Close: %v, dead %v", err, m.Dead())
	}
}

func TestDialUnknownHostKey(t *testing.T) {
	s := sshtest.Start(t)
	var stderr tail.Buffer
	args := append([]string{"-o", "UserKnownHostsFile=/dev/null"}, s.Args...)
	m, err := Dial(t.Context(), s.Dest, Options{Args: args, Stderr: &stderr})
	if err == nil {
		m.Close()
		t.Fatal("Dial trusted an unknown host key")
	}
	t.Logf("Dial: %v", err)
	if !strings.Contains(err.Error(), "Host key verification failed") {
		t.Errorf("error lacks the ssh reason: %v", err)
	}
	if !strings.Contains(stderr.String(), "Host key verification failed") {
		t.Errorf("Options.Stderr did not get the master's stderr: %q", stderr.String())
	}
}

func TestDialContextDeadline(t *testing.T) {
	// A server that accepts and never speaks keeps ssh waiting for a banner.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	args := []string{"-F", os.DevNull, "-o", "BatchMode=yes", "-o", "UserKnownHostsFile=/dev/null", "-p", strconv.Itoa(port)}
	_, err = Dial(ctx, "127.0.0.1", Options{Args: args, Stderr: io.Discard})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Dial = %v, want deadline exceeded", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Dial took %v after its deadline", d)
	}
}

// waitNoProcesses fails when, after a grace period, an ssh process still has
// every needle in its command line.
func waitNoProcesses(t *testing.T, needles ...string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		return
	}
	var left []string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		left = sshProcesses(needles)
		if len(left) == 0 {
			return
		}
	}
	t.Errorf("processes still running: %q", left)
}

func sshProcesses(needles []string) []string {
	entries, _ := os.ReadDir("/proc")
	var found []string
	for _, e := range entries {
		if e.Name()[0] < '0' || e.Name()[0] > '9' {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil || !bytes.HasPrefix(b, []byte("ssh\x00")) && !bytes.Contains(b, []byte("/ssh\x00")) {
			continue
		}
		if !slices.ContainsFunc(needles, func(n string) bool { return !bytes.Contains(b, []byte(n)) }) {
			found = append(found, strings.ReplaceAll(string(b), "\x00", " "))
		}
	}
	return found
}

// TestJumpHost reaches the server through itself as a jump host, set both in
// the config and with -J, and checks sessions work and still refuse to
// reconnect on their own once the master is gone.
func TestJumpHost(t *testing.T) {
	s := sshtest.Start(t)
	orig, err := os.ReadFile(s.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	both := strings.Replace(string(orig), "Host "+s.Dest+"\n", "Host "+s.Dest+" jump\n", 1)
	viaConfig := filepath.Join(s.Dir, "ssh_config_jump")
	if err := os.WriteFile(viaConfig, []byte("Host "+s.Dest+"\n\tProxyJump jump\n"+both), 0o600); err != nil {
		t.Fatal(err)
	}
	viaFlag := filepath.Join(s.Dir, "ssh_config_both")
	if err := os.WriteFile(viaFlag, []byte(both), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, args := range map[string][]string{
		"ProxyJump": {"-F", viaConfig},
		"-J":        {"-F", viaFlag, "-J", "jump"},
	} {
		t.Run(name, func(t *testing.T) {
			logins := func() int { return strings.Count(s.Log(), "Accepted publickey") }
			before := logins()
			m, err := Dial(t.Context(), s.Dest, Options{Args: args, Stderr: logWriter{t}})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if n := logins() - before; n != 2 {
				t.Errorf("%d logins for one Dial, want 2 (jump and target)", n)
			}
			if out, err := m.Output(t.Context(), "echo ok", nil); err != nil || string(out) != "ok\n" {
				t.Fatalf("Output = %q, %v", out, err)
			}
			m.cmd.Process.Kill()
			<-m.Done()
			if _, err := m.Output(t.Context(), "echo reconnected", nil); err == nil {
				t.Fatal("session reconnected without the master")
			}
		})
	}
}
