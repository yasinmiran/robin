package sshtest

import (
	"errors"
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
	if err := ignoresInterrupt(string(status)); err != nil {
		return fmt.Errorf("pid %d: %w", pid, err)
	}
	return nil
}

// ignoresInterrupt fails unless the SigIgn line of a /proc/<pid>/status
// has SIGINT.
func ignoresInterrupt(status string) error {
	for line := range strings.Lines(status) {
		mask, ok := strings.CutPrefix(line, "SigIgn:")
		if !ok {
			continue
		}
		bits, err := strconv.ParseUint(strings.TrimSpace(mask), 16, 64)
		if err != nil {
			return fmt.Errorf("SigIgn: %w", err)
		}
		if bits&(1<<(syscall.SIGINT-1)) == 0 {
			return errors.New("does not ignore SIGINT")
		}
		return nil
	}
	return errors.New("no SigIgn in the status")
}
