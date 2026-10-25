//go:build !linux

package sshtest

import "time"

// interruptIgnored has no /proc to read the ignore from.
func interruptIgnored(int) error {
	time.Sleep(300 * time.Millisecond)
	return nil
}
