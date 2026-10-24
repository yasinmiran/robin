//go:build !linux && !windows

package sshmux

import "errors"

// listenerOf is for forwards on loopback TCP, which only Windows uses.
func listenerOf(string, int) (listening, ours bool, err error) {
	return false, false, errors.ErrUnsupported
}
