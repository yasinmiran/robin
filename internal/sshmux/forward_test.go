package sshmux

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/sshmux/sshtest"
)

func dialNoMux(t *testing.T, s *sshtest.Server, args ...string) *Master {
	t.Helper()
	m, err := Dial(t.Context(), s.Dest, Options{Args: append(args, s.Args...), Stderr: logWriter{t}, NoMux: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

// echoServer listens on loopback and sends back whatever it gets.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
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
	return ln.Addr().String()
}

// echoThrough sends random bytes through f and checks they come back.
func echoThrough(t *testing.T, f *Forward) {
	t.Helper()
	c, err := f.Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	data := make([]byte, 256<<10)
	rand.Read(data)
	go func() {
		c.Write(data)
		c.(interface{ CloseWrite() error }).CloseWrite()
	}()
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("echo through forward: sent %d bytes, got %d", len(data), len(got))
	}
}

func TestNoMuxArgs(t *testing.T) {
	m := &Master{ssh: "ssh", args: []string{"-F", "cfg", "-p", "2222"}, dest: "host"}
	own := []string{
		"-o", "ControlMaster=no", "-o", "ControlPath=none",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3",
		"-o", "RemoteCommand=none",
	}
	session := slices.Concat(own, []string{"-o", "ClearAllForwardings=yes", "-F", "cfg", "-p", "2222", "-T", "-a", "-x", "-e", "none"})

	if got, want := m.childArgs("echo hi"), slices.Concat(session, []string{"--", "host", "echo hi"}); !slices.Equal(got, want) {
		t.Errorf("childArgs:\n got %q\nwant %q", got, want)
	}
	if got, want := m.subsystemArgs("sftp"), slices.Concat(session, []string{"-s", "--", "host", "sftp"}); !slices.Equal(got, want) {
		t.Errorf("subsystemArgs:\n got %q\nwant %q", got, want)
	}
	want := slices.Concat([]string{"-o", "ExitOnForwardFailure=yes"}, own,
		[]string{"-F", "cfg", "-p", "2222", "-T", "-a", "-x", "-e", "none", "-N", "-n", "-L", "127.0.0.1:5000:127.0.0.1:22", "--", "host"})
	if got := m.forwardArgs("127.0.0.1:5000", "127.0.0.1:22"); !slices.Equal(got, want) {
		t.Errorf("forwardArgs:\n got %q\nwant %q", got, want)
	}
}

func TestNoMuxCommands(t *testing.T) {
	s := sshtest.Start(t)
	m := dialNoMux(t, s)
	ctx := t.Context()
	logins := strings.Count(s.Log(), "Accepted publickey")
	if logins != 0 {
		t.Errorf("Dial without a master logged in %d times", logins)
	}

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
	if n := strings.Count(s.Log(), "Accepted publickey"); n != 4 {
		t.Errorf("%d logins for four commands, want one each", n)
	}

	select {
	case <-m.Done():
		t.Fatal("Done closed before Close")
	default:
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	<-m.Done()
	if err := m.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestNoMuxSFTP(t *testing.T) {
	m := dialNoMux(t, sshtest.Start(t))
	c, err := m.SFTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 1<<20)
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
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("uploaded file differs (%d bytes, %v)", len(got), err)
	}
	waitNoProcesses(t, "ControlPath=none", "\x00sftp\x00")
}

func TestNoMuxForward(t *testing.T) {
	for _, tcp := range []bool{false, true} {
		t.Run(map[bool]string{false: "unix", true: "tcp"}[tcp], func(t *testing.T) {
			m := dialNoMux(t, sshtest.Start(t))
			m.tcpForwards = tcp
			echo := echoServer(t)

			f, err := m.Forward(t.Context(), echo)
			if err != nil {
				t.Fatal(err)
			}
			if tcp && (f.network != "tcp" || !strings.HasPrefix(f.addr, "127.0.0.1:")) {
				t.Errorf("forward listens on %s %s, want loopback TCP", f.network, f.addr)
			}
			if !tcp {
				if f.network != "unix" || filepath.Dir(f.addr) != m.dir {
					t.Errorf("forward at %s %s, want a socket in %s", f.network, f.addr, m.dir)
				}
				if fi, err := os.Stat(m.dir); err != nil || fi.Mode().Perm() != 0o700 {
					t.Errorf("socket directory: %v, mode %v; want 0700", err, fi.Mode().Perm())
				}
			}
			echoThrough(t, f)
			echoThrough(t, f)
			g, err := m.Forward(t.Context(), echo)
			if err != nil {
				t.Fatal(err)
			}
			if g.addr == f.addr {
				t.Errorf("two forwards share %s", f.addr)
			}
			echoThrough(t, g)

			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			waitNoProcesses(t, f.addr+":"+echo)
			waitNoProcesses(t, g.addr+":"+echo)
			if c, err := f.Dial(); err == nil {
				c.Close()
				t.Error("forward still accepts after Close")
			}
			if _, err := m.Forward(t.Context(), echo); err == nil {
				t.Error("Forward after Close succeeded")
			}
			if _, err := os.Stat(m.dir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("socket directory left behind: %v", err)
			}
		})
	}
}

// squatter listens on a loopback port as another program on this machine
// could, counting the connections it gets.
type squatter struct {
	net.Listener
	accepted atomic.Int32
}

func squat(t *testing.T, addr string) *squatter {
	t.Helper()
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	sq := &squatter{Listener: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			sq.accepted.Add(1)
			c.Close()
		}
	}()
	return sq
}

