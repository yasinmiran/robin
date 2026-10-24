package peer

import (
	"math"
	"time"
)

// Bulk pacing. Bulk is invisible to GCC, which sees RTP only, so it keeps
// under the target: what media leaves of it, and never less than
// bulkFloor of it, so a transfer still moves while the screen fills the
// link. A burst is bulkBurst of that rate.
const (
	bulkFloor = 0.25
	bulkBurst = 20 * time.Millisecond
)

// bulkRate is what bulk may send now, in bytes per second, with e.mu held.
func (e *estimator) bulkRate(now time.Time) float64 {
	target := float64(e.target) / 8
	return max(target-e.media.at(now), bulkFloor*target)
}

// bulkChunk is how much a bulk sender should send at once, between min
// and max bytes.
func (e *estimator) bulkChunk(minBytes, maxBytes int) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return min(max(int(e.bulkRate(e.now())*bulkBurst.Seconds()), minBytes), maxBytes)
}

// reserveBulk takes n bytes of the bulk allowance and returns how long the
// sender waits before sending them: transfers at once share the allowance
// in the order they ask.
func (e *estimator) reserveBulk(n int) time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	rate := e.bulkRate(now)
	burst := max(rate*bulkBurst.Seconds(), float64(n))
	if e.bulkAt.IsZero() {
		e.bulk = burst
	} else {
		e.bulk = min(e.bulk+rate*now.Sub(e.bulkAt).Seconds(), burst)
	}
	e.bulkAt = now
	e.bulk -= float64(n)
	if e.bulk >= 0 || rate <= 0 {
		return 0
	}
	return time.Duration(-e.bulk / rate * float64(time.Second))
}

// ewma is a rate over roughly the last ewmaTau, in units per second.
type ewma struct {
	rate float64
	last time.Time
}

const ewmaTau = 500 * time.Millisecond

func (r *ewma) decay(now time.Time) {
	if !r.last.IsZero() && now.After(r.last) {
		r.rate *= math.Exp(-now.Sub(r.last).Seconds() / ewmaTau.Seconds())
	}
	if now.After(r.last) {
		r.last = now
	}
}

func (r *ewma) add(now time.Time, n float64) {
	r.decay(now)
	r.rate += n / ewmaTau.Seconds()
}

func (r *ewma) at(now time.Time) float64 {
	r.decay(now)
	return r.rate
}
