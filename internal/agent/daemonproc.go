//go:build linux

package agent

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// processUID is the real uid pid runs as. A var so a test can make its own
// process a stranger's.
var processUID = func(pid int) (uint32, error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, err
	}
	for line := range strings.Lines(string(b)) {
		if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
			if f := strings.Fields(rest); len(f) > 0 {
				uid, err := strconv.ParseUint(f[0], 10, 32)
				return uint32(uid), err
			}
		}
	}
	return 0, errors.New("agent: no Uid in /proc status")
}

// IsDaemon reports whether pid is a process of this user's running
// `robin-agent daemon -id id`. Anyone can run a process whose command line
// says so, and read every command line on the host.
func IsDaemon(pid int, id string) bool {
	if pid <= 0 || !ours(pid) {
		return false
	}
	return commandIsDaemon(cmdline(pid), id)
}

// FindDaemon is the pid of this user's process running `robin-agent daemon
// -id id`, or 0.
func FindDaemon(id string) int {
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if commandIsDaemon(cmdline(pid), id) && ours(pid) {
			return pid
		}
	}
	return 0
}

func ours(pid int) bool {
	uid, err := processUID(pid)
	return err == nil && uid == uint32(os.Getuid())
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
