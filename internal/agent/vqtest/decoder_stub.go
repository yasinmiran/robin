//go:build !(linux && cgo && openh264)

package vqtest

import (
	"errors"

	"github.com/yasinmiran/robin/internal/agent/yuv"
)

// ErrNoDecoder means this binary was built without the openh264 tag.
var ErrNoDecoder = errors.New("vqtest: built without openh264 (build with -tags openh264)")

// Decoder is a placeholder in builds without openh264.
type Decoder struct{}

// NewDecoder always fails in builds without openh264.
func NewDecoder() (*Decoder, error) { return nil, ErrNoDecoder }

// Decode is never reachable since NewDecoder fails.
func (*Decoder) Decode([]byte) (*yuv.I420, error) { return nil, ErrNoDecoder }

// Close is never reachable since NewDecoder fails.
func (*Decoder) Close() error { return nil }
