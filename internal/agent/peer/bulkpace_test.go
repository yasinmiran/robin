package peer

import (
	"testing"
	"time"
)

// TestEstimatorPacesBulkUnderTheTarget: bulk gets what media leaves of
// the target, and a quarter of it when media fills it.
func TestEstimatorPacesBulkUnderTheTarget(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mediaBps  int
		wantBytes float64 // per second
	}{
		{"still", 0, 1_000_000},
		{"half", 4_000_000, 500_000},
		{"full", 8_000_000, 250_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(0, 0)
			e, f := testEstimator(&now, 8_000_000)
			feed(t, e, f, &now, tc.mediaBps, 3*time.Second, 0)
			chunk := e.bulkChunk(4<<10, 64<<10)
			if want := int(tc.wantBytes * bulkBurst.Seconds()); chunk < want*9/10 || chunk > want*11/10 {
				t.Errorf("chunk %d bytes, want about %d", chunk, want)
			}
			// A second of chunks, asked for at once by two senders, is due
			// over about a second at the bulk rate, less the burst.
			var last time.Duration
			n := int(tc.wantBytes) / chunk
			for range n {
				last = e.reserveBulk(chunk)
			}
			want := time.Second - bulkBurst
			if last < want*85/100 || last > want*115/100 {
				t.Errorf("the last of %d chunks is due in %v, want about %v", n, last, want)
			}
			// Waiting it out leaves no debt, and an idle stretch banks no
			// more than a burst.
			now = now.Add(10 * time.Second)
			chunk = e.bulkChunk(4<<10, 64<<10)
			if d := e.reserveBulk(chunk); d != 0 {
				t.Errorf("after a pause a chunk waits %v", d)
			}
			if d := e.reserveBulk(2 * chunk); d == 0 {
				t.Error("two more chunks after a pause go out at once: more than a burst was banked")
			}
		})
	}
	now := time.Unix(0, 0)
	e, _ := testEstimator(&now, 1_000_000)
	if got := e.bulkChunk(4<<10, 64<<10); got != 4<<10 {
		t.Errorf("chunk at 1 Mbit/s %d bytes, want the 4 KiB floor", got)
	}
	e.target = 100_000_000
	if got := e.bulkChunk(4<<10, 64<<10); got != 64<<10 {
		t.Errorf("chunk at 100 Mbit/s %d bytes, want the 64 KiB ceiling", got)
	}
}
