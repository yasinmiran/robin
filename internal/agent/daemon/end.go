//go:build linux

package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/yasinmiran/robin/internal/agent"
)

// endTimeout bounds the wait for a daemon told to end to take its run
// directory with it.
const endTimeout = 15 * time.Second

// End tells the daemon of session id under runRoot to end, which tears the
// desktop down like bye does, and waits for its run directory to go. It
// signals only a process whose command line is that daemon's, since the
// pid in session.json may have been reused once the daemon died; such a
// stale directory is removed instead. A directory with no trace of a
// session is left alone.
func End(ctx context.Context, runRoot, id string) error {
	if err := validID(id); err != nil {
		return err
	}
	dir := filepath.Join(runRoot, id)
	pid, err := daemonPID(dir, id)
	if err != nil {
		return err
	}
	if pid == 0 {
		return os.RemoveAll(dir)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("end session %s: %w", id, err)
	}
	ctx, cancel := context.WithTimeout(ctx, endTimeout)
	defer cancel()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return fmt.Errorf("session %s did not end within %s", id, endTimeout)
		}
	}
}

// daemonPID is the pid of the live daemon of session id in dir: the one
// session.json names when it still runs that daemon, else the one whose
// command line says so, as a daemon that has not written session.json yet.
// It is 0 for a directory the daemon left behind, and the error says when
// dir is no session directory at all.
func daemonPID(dir, id string) (int, error) {
	st, err := agent.ReadSession(dir)
	switch {
	case err == nil:
		if agent.IsDaemon(st.PID, id) {
			return st.PID, nil
		}
		return 0, nil
	case !errors.Is(err, os.ErrNotExist):
		return 0, err
	}
	if pid := agent.FindDaemon(id); pid != 0 {
		return pid, nil
	}
	if fi, err := os.Lstat(filepath.Join(dir, SocketFile)); err == nil && fi.Mode()&os.ModeSocket != 0 {
		return 0, nil
	}
	return 0, &NoSessionError{id}
}
