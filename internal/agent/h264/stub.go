//go:build !(linux && cgo && openh264)

package h264

import (
	"errors"
	"time"

	"github.com/yasinmiran/remotejump/internal/agent/yuv"
)

// ErrUnavailable means this binary was built without the openh264 tag, so it
// has no encoder. The Makefile builds the agent with it.
var ErrUnavailable = errors.New("h264: built without openh264 (build with -tags openh264)")

// Encoder is a placeholder in builds without openh264.
type Encoder struct{}

// New always fails in builds without openh264.
func New(width, height, fps, bitrate int) (*Encoder, error) { return nil, ErrUnavailable }

// Encode is never reachable since New fails.
func (*Encoder) Encode(*yuv.I420, time.Duration, bool) ([]byte, error) { return nil, ErrUnavailable }

// SetBitrate is never reachable since New fails.
func (*Encoder) SetBitrate(int) error { return ErrUnavailable }

// Close is never reachable since New fails.
func (*Encoder) Close() error { return nil }
