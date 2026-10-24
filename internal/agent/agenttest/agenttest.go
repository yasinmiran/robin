//go:build linux

// Package agenttest holds what tests of the agent and its daemon share:
// finding the processes a session started.
package agenttest

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// FindProcess returns the pid of a live process with the command named and
// every other word given on its command line, or 0.
func FindProcess(want ...string) int {
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		argv := strings.Split(string(b), "\x00")
		// argv[0] is the path LookPath found, not the bare name.
		if len(argv) == 0 || filepath.Base(argv[0]) != want[0] {
			continue
		}
		if slices.ContainsFunc(want[1:], func(w string) bool { return !slices.Contains(argv, w) }) {
			continue
		}
		if Alive(pid) {
			return pid
		}
	}
	return 0
}

// Alive reports whether a process with pid exists.
func Alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) != syscall.ESRCH }

// XvfbPID finds the Xvfb whose cookie file is in run directory dir, or 0.
func XvfbPID(dir string) int {
	return FindProcess("Xvfb", "-auth", filepath.Join(dir, "Xauthority"))
}
