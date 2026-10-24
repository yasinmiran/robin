//go:build linux

package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/yasinmiran/robin/internal/agent"
)

// endTimeout bounds the wait for a daemon told to end to take its run
// directory with it.
const endTimeout = 15 * time.Second

// End tells the daemon of the session in dir to end, which tears the
// desktop down like bye does, and waits for its run directory to go. It
// signals only a process whose command line is that daemon's, since the
// pid in session.json may have been reused once the daemon died; such a
// stale directory is removed instead.
func End(dir string) error {
	id := filepath.Base(dir)
	st, err := agent.ReadSession(dir)
	if errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Lstat(dir); statErr == nil {
			return os.RemoveAll(dir)
		}
		return fmt.Errorf("no session %s on this host", id)
	}
	if err != nil {
		return err
	}
	if !isDaemon(st.PID, id) {
		return os.RemoveAll(dir)
	}
	if err := syscall.Kill(st.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("end session %s: %w", id, err)
	}
	deadline := time.Now().Add(endTimeout)
	for time.Now().Before(deadline) {
		if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("session %s did not end within %s", id, endTimeout)
}

// isDaemon reports whether pid runs `robin-agent daemon -id id`.
func isDaemon(pid int, id string) bool {
	if pid <= 0 {
		return false
	}
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	return commandIsDaemon(bytes.Split(b, []byte{0}), id)
}

func commandIsDaemon(argv [][]byte, id string) bool {
	for i := 1; i+2 < len(argv); i++ {
		if string(argv[i]) == "daemon" && string(argv[i+1]) == "-id" && string(argv[i+2]) == id {
			return true
		}
	}
	return false
}
