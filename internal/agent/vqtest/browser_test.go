//go:build linux && cgo && openh264

package vqtest

import (
	"errors"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/protocol"
)

var errNoAnswer = errors.New("no answer")

type refusingAgent struct{}

func (refusingAgent) Answer(string) (string, error)         { return "", errNoAnswer }
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
	if err := b.Connect(refusingAgent{}, nil); !errors.Is(err, errNoAnswer) {
		t.Fatalf("Connect: %v", err)
	}
	// A slice with no parameter sets fails; parameter sets alone show
	// nothing.
	b.frames <- decodeJob{f: Frame{Payloads: [][]byte{{0x65, 0x88, 0x84}}}, at: time.Now()}
	b.frames <- decodeJob{f: Frame{Payloads: [][]byte{{0x68, 0xce, 0x3c, 0x80}}}, at: time.Now()}
	r, err := b.Close()
	if err != nil {
		t.Error(err)
	}
	if r.DecodeErrors+r.NoPicture != 2 || len(r.Shown) != 0 {
		t.Fatalf("result %+v", r)
	}
	if again, err := b.Close(); err != nil || again.DecodeErrors != r.DecodeErrors {
		t.Fatalf("a second Close: %+v, %v", again, err)
	}
}

// TestBrowserScoresEverythingBeforeClose hands the browser a run of
// frames and closes it at once: every picture is still shown and scored.
func TestBrowserScoresEverythingBeforeClose(t *testing.T) {
	n, err := Link{}.Build()
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	clip, err := NewClip(Captured, 320, 240, Segment{Scroll, 30})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBrowser(n.Browser, clip, Chrome)
	if err != nil {
		t.Fatal(err)
	}
	aus := encodeClip(t, clip, 30, 2_000_000)
	rx := NewReceiver(Chrome, time.Now())
	for _, p := range packetize(aus) {
		for _, f := range rx.Push(p, time.Now()) {
			b.frames <- decodeJob{f: f, at: time.Now()}
		}
	}
	r, err := b.Close()
	if err != nil {
		t.Error(err)
	}
	if len(r.Shown) != len(aus) {
		t.Fatalf("showed %d of %d pictures", len(r.Shown), len(aus))
	}
	for _, s := range r.Shown {
		if s.Score.PSNR == 0 || s.Score.SSIM == 0 {
			t.Errorf("frame %d was shown but not scored: %+v", s.Index, s.Score)
		}
	}
}
