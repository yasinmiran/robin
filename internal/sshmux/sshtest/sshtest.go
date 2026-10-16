// Package sshtest runs a throwaway OpenSSH server as the current user for
// integration tests. The server listens on 127.0.0.1 and accepts only a key it
// generates; the client side is a config file that trusts only that server.
package sshtest

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Dest is the host alias that ConfigFile defines for the server.
const Dest = "sshtest"

// Server is a running sshd. Sessions run as the current user with the user's
// own login shell and home directory.
type Server struct {
	// Dest is the destination to pass to ssh, defined in ConfigFile.
	Dest string
	// ConfigFile is an ssh client config with the key, port and known host.
	ConfigFile string
	// Args is {"-F", ConfigFile}, ready to put before the destination.
	Args []string
	// Addr is the server's "127.0.0.1:port".
	Addr string
	// Dir holds the keys, configs and the server log.
	Dir string

	cmd    *exec.Cmd
	exited chan struct{}
}

var errPortTaken = errors.New("port taken")

// Start runs sshd until the test ends. It skips the test when ssh, ssh-keygen
// or sshd are missing.
func Start(t testing.TB) *Server {
	t.Helper()
	sshd := findSSHD()
	if sshd == "" {
		t.Skip("sshtest: sshd not found")
	}
	for _, tool := range []string{"ssh", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("sshtest: %s not found", tool)
		}
	}
	u, err := user.Current()
	if err != nil {
		t.Fatalf("sshtest: current user: %v", err)
	}

	dir := t.TempDir()
	s := &Server{Dest: Dest, Dir: dir, ConfigFile: filepath.Join(dir, "ssh_config")}
	s.Args = []string{"-F", s.ConfigFile}
	hostKey := keygen(t, dir, "host_key")
	clientKey := keygen(t, dir, "client_key")
	copyFile(t, clientKey+".pub", filepath.Join(dir, "authorized_keys"))

	for attempt := 0; ; attempt++ {
		port := freePort(t)
		err := s.launch(t, sshd, port, hostKey)
		if errors.Is(err, errPortTaken) && attempt < 5 {
			continue
		}
		if err != nil {
			t.Fatalf("sshtest: %v", err)
		}
		s.writeClientConfig(t, port, hostKey, clientKey, u.Username)
		return s
	}
}

// Stop kills the listener, so new connections fail. Open sessions run in
// their own process groups and last until their client disconnects. Stop is
// safe to call more than once; Start registers it as a cleanup.
func (s *Server) Stop() {
	select {
	case <-s.exited:
		return
	default:
	}
	_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-s.exited:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		<-s.exited
	}
}

// Log returns what sshd has logged so far.
func (s *Server) Log() string {
	b, _ := os.ReadFile(filepath.Join(s.Dir, "sshd.log"))
	return string(b)
}

func (s *Server) launch(t testing.TB, sshd string, port int, hostKey string) error {
	cfg := filepath.Join(s.Dir, "sshd_config")
	writeFile(t, cfg, fmt.Sprintf(`ListenAddress 127.0.0.1:%d
HostKey %s
AuthorizedKeysFile %s
PidFile none
UsePAM no
StrictModes no
PubkeyAuthentication yes
PasswordAuthentication no
Subsystem sftp internal-sftp
AllowTcpForwarding yes
AllowStreamLocalForwarding yes
PrintMotd no
LogLevel VERBOSE
`, port, hostKey, filepath.Join(s.Dir, "authorized_keys")))

	logPath := filepath.Join(s.Dir, "sshd.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer logFile.Close()

	// sshd re-executes itself and refuses to run unless started by absolute path.
	cmd := exec.Command(sshd, "-D", "-e", "-f", cfg)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sshd: %w", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	s.cmd, s.exited = cmd, exited
	s.Addr = fmt.Sprintf("127.0.0.1:%d", port)

	deadline := time.Now().Add(10 * time.Second)
	for {
		if hasBanner(s.Addr) {
			t.Cleanup(func() {
				s.Stop()
				if t.Failed() {
					t.Logf("sshtest: sshd log:\n%s", s.Log())
				}
			})
			return nil
		}
		select {
		case <-exited:
			log := s.Log()
			if strings.Contains(log, "Address already in use") {
				return errPortTaken
			}
			return fmt.Errorf("sshd exited before listening:\n%s", log)
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			s.Stop()
			return fmt.Errorf("sshd not listening on %s after 10s:\n%s", s.Addr, s.Log())
		}
	}
}

func (s *Server) writeClientConfig(t testing.TB, port int, hostKey, clientKey, username string) {
	pub, err := os.ReadFile(hostKey + ".pub")
	if err != nil {
		t.Fatalf("sshtest: %v", err)
	}
	fields := strings.Fields(string(pub))
	knownHosts := filepath.Join(s.Dir, "known_hosts")
	writeFile(t, knownHosts, fmt.Sprintf("[127.0.0.1]:%d %s %s\n", port, fields[0], fields[1]))
	writeFile(t, s.ConfigFile, fmt.Sprintf(`Host %s
	HostName 127.0.0.1
	Port %d
	User %s
	IdentityFile %s
	IdentitiesOnly yes
	IdentityAgent none
	UserKnownHostsFile %s
	GlobalKnownHostsFile /dev/null
	StrictHostKeyChecking yes
	CheckHostIP no
	BatchMode yes
	PasswordAuthentication no
	LogLevel ERROR
`, s.Dest, port, username, clientKey, knownHosts))
}

func hasBanner(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	return err == nil && strings.HasPrefix(line, "SSH-2.0-")
}

func findSSHD() string {
	for _, p := range []string{"/usr/sbin/sshd", "/usr/local/sbin/sshd", "/opt/homebrew/sbin/sshd"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("sshd"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
	}
	return ""
}

func freePort(t testing.TB) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("sshtest: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func keygen(t testing.TB, dir, name string) string {
	path := filepath.Join(dir, name)
	out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "", "-f", path).CombinedOutput()
	if err != nil {
		t.Fatalf("sshtest: ssh-keygen: %v: %s", err, out)
	}
	return path
}

func writeFile(t testing.TB, path, content string) {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("sshtest: %v", err)
	}
}

func copyFile(t testing.TB, src, dst string) {
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("sshtest: %v", err)
	}
	writeFile(t, dst, string(b))
}
