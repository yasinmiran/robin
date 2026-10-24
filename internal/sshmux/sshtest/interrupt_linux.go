package sshtest

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// interruptIgnored fails unless pid's SigIgn mask in /proc has SIGINT.
func interruptIgnored(pid int) error {
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return err
	}
	ignored, err := ignoresInterrupt(string(status))
	switch {
	case err != nil:
		return fmt.Errorf("pid %d: %w", pid, err)
	case !ignored:
		return fmt.Errorf("pid %d does not ignore SIGINT", pid)
	}
	return nil
}

// ignoresInterrupt reads the SigIgn line of a /proc/<pid>/status.
func ignoresInterrupt(status string) (bool, error) {
	for line := range strings.Lines(status) {
		mask, ok := strings.CutPrefix(line, "SigIgn:")
		if !ok {
			continue
		}
		bits, err := strconv.ParseUint(strings.TrimSpace(mask), 16, 64)
		if err != nil {
			return false, fmt.Errorf("SigIgn: %w", err)
		}
		return bits&(1<<(syscall.SIGINT-1)) != 0, nil
	}
	return false, fmt.Errorf("no SigIgn in the status")
}
