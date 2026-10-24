//go:build linux

package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yasinmiran/robin/internal/agent"
)

// endTimeout bounds the wait for a daemon told to end to take its run
// directory with it.
const endTimeout = 15 * time.Second

// End tells the daemon of session id under runRoot to end, which tears the
// desktop down like bye does, and waits for its run directory to go. It
// refuses a directory of another user's, and signals only a process of
// this user whose command line is that daemon's, since the pid in
// session.json may have been reused once the daemon died, and anyone may
// start a process with any command line; such a stale directory is
// removed instead. A directory with no trace of a session is left alone.
func End(ctx context.Context, runRoot, id string) error {
	if err := validID(id); err != nil {
		return err
	}
	dir := filepath.Join(runRoot, id)
	if err := checkOwner(dir); err != nil {
		return fmt.Errorf("end session %s: %w", id, err)
	}
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

// checkOwner refuses a session directory of another user's: the daemon in
// it is not ours to signal, nor its files ours to remove. A missing one is
// left for daemonPID to tell apart.
func checkOwner(dir string) error {
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s belongs to uid %d, not us", dir, st.Uid)
	}
	return nil
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
		if isDaemon(st.PID, id) {
			return st.PID, nil
		}
		return 0, nil
	case !errors.Is(err, os.ErrNotExist):
		return 0, err
	}
	if pid := findDaemon(id); pid != 0 {
		return pid, nil
	}
	if fi, err := os.Lstat(filepath.Join(dir, SocketFile)); err == nil && fi.Mode()&os.ModeSocket != 0 {
		return 0, nil
	}
	return 0, &NoSessionError{id}
}

// isDaemon reports whether pid is a process of ours running
// `robin-agent daemon -id id`.
func isDaemon(pid int, id string) bool {
	if pid <= 0 {
		return false
	}
	return commandIsDaemon(cmdline(pid), id) && ours(pid)
}

// findDaemon is the pid of our process running `robin-agent daemon -id id`,
// or 0.
func findDaemon(id string) int {
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if isDaemon(pid, id) {
			return pid
		}
	}
	return 0
}

// ours reports whether pid's real uid is this process's. Its command line
// alone proves nothing: another user's process may say what it likes
// there, and /proc shows it to everyone.
func ours(pid int) bool {
	b, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	// "Uid:\t<real>\t<effective>\t<saved>\t<filesystem>", never the first line.
	_, ids, _ := strings.Cut(string(b), "\nUid:")
	ruid, _, _ := strings.Cut(strings.TrimLeft(ids, "\t "), "\t")
	return ruid == strconv.Itoa(os.Getuid())
}

func cmdline(pid int) [][]byte {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil
	}
	return bytes.Split(b, []byte{0})
}

func commandIsDaemon(argv [][]byte, id string) bool {
	for i := 1; i+2 < len(argv); i++ {
		if string(argv[i]) == "daemon" && string(argv[i+1]) == "-id" && string(argv[i+2]) == id {
			return true
		}
	}
	return false
}
