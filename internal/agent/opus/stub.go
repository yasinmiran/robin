//go:build !(linux && cgo && opus)

package opus

import "errors"

// ErrUnavailable means this binary was built without the opus tag, so it has
// no codec. The Makefile builds the agent with it.
var ErrUnavailable = errors.New("opus: built without libopus (build with -tags opus)")

// Encoder is a placeholder in builds without libopus.
type Encoder struct{}

// NewEncoder always fails in builds without libopus.
func NewEncoder(bitrate, complexity int) (*Encoder, error) { return nil, ErrUnavailable }

// Encode is never reachable since NewEncoder fails.
func (*Encoder) Encode([]int16) ([]byte, error) { return nil, ErrUnavailable }

// Close is never reachable since NewEncoder fails.
func (*Encoder) Close() error { return nil }

// Decoder is a placeholder in builds without libopus.
type Decoder struct{}

// NewDecoder always fails in builds without libopus.
func NewDecoder(channels int) (*Decoder, error) { return nil, ErrUnavailable }

// Decode is never reachable since NewDecoder fails.
func (*Decoder) Decode([]byte, []int16) (int, error) { return 0, ErrUnavailable }

// Recover is never reachable since NewDecoder fails.
func (*Decoder) Recover([]byte, []int16, int) (int, error) { return 0, ErrUnavailable }

// Conceal is never reachable since NewDecoder fails.
func (*Decoder) Conceal([]int16, int) (int, error) { return 0, ErrUnavailable }

// Close is never reachable since NewDecoder fails.
func (*Decoder) Close() error { return nil }

// PacketSamples always fails in builds without libopus.
func PacketSamples([]byte) (int, error) { return 0, ErrUnavailable }
