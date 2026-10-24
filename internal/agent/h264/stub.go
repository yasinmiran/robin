//go:build !(linux && cgo && openh264)

package h264

import (
	"errors"
	"time"

	"github.com/yasinmiran/robin/internal/agent/yuv"
)

// ErrUnavailable means this binary was built without the openh264 tag, so it
// has no codec. The Makefile builds the agent with it.
var ErrUnavailable = errors.New("h264: built without openh264 (build with -tags openh264)")

// Encoder is a placeholder in builds without openh264.
type Encoder struct{}

// New always fails in builds without openh264.
func New(width, height, fps, bitrate int) (*Encoder, error) { return nil, ErrUnavailable }

// Open always fails in builds without openh264.
func Open(Params) (*Encoder, error) { return nil, ErrUnavailable }

// Encode is never reachable since New fails.
func (*Encoder) Encode(*yuv.I420, time.Duration, bool) ([]byte, error) { return nil, ErrUnavailable }

// QP is never reachable since New fails.
func (*Encoder) QP() int { return -1 }

// SetBitrate is never reachable since New fails.
func (*Encoder) SetBitrate(int) error { return ErrUnavailable }

// Close is never reachable since New fails.
func (*Encoder) Close() error { return nil }

// Decoder is a placeholder in builds without openh264.
type Decoder struct{}

// NewDecoder always fails in builds without openh264.
func NewDecoder() (*Decoder, error) { return nil, ErrUnavailable }

// Decode is never reachable since NewDecoder fails.
func (*Decoder) Decode([]byte) (*Picture, error) { return nil, ErrUnavailable }

// Close is never reachable since NewDecoder fails.
func (*Decoder) Close() error { return nil }
