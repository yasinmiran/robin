//go:build !(linux && cgo && opus)

package opus

import "errors"

// ErrUnavailable means this binary was built without the opus tag, so it has
// no encoder. The Makefile builds the agent with it.
var ErrUnavailable = errors.New("opus: built without libopus (build with -tags opus)")

// Encoder is a placeholder in builds without libopus.
type Encoder struct{}

// NewEncoder always fails in builds without libopus.
func NewEncoder(bitrate, complexity int) (*Encoder, error) { return nil, ErrUnavailable }

// Encode is never reachable since NewEncoder fails.
func (*Encoder) Encode([]int16) ([]byte, error) { return nil, ErrUnavailable }

// Close is never reachable since NewEncoder fails.
func (*Encoder) Close() error { return nil }
