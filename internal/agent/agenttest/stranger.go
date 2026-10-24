//go:build linux

package agenttest

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// StrangerName is the second account of the test-x image: another user
// of the host, for tests of what such a user can reach of a session.
const StrangerName = "robin-stranger"

// strangerEnv marks a test binary that Stranger.RunTest started.
const strangerEnv = "ROBIN_TEST_STRANGER"

// Stranger is the second account, able to run this test binary.
type Stranger struct {
	UID, GID uint32
	// Exe is a copy of the running test binary, in a directory of its
	// own that the stranger can read.
	Exe string
}

// NewStranger returns the second account. Running a process as another
// uid takes root, so the test is skipped unless it runs as root on a host
// that has the account: inside the test-x container.
func NewStranger(t testing.TB) *Stranger {
	t.Helper()
	u, err := user.Lookup(StrangerName)
	if os.Getuid() != 0 || err != nil {
		t.Skipf("needs root and the %s account; run via internal/agent/x11/testdata/xtest.sh", StrangerName)
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
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

// Command runs name as the stranger, with this process's environment.
func (s *Stranger) Command(name string, arg ...string) *exec.Cmd {
	cmd := exec.Command(name, arg...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: s.UID, Gid: s.GID}}
	return cmd
}

// RunTest runs the test named again in a copy of this binary, as the
// stranger, with env added, and returns what it printed. The test tells
// the run apart with IsStranger and takes the stranger's side there.
func (s *Stranger) RunTest(t testing.TB, test string, env ...string) string {
	t.Helper()
	cmd := s.Command(s.Exe, "-test.run=^"+test+"$", "-test.count=1")
	cmd.Dir = filepath.Dir(s.Exe)
	cmd.Env = append(append(os.Environ(), strangerEnv+"=1"), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s as %s: %v\n%s", test, StrangerName, err, out)
	}
	return string(out)
}

// IsStranger reports whether this test binary is one RunTest started.
func IsStranger() bool { return os.Getenv(strangerEnv) != "" }
