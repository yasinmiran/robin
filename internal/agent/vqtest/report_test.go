package vqtest

import (
	"encoding/binary"
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
			{Index: 30, Bytes: 500, QP: 20, Target: 1_000_000}, {Index: 45, Bytes: 500, QP: 24, Target: 2_000_000},
			{Index: 59, Bytes: 500, QP: -1, Target: 3_000_000},
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
	if scroll.SentFrames != 3 || scroll.Shown != 2 || scroll.Keyframes != 0 || scroll.MeanQP != 22 || scroll.KbpsTarget != 2000 {
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
	if s := r.Summary(); !strings.Contains(s, "NACKed 2, recovered 1") || !strings.Contains(s, "idle=2") || strings.Contains(s, "sound") {
		t.Errorf("summary %q", s)
	}
	// The first half of the scroll, counted as motion.
	if w := r.Window("half", 30, 45); w.Name != "half" || w.SentFrames != 1 || w.Shown != 1 || w.MaxFreeze != 50*time.Millisecond {
		t.Fatalf("window %+v", w)
	}
}

func TestSoundDelays(t *testing.T) {
	start := time.Unix(100, 0)
	r := &Run{}
	for i := range 100 {
		sent := start.Add(time.Duration(i) * 20 * time.Millisecond)
		r.Result.Sound = append(r.Result.Sound, Sound{Sent: sent, At: sent.Add(time.Duration(10+i) * time.Millisecond)})
	}
	d := r.SoundDelays()
	if d.N != 100 || d.Median != 60*time.Millisecond || d.P99 != 109*time.Millisecond || d.Max != 109*time.Millisecond {
		t.Fatalf("delays %+v", d)
	}
	if s := r.Summary(); !strings.Contains(s, "sound 100 packets, one way 60ms median, 109ms at the 99th percentile, 109ms at most") {
		t.Errorf("summary %q", s)
	}
	if (&Run{}).SoundDelays() != (Delays{}) {
		t.Error("delays without sound")
	}
}

func TestSoundPayloadCarriesItsSendTime(t *testing.T) {
	sent := time.Unix(1700000000, 123456789)
	b := SoundPayload(240, sent)
	if len(b) != 240 || time.Unix(0, int64(binary.BigEndian.Uint64(b))) != sent {
		t.Fatalf("payload of %d bytes starting %x", len(b), b[:8])
	}
	if len(SoundPayload(2, sent)) != 8 {
		t.Fatal("a short payload has no room for the time")
	}
}
