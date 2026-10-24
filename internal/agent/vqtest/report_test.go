package vqtest

import (
	"strings"
	"testing"
	"time"
)

func TestSpans(t *testing.T) {
	clip, err := NewClip(Synthetic, 320, 180, Segment{Still, 30}, Segment{Scroll, 30}, Segment{Still, 30})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Unix(100, 0)
	at := func(ms int) time.Time { return start.Add(time.Duration(ms) * time.Millisecond) }
	r := &Run{Clip: clip, FPS: 30, Start: start,
		Sent: []Sent{
			{Index: 0, Bytes: 1000, Key: true, QP: 30},
			{Index: 30, Bytes: 500, QP: 20}, {Index: 45, Bytes: 500, QP: 24}, {Index: 59, Bytes: 500},
			{Index: 60, Bytes: 2000, Key: true, QP: 28},
		},
		Result: Result{
			Shown: []Shown{
				{Index: -1, At: at(-500)},
				{Index: 0, At: at(40), Score: Score{PSNR: 40, SSIM: 0.99, WorstTile: 0.95}},
				{Index: 30, At: at(1050), Score: Score{PSNR: 30, SSIM: 0.9, WorstTile: 0.5}},
				{Index: 45, At: at(1550), Score: Score{PSNR: 34, SSIM: 0.94, WorstTile: 0.7}},
				{Index: 60, At: at(2100), Score: Score{PSNR: 42, SSIM: 0.995, WorstTile: 0.97}},
			},
			Requests: []Request{{At: at(500), Why: PLIIdle}, {At: at(1200), Why: PLIStalled}, {At: at(5000), Why: PLIIdle}},
			Stats:    ReceiverStats{Packets: 10, NACKed: 2, Recovered: 1},
		},
	}
	spans := r.Spans()
	if len(spans) != 4 || spans[0].Name != "still 1" || spans[1].Name != "scroll" || spans[2].Name != "still 2" || spans[3].Name != "all" {
		t.Fatalf("spans %+v", spans)
	}
	scroll := spans[1]
	if scroll.SentFrames != 3 || scroll.Shown != 2 || scroll.Keyframes != 0 || scroll.MeanQP != 22 {
		t.Fatalf("scroll %+v", scroll)
	}
	if want := 8.0 * 1500 / 1000; scroll.KbpsSent != want {
		t.Fatalf("scroll sent %f kbit/s, want %f", scroll.KbpsSent, want)
	}
	// The picture first changed at 1000 ms and was first shown at 1050.
	if scroll.MaxFreeze != 500*time.Millisecond || scroll.Lag != 50*time.Millisecond {
		t.Fatalf("freeze %v, lag %v", scroll.MaxFreeze, scroll.Lag)
	}
	if scroll.PSNR != 32 || scroll.MinSSIM != 0.9 || scroll.WorstTile != 0.5 || scroll.Last.PSNR != 34 {
		t.Fatalf("scores %+v", scroll)
	}
	if scroll.Requests[PLIStalled] != 1 || spans[0].Requests[PLIIdle] != 1 || len(spans[2].Requests) != 0 {
		t.Fatalf("requests %v %v %v", spans[0].Requests, scroll.Requests, spans[2].Requests)
	}
	if spans[0].MaxFreeze != 0 {
		t.Fatal("a still span reports a freeze")
	}
	if got := r.Requests(); got[PLIIdle] != 2 || got[PLIStalled] != 1 {
		t.Fatalf("Requests %v", got)
	}
	table := Table(spans)
	for _, want := range []string{"span", "still 1", "scroll", "all", "idle=1", "stalled on loss=1"} {
		if !strings.Contains(table, want) {
			t.Errorf("table lacks %q:\n%s", want, table)
		}
	}
	if s := r.Summary(); !strings.Contains(s, "NACKed 2, recovered 1") || !strings.Contains(s, "idle=2") {
		t.Errorf("summary %q", s)
	}
}

func TestLinkNames(t *testing.T) {
	for _, tc := range []struct {
		l    Link
		want string
	}{
		{Link{Name: "fast"}, "fast"},
		{Link{Delay: 10 * time.Millisecond}, "unshaped, 20ms RTT, 0.0% loss"},
		{Link{Rate: 4_000_000, Queue: 100 * time.Millisecond, Delay: 30 * time.Millisecond, Loss: 0.01},
			"4 Mbit/s 100ms queue, 60ms RTT, 1.0% loss"},
	} {
		if got := tc.l.String(); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}

func TestLinkBuilds(t *testing.T) {
	for _, l := range []Link{{}, {Rate: 1_000_000, Queue: 50 * time.Millisecond, Delay: time.Millisecond, Loss: 0.5}} {
		n, err := l.Build()
		if err != nil {
			t.Fatal(err)
		}
		if (n.TBF != nil) != (l.Rate > 0) {
			t.Errorf("%v: TBF %v", l, n.TBF)
		}
		n.Close()
	}
}
