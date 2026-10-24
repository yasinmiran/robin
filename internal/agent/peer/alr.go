package peer

import "time"

// The application-limited region detector's constants are libwebrtc's
// (AlrDetector): a budget fills at alrUsage times the target, up to
// alrWindow of it, and every byte sent spends from it.
const (
	// alrUsage of the target is what a sender has to keep up to count as
	// testing the link.
	alrUsage = 0.65
	// alrStart and alrStop are how full the budget is, as a share of its
	// size, when a spell starts and when it ends.
	alrStart = 0.80
	alrStop  = 0.50
	// alrWindow at alrUsage times the target is the budget's size, and
	// as far below empty as it goes.
	alrWindow = 500 * time.Millisecond
)

// alrDetector tells when the sender is application limited: sending well
// under the congestion target because it has little to send, as a still
// screen does, rather than because the link holds it back. Over such a
// spell the delay and the rate the browser reports say nothing about what
// the link carries. A sender that goes quiet after filling the target is
// limited 0.9 s later, and one that fills it again after a long spell
// stops being limited 0.46 s after it starts.
type alrDetector struct {
	rate    float64 // bytes per second the budget fills at
	budget  float64 // bytes not spent; negative when overspent
	last    time.Time
	limited bool
}

// setTarget follows the congestion target, in bits per second.
func (d *alrDetector) setTarget(bps int) {
	d.rate = alrUsage * float64(max(bps, 0)) / 8
	size := d.size()
	d.budget = min(max(d.budget, -size), size)
}

func (d *alrDetector) size() float64 { return d.rate * alrWindow.Seconds() }

// sent spends n bytes sent at now, and reports whether the sender is
// limited.
func (d *alrDetector) sent(now time.Time, n int) bool {
	d.advance(now)
	d.budget = max(d.budget-float64(n), -d.size())
	return d.check()
}

// at reports whether the sender is limited at now, with nothing sent since
// it last heard.
func (d *alrDetector) at(now time.Time) bool {
	d.advance(now)
	return d.check()
}

func (d *alrDetector) advance(now time.Time) {
	if d.last.IsZero() {
		d.last = now
		return
	}
	if dt := now.Sub(d.last); dt > 0 {
		d.budget = min(d.budget+d.rate*dt.Seconds(), d.size())
		d.last = now
	}
}

func (d *alrDetector) check() bool {
	size := d.size()
	if size <= 0 {
		return d.limited
	}
	switch fill := d.budget / size; {
	case !d.limited && fill > alrStart:
		d.limited = true
	case d.limited && fill < alrStop:
		d.limited = false
	}
	return d.limited
}
