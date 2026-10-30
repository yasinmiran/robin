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
		[]string{"-F", "cfg", "-p", "2222", "-T", "-a", "-x", "-e", "none", "-N", "-n", "-L", "/run/fwd1.sock:127.0.0.1:22", "--", "host"})
	if got := m.forwardArgs("/run/fwd1.sock", "127.0.0.1:22"); !slices.Equal(got, want) {
		t.Errorf("forwardArgs:\n got %q\nwant %q", got, want)
	}
	if got, want := m.stdioArgs("127.0.0.1:22"), slices.Concat(session, []string{"-W", "127.0.0.1:22", "--", "host"}); !slices.Equal(got, want) {
		t.Errorf("stdioArgs:\n got %q\nwant %q", got, want)
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

// TestNoMuxForward runs a forward without a master both ways: an ssh -L on
// a unix socket in the private directory, and on Windows, whose OpenSSH
// forwards no unix sockets, an ssh -W per connection. Neither listens on
// a TCP port, which any user of this machine could connect to and so get
// past the tunnel's check.
func TestNoMuxForward(t *testing.T) {
	for _, stdio := range []bool{false, true} {
		t.Run(map[bool]string{false: "unix", true: "stdio"}[stdio], func(t *testing.T) {
			m := dialNoMux(t, sshtest.Start(t))
			m.stdioForwards = stdio
			echo := echoServer(t)

			f, err := m.Forward(t.Context(), echo)
			if err != nil {
				t.Fatal(err)
			}
			if !stdio {
				if f.network != "unix" || filepath.Dir(f.addr) != m.dir {
					t.Errorf("forward at %s %s, want a socket in %s", f.network, f.addr, m.dir)
				}
				if fi, err := os.Stat(m.dir); err != nil || fi.Mode().Perm() != 0o700 {
					t.Errorf("socket directory: %v, mode %v; want 0700", err, fi.Mode().Perm())
				}
			}
			noTCPListener(t, echo)
			held, err := f.Dial()
			if err != nil {
				t.Fatal(err)
			}
			defer held.Close()
			if _, err := held.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(held, make([]byte, 1)); err != nil {
				t.Fatal(err)
			}
			noTCPListener(t, echo)
			echoThrough(t, f)
			echoThrough(t, f)
			g, err := m.Forward(t.Context(), echo)
			if err != nil {
				t.Fatal(err)
			}
			if !stdio && g.addr == f.addr {
				t.Errorf("two forwards share %s", f.addr)
			}
			echoThrough(t, g)

			// Close ends the connection still open, and its ssh.
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			waitNoProcesses(t, "ControlPath=none", echo)
			held.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := held.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
				t.Errorf("a connection open at Close: read %v, want it ended", err)
			}
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

// TestNoMuxStdioForwardClosesItsSSH: each connection through a forward of
// ssh -W connections is an ssh of its own, which closing it stops.
func TestNoMuxStdioForwardClosesItsSSH(t *testing.T) {
	m := dialNoMux(t, sshtest.Start(t))
	m.stdioForwards = true
	echo := echoServer(t)
	f, err := m.Forward(t.Context(), echo)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		echoThrough(t, f)
	}
	waitNoProcesses(t, "ControlPath=none", echo)
	m.mu.Lock()
	left := len(m.forwards)
	m.mu.Unlock()
	if left != 0 {
		t.Errorf("%d ssh processes still tracked after their connections closed", left)
	}
}

// noTCPListener fails when an ssh forwarding to target listens on a TCP
// port.
func noTCPListener(t *testing.T, target string) {
	t.Helper()
	listening := map[string]string{} // socket link -> local address
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(table)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n")[1:] {
			if f := strings.Fields(line); len(f) > 9 && f[3] == "0A" {
				listening["socket:["+f[9]+"]"] = f[1]
			}
		}
	}
	procs, _ := os.ReadDir("/proc")
	for _, p := range procs {
		cmdline, err := os.ReadFile(filepath.Join("/proc", p.Name(), "cmdline"))
		if err != nil || !bytes.Contains(cmdline, []byte("ControlPath=none")) || !bytes.Contains(cmdline, []byte(target)) {
			continue
		}
		fds := filepath.Join("/proc", p.Name(), "fd")
		entries, _ := os.ReadDir(fds)
		for _, fd := range entries {
			if link, err := os.Readlink(filepath.Join(fds, fd.Name())); err == nil && listening[link] != "" {
				t.Errorf("ssh %s, forwarding to %s, listens on TCP %s", p.Name(), target, listening[link])
			}
		}
	}
}

// TestNoMuxForwardDead kills a forward's ssh, and checks the forward then
// refuses to dial.
func TestNoMuxForwardDead(t *testing.T) {
	m := dialNoMux(t, sshtest.Start(t))
	f, err := m.Forward(t.Context(), echoServer(t))
	if err != nil {
		t.Fatal(err)
	}
	echoThrough(t, f)
	f.proc.cmd.Process.Kill()
	<-f.proc.exited
	if c, err := f.Dial(); err == nil {
		c.Close()
		t.Error("a forward whose ssh has exited still dials")
	} else {
		t.Logf("Dial after its ssh exited: %v", err)
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

	// An ssh -W logs in only once dialled, and its connection ends when
	// it cannot.
	m.stdioForwards = true
	f, err := m.Forward(t.Context(), "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("a connection whose ssh cannot log in: read %v, want it ended", err)
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

// TestNoMuxConfigForwards checks that commands and ssh -W connections
// leave the forwards in the user's config alone, while a forward's own ssh
// -L, which cannot clear them without clearing its own, still works with
// them there.
func TestNoMuxConfigForwards(t *testing.T) {
	s := sshtest.Start(t)
	echo := echoServer(t)
	free, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	taken := free.Addr().String()
	free.Close()
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

	m.stdioForwards = true
	w, err := m.Forward(t.Context(), echo)
	if err != nil {
		t.Fatal(err)
	}
	c, err := w.Dial()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(c, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if l, err := net.Dial("tcp", taken); err == nil {
		l.Close()
		t.Error("an ssh -W set up the config's LocalForward")
	}
	c.Close()

	m.stdioForwards = false
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

// tripwireMasters are a master and master-less connections, forwarding
// through unix sockets or ssh -W, whose ssh only records that it ran, by
// creating the returned path.
func tripwireMasters(t *testing.T) (map[string]*Master, string) {
	t.Helper()
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	tripwire := filepath.Join(dir, "ssh")
	if err := os.WriteFile(tripwire, []byte("#!/bin/sh\ntouch "+ran+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return map[string]*Master{
		"master":    {ssh: tripwire, dest: "host", dir: dir, ctl: filepath.Join(dir, "ctl"), mux: true},
		"no master": {ssh: tripwire, dest: "host", dir: dir, done: make(chan struct{})},
		"ssh -W":    {ssh: tripwire, dest: "host", stdioForwards: true, done: make(chan struct{})},
	}, ran
}

// refusedBeforeSSH checks every target is refused as malformed, with a
// master and without, and that ssh never ran for any of them.
func refusedBeforeSSH(t *testing.T, targets ...string) {
	t.Helper()
	masters, ran := tripwireMasters(t)
	for name, m := range masters {
		for _, remote := range targets {
			_, err := m.Forward(t.Context(), remote)
			if err == nil || !strings.Contains(err.Error(), "malformed forward target") {
				t.Errorf("%s: Forward(%q) = %v, want it refused as malformed", name, remote, err)
			}
		}
	}
	if _, err := os.Stat(ran); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ssh ran for a refused target: %v", err)
	}
}

// TestForwardRefusesAMalformedTarget: the target comes from the host, and
// anything but a loopback address and port could be split by ssh into
// other fields of the -L spec. It is refused before ssh runs.
func TestForwardRefusesAMalformedTarget(t *testing.T) {
	refusedBeforeSSH(t,
		"", "a:b:c", "/run/x:1", "[::1]:22:3", "127.0.0.1:0", "127.0.0.1:70000", "127.0.0.1:022",
		"[127.0.0.1]:1", "127.0.0.1:", ":22", "relative/x.sock", `/run/back\slash`, "[fe80::1%eth0]:22",
		"[::1%lo]:22", "-oProxyCommand=x:22", "host name:22", `h\ost:22`, "127.0.0.1:22 ", "[::1]:+22",
	)
}

// TestForwardRefusesTargetsSSHWouldExpand: ssh expands ${VAR} in a -L spec
// from the client's environment, so a target the host chose could carry the
// user's secrets to the host's sshd in the forward's connect path. Only the
// agent's own loopback address and port get to ssh.
func TestForwardRefusesTargetsSSHWouldExpand(t *testing.T) {
	refusedBeforeSSH(t, "/tmp/${HOME}", "/run/x${PATH}y", "127.0.0.1:${PORT}", "localhost:4000", "10.0.0.5:4000", "/run/user/1000/robin/ice.sock")
}

func TestCheckTargetTakesWhatTheAgentSends(t *testing.T) {
	for _, remote := range []string{"127.0.0.1:41234", "127.0.0.1:1", "127.0.0.1:65535", "[::1]:22"} {
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
