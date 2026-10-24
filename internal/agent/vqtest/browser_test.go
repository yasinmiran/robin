//go:build linux && cgo && openh264

package vqtest

import (
	"errors"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/protocol"
)

type refusingAgent struct{}

func (refusingAgent) Answer(string) (string, error)         { return "", errors.New("no answer") }
func (refusingAgent) AddCandidate(protocol.Candidate) error { return nil }

// TestBrowserAlone covers what a browser does without a working agent: it
// reports that nothing was shown, passes on a failed answer, and counts
// frames it could not decode.
func TestBrowserAlone(t *testing.T) {
	n, err := Link{}.Build()
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	clip, err := NewClip(Synthetic, 320, 180, Segment{Still, 1})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBrowser(n.Browser, clip, Chrome)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.WaitShown(30 * time.Millisecond); !errors.Is(err, errNoVideo) {
		t.Fatalf("WaitShown: %v", err)
	}
	if err := b.Connect(refusingAgent{}, nil); err == nil || err.Error() != "no answer" {
		t.Fatalf("Connect: %v", err)
	}
	// A slice with no parameter sets fails; parameter sets alone show
	// nothing.
	b.frames <- decodeJob{f: Frame{Payloads: [][]byte{{0x65, 0x88, 0x84}}}, at: time.Now()}
	b.frames <- decodeJob{f: Frame{Payloads: [][]byte{{0x68, 0xce, 0x3c, 0x80}}}, at: time.Now()}
	r, _ := b.Close()
	if r.DecodeErrors+r.NoPicture != 2 || len(r.Shown) != 0 {
		t.Fatalf("result %+v", r)
	}
}
