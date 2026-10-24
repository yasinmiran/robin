//go:build !(linux && cgo && openh264)

package vqtest

import (
	"errors"
	"testing"
)

func TestStubDecoder(t *testing.T) {
	if _, err := NewDecoder(); !errors.Is(err, ErrNoDecoder) {
		t.Fatalf("NewDecoder: %v", err)
	}
	var d Decoder
	if _, err := d.Decode(nil); !errors.Is(err, ErrNoDecoder) {
		t.Fatalf("Decode: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewBrowser(nil, nil, Chrome); !errors.Is(err, ErrNoDecoder) {
		t.Fatalf("NewBrowser: %v", err)
	}
}
