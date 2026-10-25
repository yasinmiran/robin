package peer

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// fakeGCC counts the feedback it is given.
type fakeGCC struct{ feedback int }

func (f *fakeGCC) AddStream(_ *interceptor.StreamInfo, w interceptor.RTPWriter) interceptor.RTPWriter {
	return w
}
func (f *fakeGCC) WriteRTCP([]rtcp.Packet, interceptor.Attributes) error { f.feedback++; return nil }
func (f *fakeGCC) GetTargetBitrate() int                                 { return 0 }
func (f *fakeGCC) OnTargetBitrateChange(func(int))                       {}
func (f *fakeGCC) GetStats() map[string]any                              { return map[string]any{} }
func (f *fakeGCC) Close() error                                          { return nil }

// testEstimator is an estimator over a fake GCC at bps, with time from
// *now, whose pacer sends at once.
func testEstimator(now *time.Time, bps int) (*estimator, *fakeGCC) {
	f := &fakeGCC{}
	p := gcc.NewNoOpPacer()
	p.AddStream(1, interceptor.RTPWriterFunc(func(h *rtp.Header, b []byte, _ interceptor.Attributes) (int, error) {
		return h.MarshalSize() + len(b), nil
	}))
	e := &estimator{inner: f, pacer: p, now: func() time.Time { return *now }, log: slog.New(slog.NewTextHandler(io.Discard, nil)), target: bps}
	e.alr.setTarget(bps)
	return e, f
}

// feed sends at bps for d in 10 ms steps through the estimator's pacer,
// with feedback every 50 ms, and returns how much of the feedback reached
// GCC after the first `settle` of it.
func feed(t *testing.T, e *estimator, f *fakeGCC, now *time.Time, bps int, d, settle time.Duration) (passed, offered int) {
	t.Helper()
	const step = 10 * time.Millisecond
	payload := make([]byte, 1000)
	owed := 0.0
	for at := time.Duration(0); at < d; at += step {
		*now = now.Add(step)
		for owed += float64(bps) / 8 * step.Seconds(); owed >= 1012; owed -= 1012 {
			if _, err := (meteredPacer{e}).Write(&rtp.Header{SSRC: 1}, payload, nil); err != nil {
				t.Fatal(err)
			}
		}
		if (at/step)%5 == 4 {
			before := f.feedback
			if err := e.WriteRTCP([]rtcp.Packet{&rtcp.TransportLayerCC{}}, nil); err != nil {
				t.Fatal(err)
			}
			if at >= settle {
				offered++
				passed += f.feedback - before
			}
		}
	}
	return passed, offered
}

// TestEstimatorWithholdsFeedbackOfAStillScreen is the hold and the probe:
// GCC hears the browser while the agent fills the target, nothing once it
// has gone still, and again soon after it fills the target once more, so
// the motion after a still screen starts at the target from before it.
func TestEstimatorWithholdsFeedbackOfAStillScreen(t *testing.T) {
	now := time.Unix(0, 0)
	e, f := testEstimator(&now, 2_000_000)
	if passed, offered := feed(t, e, f, &now, 2_000_000, 2*time.Second, 0); passed != offered {
		t.Fatalf("filling the target: %d of %d feedback reached GCC", passed, offered)
	}
	// A still screen: a refresh now and then, and sound.
	if passed, _ := feed(t, e, f, &now, 150_000, 30*time.Second, 1500*time.Millisecond); passed != 0 {
		t.Fatalf("still: %d feedback reached GCC", passed)
	}
	if !e.GetStats()["applicationLimited"].(bool) {
		t.Fatal("stats do not say the sender is limited")
	}
	if passed, offered := feed(t, e, f, &now, 2_000_000, 2*time.Second, 800*time.Millisecond); passed != offered {
		t.Fatalf("filling the target again: %d of %d feedback reached GCC after 0.8 s", passed, offered)
	}
	if e.GetStats()["applicationLimited"].(bool) {
		t.Fatal("stats say the sender is still limited")
	}
}

// TestEstimatorFollowsTheTarget checks that the detector's budget is the
// target GCC sets through the pacer.
func TestEstimatorFollowsTheTarget(t *testing.T) {
	now := time.Unix(0, 0)
	e, f := testEstimator(&now, 10_000_000)
	(meteredPacer{e}).SetTargetBitrate(1_000_000)
	if e.target != 1_000_000 {
		t.Fatalf("target %d", e.target)
	}
	// 1 Mbit/s fills a 1 Mbit/s target, which it would not at 10.
	feed(t, e, f, &now, 0, 5*time.Second, 0)
	if passed, offered := feed(t, e, f, &now, 1_000_000, 2*time.Second, 800*time.Millisecond); passed != offered {
		t.Fatalf("filling the lowered target: %d of %d feedback reached GCC", passed, offered)
	}
}

func TestEstimatorPrioritizesSound(t *testing.T) {
	p := newPacerAt(1_000_000, time.Now)
	e, err := newEstimator(Config{MinBitrate: 100_000, StartBitrate: 1_000_000, MaxBitrate: 2_000_000}, p, slog.Default(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	w := interceptor.RTPWriterFunc(func(h *rtp.Header, b []byte, _ interceptor.Attributes) (int, error) { return len(b), nil })
	e.AddStream(&interceptor.StreamInfo{SSRC: 1, MimeType: "video/H264"}, w)
	e.AddStream(&interceptor.StreamInfo{SSRC: 2, MimeType: "audio/opus"}, w)
	if p.priority[1] || !p.priority[2] {
		t.Fatalf("priority %v, want the audio stream only", p.priority)
	}
}
