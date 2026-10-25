//go:build linux

package agenttest

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// strangerName is the second account of the test-x image: another user
// of the host, for tests of what such a user can reach of a session.
const strangerName = "robin-stranger"

// strangerEnv marks a test binary that Stranger.RunTest started, and holds
// the socket that binary is to try.
const strangerEnv = "ROBIN_TEST_STRANGER"

// xtestEnv is set by internal/agent/x11/testdata/xtest.sh in its container,
// which has the second account: there a test that cannot have it fails
// instead of skipping.
const xtestEnv = "ROBIN_XTEST"

// Stranger is the second account, able to run this test binary.
type Stranger struct {
	UID, GID uint32
	// Exe is a copy of the running test binary, in a directory of its
	// own that the stranger can read.
	Exe string
}

// NewStranger returns the second account. Running a process as another
// uid takes root, so the test is skipped unless it runs as root on a host
// that has the account, as inside the test-x container, where it fails
// instead.
func NewStranger(t testing.TB) *Stranger {
	t.Helper()
	u, err := user.Lookup(strangerName)
	if os.Getuid() != 0 || err != nil {
		if os.Getenv(xtestEnv) != "" {
			t.Fatalf("the test-x container has no root or no %s account: uid %d, lookup: %v", strangerName, os.Getuid(), err)
		}
		t.Skipf("needs root and the %s account; run via internal/agent/x11/testdata/xtest.sh", strangerName)
	}
	uid, uidErr := strconv.ParseUint(u.Uid, 10, 32)
	gid, gidErr := strconv.ParseUint(u.Gid, 10, 32)
	if err := errors.Join(uidErr, gidErr); err != nil {
		t.Fatalf("%s's ids: %v", strangerName, err)
	}
	s := &Stranger{UID: uint32(uid), GID: uint32(gid), Exe: filepath.Join(OpenDir(t), filepath.Base(os.Args[0]))}
	b, err := os.ReadFile("/proc/self/exe")
	if err == nil {
		err = os.WriteFile(s.Exe, b, 0o755)
	}
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// OpenDir makes a directory anyone may enter and list, removed when the
// test ends. t.TempDir's parent is private to the test's uid.
func OpenDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "robin-stranger-")
	if err == nil {
		t.Cleanup(func() { os.RemoveAll(dir) })
		err = os.Chmod(dir, 0o755)
	}
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Command returns a command that runs name as the stranger, with this
// process's environment.
func (s *Stranger) Command(name string, arg ...string) *exec.Cmd {
	cmd := exec.Command(name, arg...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: s.UID, Gid: s.GID}}
	return cmd
}

// RunTest runs the test named again in a copy of this binary, as the
// stranger, and returns what it printed. That run finds socket in
// StrangerSocket and takes the stranger's side with it.
func (s *Stranger) RunTest(t testing.TB, test, socket string) string {
	t.Helper()
	cmd := s.Command(s.Exe, "-test.run=^"+test+"$", "-test.count=1")
	cmd.Dir = filepath.Dir(s.Exe)
	cmd.Env = append(os.Environ(), strangerEnv+"="+socket)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s as %s: %v\n%s", test, strangerName, err, out)
	}
	return string(out)
}

// StrangerSocket is the socket a test binary RunTest started is to try,
// or "" in any other run.
func StrangerSocket() string { return os.Getenv(strangerEnv) }

// HungUp reports whether err, from a read, says the other end closed the
// connection: EOF, or a reset when it closed with what was sent to it
// still unread.
func HungUp(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET)
}
