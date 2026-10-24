//go:build linux

package agent

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/protocol"
)

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
	if IsDaemon(os.Getpid(), "ab12") || IsDaemon(0, "ab12") || IsDaemon(1<<30, "ab12") {
		t.Error("IsDaemon found a daemon where there is none")
	}
	if FindDaemon("no-such-session-anywhere") != 0 {
		t.Error("FindDaemon found a daemon where there is none")
	}
}

// fakeDaemon runs a process whose command line is that of the daemon of
// session id, until the test ends.
func fakeDaemon(t *testing.T, id string) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "read x", "sh", "daemon", "-id", id)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		in.Close()
		cmd.Process.Kill()
		cmd.Wait()
	})
	// Start returns once exec has begun, which can be before the kernel
	// shows the new command line.
	pid := cmd.Process.Pid
	for deadline := time.Now().Add(2 * time.Second); !commandIsDaemon(cmdline(pid), id); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d never showed its command line", pid)
		}
	}
	return pid
}

// TestDaemonIsThisUsersProcess: anyone can run a process whose command
// line names a session's daemon, and read every command line in /proc;
// only one of this user's processes counts.
func TestDaemonIsThisUsersProcess(t *testing.T) {
	pid := fakeDaemon(t, "zz11")
	if !IsDaemon(pid, "zz11") || FindDaemon("zz11") != pid {
		t.Fatalf("IsDaemon %v, FindDaemon %d; want this user's daemon %d", IsDaemon(pid, "zz11"), FindDaemon("zz11"), pid)
	}
	saved := processUID
	processUID = func(int) (uint32, error) { return uint32(os.Getuid()) + 1, nil }
	t.Cleanup(func() { processUID = saved })
	if IsDaemon(pid, "zz11") {
		t.Error("IsDaemon took another user's process for the daemon")
	}
	if got := FindDaemon("zz11"); got != 0 {
		t.Errorf("FindDaemon = %d, another user's process", got)
	}
}

// TestSweepRunRootRemovesADeadDaemonsDirectory: a daemon killed before its
// first session.json leaves its socket behind, and nothing else would
// ever remove the directory.
func TestSweepRunRootRemovesADeadDaemonsDirectory(t *testing.T) {
	root := t.TempDir()
	mk := func(name string) string {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		ln, err := net.Listen("unix", filepath.Join(dir, SocketFile))
		if err != nil {
			t.Fatal(err)
		}
		// The socket stays as a killed daemon leaves it.
		ln.(*net.UnixListener).SetUnlinkOnClose(false)
		ln.Close()
		return dir
	}
	dead := mk("ab12")
	starting := mk("cd34")
	fakeDaemon(t, "cd34")

	sweepRunRoot(root, "")

	if _, err := os.Lstat(dead); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a dead daemon's directory survived the sweep: %v", err)
	}
	if _, err := os.Lstat(starting); err != nil {
		t.Errorf("a starting daemon's directory was swept: %v", err)
	}
}

// TestSweepRunRootRemovesADirectoryWhosePidIsAnotherUsers: once a daemon
// is gone its pid may go to another user's process, which this user
// cannot signal and so cannot be its daemon.
func TestSweepRunRootRemovesADirectoryWhosePidIsAnotherUsers(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can signal every process")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "ab12")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Pid 1 is init, root's.
	if err := WriteSession(dir, protocol.Session{ID: "ab12", PID: 1}); err != nil {
		t.Fatal(err)
	}
	sweepRunRoot(root, "")
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a directory whose pid is another user's survived the sweep: %v", err)
	}
}
