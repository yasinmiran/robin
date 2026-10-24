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
			chunk := e.bulkChunk()
			if want := int(tc.wantBytes * bulkBurst.Seconds()); chunk < want*9/10 || chunk > want*11/10 {
				t.Errorf("chunk %d bytes, want about %d", chunk, want)
			}
			// A second of chunks, asked for at once, is due over about a
			// second at the bulk rate, less the burst.
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
			chunk = e.bulkChunk()
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
	if got := e.bulkChunk(); got != 4<<10 {
		t.Errorf("chunk at 1 Mbit/s %d bytes, want the 4 KiB floor", got)
	}
	e.target = 100_000_000
	if got := e.bulkChunk(); got != 64<<10 {
		t.Errorf("chunk at 100 Mbit/s %d bytes, want the 64 KiB ceiling", got)
	}
}

// TestEstimatorSharesBulkInTheOrderAsked: two transfers at once, each
// asking for its next chunk once the last is due, share one allowance,
// half each, where an allowance each would send twice the bulk rate.
func TestEstimatorSharesBulkInTheOrderAsked(t *testing.T) {
	now := time.Unix(0, 0)
	start := now
	e, _ := testEstimator(&now, 8_000_000)
	const rate = 1_000_000 // bytes per second, all of the target on a still screen
	chunk := e.bulkChunk()
	var due [2]time.Time
	var sent [2]int
	for {
		i := 0
		if due[1].Before(due[0]) {
			i = 1
		}
		if due[i].Sub(start) >= time.Second {
			break
		}
		if due[i].After(now) {
			now = due[i]
		}
		due[i] = now.Add(e.reserveBulk(chunk))
		sent[i] += chunk
	}
	total := sent[0] + sent[1]
	if want := rate + chunk; total < want*95/100 || total > want*105/100 {
		t.Errorf("%d bytes went out in a second, want about %d", total, want)
	}
	for i, n := range sent {
		if n < total*45/100 || n > total*55/100 {
			t.Errorf("transfer %d sent %d of %d bytes, want about half", i, n, total)
		}
	}
}