// TestNoMuxForwardTaken has another program take the forward's port while
// ssh logs in, and checks no connection goes to it.
func TestNoMuxForwardTaken(t *testing.T) {
	m := dialNoMux(t, sshtest.Start(t))
	m.tcpForwards = true
	sq := squat(t, "127.0.0.1:0")
	m.freeAddr = func() (string, error) { return sq.Addr().String(), nil }
	f, err := m.Forward(t.Context(), echoServer(t))
	t.Logf("Forward onto a taken port: %v", err)
	if err == nil {
		if c, err := f.Dial(); err == nil {
			c.Close()
		}
		t.Error("Forward onto a port another program holds succeeded")
	}
	if n := sq.accepted.Load(); n != 0 {
		t.Errorf("the program holding the port got %d connections", n)
	}
	m.Close()
	waitNoProcesses(t, sq.Addr().String())
}

// TestNoMuxForwardBlindTable checks a forward fails, rather than wait for
// ever, where the table of listeners never shows ssh on its port.
func TestNoMuxForwardBlindTable(t *testing.T) {
	defer func(f func(string, int) (bool, bool, error)) { lookupListener = f }(lookupListener)
	lookupListener = func(string, int) (bool, bool, error) { return false, false, nil }
	m := dialNoMux(t, sshtest.Start(t))
	m.tcpForwards = true
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := m.Forward(ctx, echoServer(t))
	t.Logf("Forward with a blind listener table: %v", err)
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Forward = %v, want it to fail on what the table does not show", err)
	}
}

// TestNoMuxForwardDead kills a forward's ssh, and checks the forward then
// refuses to dial, even when another program has taken its port.
func TestNoMuxForwardDead(t *testing.T) {
	for _, tcp := range []bool{false, true} {
		t.Run(map[bool]string{false: "unix", true: "tcp"}[tcp], func(t *testing.T) {
			m := dialNoMux(t, sshtest.Start(t))
			m.tcpForwards = tcp
			f, err := m.Forward(t.Context(), echoServer(t))
			if err != nil {
				t.Fatal(err)
			}
			echoThrough(t, f)
			f.proc.cmd.Process.Kill()
			<-f.proc.exited
			var sq *squatter
			if tcp {
				sq = squat(t, f.addr)
			}
			if c, err := f.Dial(); err == nil {
				c.Close()
				t.Error("a forward whose ssh has exited still dials")
			} else {
				t.Logf("Dial after its ssh exited: %v", err)
			}
			if sq != nil && sq.accepted.Load() != 0 {
				t.Error("the program now on the forward's port got a connection")
			}
		})
	}
}

func TestNoMuxForwardFails(t *testing.T) {
	s := sshtest.Start(t)
	m := dialNoMux(t, s)
	s.Stop()
	_, err := m.Forward(t.Context(), "127.0.0.1:1")
	t.Logf("Forward to a stopped server: %v", err)
	if err == nil || !strings.Contains(err.Error(), "ssh failed") {
		t.Fatalf("Forward to a stopped server: %v", err)
	}
	if _, err := m.Forward(t.Context(), ""); err == nil {
		t.Error("empty forward target accepted")
	}
}

func TestNoMuxForwardCancelled(t *testing.T) {
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
	m, err := Dial(t.Context(), "127.0.0.1", Options{NoMux: true, Stderr: io.Discard, Args: []string{
		"-F", os.DevNull, "-o", "BatchMode=yes", "-o", "UserKnownHostsFile=/dev/null", "-p", strconv.Itoa(port),
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if _, err := m.Forward(ctx, "127.0.0.1:1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Forward = %v, want deadline exceeded", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	waitNoProcesses(t, "ExitOnForwardFailure=yes", "-p\x00"+strconv.Itoa(port))
}

// TestNoMuxConfigForwards checks that commands leave the forwards in the
// user's config alone, while a forward's own ssh, which cannot clear them
// without clearing its own, still works with them there.
func TestNoMuxConfigForwards(t *testing.T) {
	s := sshtest.Start(t)
	echo := echoServer(t)
	taken, err := freeLoopbackAddr()
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(s.Dir, "ssh_config_forwards")
	orig, err := os.ReadFile(s.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	extra := "Host " + s.Dest + "\n\tLocalForward " + taken + " " + echo + "\n"
	if err := os.WriteFile(cfg, append([]byte(extra), orig...), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Dial(t.Context(), s.Dest, Options{Args: []string{"-F", cfg}, Stderr: logWriter{t}, NoMux: true})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	cmd := m.Command(t.Context(), "echo up; sleep 30")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	if _, err := stdout.Read(make([]byte, 3)); err != nil {
		t.Fatal(err)
	}
	if c, err := net.Dial("tcp", taken); err == nil {
		c.Close()
		t.Error("a command set up the config's LocalForward")
	}

	f, err := m.Forward(t.Context(), echo)
	if err != nil {
		t.Fatal(err)
	}
	echoThrough(t, f)
}

// TestNoMuxStartsNoMaster checks that a ControlMaster in the user's config
// does not turn a command into a master that outlives the session.
func TestNoMuxStartsNoMaster(t *testing.T) {
	s := sshtest.Start(t)
	ctlDir, err := os.MkdirTemp("/tmp", "robin-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(ctlDir)
	cfg := filepath.Join(s.Dir, "ssh_config_master")
	orig, err := os.ReadFile(s.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	extra := "Host " + s.Dest + "\n\tControlMaster auto\n\tControlPath " + ctlDir + "/%C\n\tControlPersist yes\n"
	if err := os.WriteFile(cfg, append([]byte(extra), orig...), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Dial(t.Context(), s.Dest, Options{Args: []string{"-F", cfg}, Stderr: logWriter{t}, NoMux: true})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := m.Output(t.Context(), "echo ok", nil); err != nil || string(out) != "ok\n" {
		t.Fatalf("Output = %q, %v", out, err)
	}
	if _, err := m.Forward(t.Context(), echoServer(t)); err != nil {
		t.Fatal(err)
	}
	m.Close()
	if left, _ := os.ReadDir(ctlDir); len(left) > 0 {
		t.Errorf("control sockets left behind: %v", left)
	}
	waitNoProcesses(t, ctlDir)
}

func TestForwardOverMaster(t *testing.T) {
	m := dial(t)
	echo := echoServer(t)
	f, err := m.Forward(t.Context(), echo)
	if err != nil {
		t.Fatal(err)
	}
	if f.network != "unix" || filepath.Dir(f.addr) != m.dir {
		t.Errorf("forward at %s %s, want a socket in %s", f.network, f.addr, m.dir)
	}
	echoThrough(t, f)
	g, err := m.Forward(t.Context(), echo)
	if err != nil {
		t.Fatal(err)
	}
	if g.addr == f.addr {
		t.Errorf("two forwards share %s", f.addr)
	}
	echoThrough(t, g)
	if _, err := m.Forward(t.Context(), ""); err == nil {
		t.Error("empty forward target accepted")
	}
}

// TestForwardRefusesAMalformedTarget: the target comes from the host, and
// anything but host:port or an absolute socket path would be split by ssh
// into other fields of the -L spec. It is refused before ssh runs, with a
// master or without.
func TestForwardRefusesAMalformedTarget(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	tripwire := filepath.Join(dir, "ssh")
	if err := os.WriteFile(tripwire, []byte("#!/bin/sh\ntouch "+ran+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	masters := map[string]*Master{
		"master":    {ssh: tripwire, dest: "host", dir: dir, ctl: filepath.Join(dir, "ctl"), mux: true},
		"no master": {ssh: tripwire, dest: "host", dir: dir, freeAddr: freeLoopbackAddr, done: make(chan struct{})},
	}
	for name, m := range masters {
		for _, remote := range []string{
			"", "a:b:c", "/run/x:1", "[::1]:22:3", "host:0", "127.0.0.1:70000", "127.0.0.1:022",
			"[127.0.0.1]:1", "127.0.0.1:", ":22", "relative/x.sock", `/run/back\slash`, "[fe80::1%eth0]:22",
			"-oProxyCommand=x:22", "host name:22", `h\ost:22`, "127.0.0.1:22 ",
		} {
			_, err := m.Forward(t.Context(), remote)
			if err == nil || !strings.Contains(err.Error(), "malformed forward target") {
				t.Errorf("%s: Forward(%q) = %v, want it refused as malformed", name, remote, err)
			}
		}
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("ssh ran for a malformed target")
	}
}

func TestCheckTargetTakesWhatTheAgentSends(t *testing.T) {
	for _, remote := range []string{"127.0.0.1:41234", "[::1]:22", "localhost:22", "lab-node.example:65535", "/run/user/1000/robin/ice.sock"} {
		if err := checkTarget(remote); err != nil {
			t.Errorf("checkTarget(%q): %v", remote, err)
		}
	}
}

func TestForwardLocalNeedsMaster(t *testing.T) {
	m := &Master{dest: "host"}
	if err := m.forwardLocal(t.Context(), "/tmp/x.sock", "127.0.0.1:1"); err == nil {
		t.Error("a unix socket forward without a master succeeded")
	}
}
