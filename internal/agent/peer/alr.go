package peer

import "time"

// The application-limited region detector's constants are libwebrtc's
// (AlrDetector): a budget fills at alrUsage times the target, up to
// alrWindow of it, and every byte sent spends from it. Unlike libwebrtc's,
// the budget does not go below empty, so a sender that filled the target
// and then went quiet is limited 0.4 s later instead of 0.9 s. GCC raises
// its target while the rate it hears falls: in one run over the quality
// matrix's tight link the 0.9 s raised it by 16%, and the motion after
// the spell overflowed the link.
const (
	// alrUsage of the target is what a sender has to keep up to count as
	// testing the link.
	alrUsage = 0.65
	// alrStart and alrStop are how full the budget is, as a share of its
	// size, when a spell starts and when it ends.
	alrStart = 0.80
	alrStop  = 0.50
	// alrWindow at alrUsage times the target is the budget's size.
	alrWindow = 500 * time.Millisecond
	// alrStopAfter is how long the budget has to stay under alrStop for a
	// spell to end. A burst can empty the budget, and a refresh of the
	// still picture does when the target is low, but the budget then
	// refills past alrStop within half of alrWindow: only a sender that
	// keeps filling the target holds it under for longer.
	alrStopAfter = 300 * time.Millisecond
)

// alrDetector tells when the sender is application limited: sending well
// under the congestion target because it has little to send, as a still
// screen does, rather than because the link holds it back. Over such a
// spell the delay and the rate the browser reports say nothing about what
// the link carries. A sender that goes quiet is limited 0.4 s later, and
// one that fills the target again after a long spell stops being limited
// 0.76 s after it starts.
type alrDetector struct {
	rate    float64 // bytes per second the budget fills at
	budget  float64 // bytes not spent
	last    time.Time
	limited bool
	// under is when the budget of a spell last fell under alrStop; zero
	// while it is over.
	under time.Time
}

// setTarget follows the congestion target, in bits per second.
func (d *alrDetector) setTarget(bps int) {
	d.rate = alrUsage * float64(max(bps, 0)) / 8
	d.budget = min(d.budget, d.size())
}

func (d *alrDetector) size() float64 { return d.rate * alrWindow.Seconds() }

// sent spends n bytes sent at now, and reports whether the sender is
// limited.
func (d *alrDetector) sent(now time.Time, n int) bool {
	d.advance(now)
	d.budget = max(d.budget-float64(n), 0)
	return d.check(now)
}

// at reports whether the sender is limited at now, with nothing sent since
// it last heard.
func (d *alrDetector) at(now time.Time) bool {
	d.advance(now)
	return d.check(now)
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

// check samples the budget. Between two samples it only fills, so one
// under alrStop has been under it since the sample before.
func (d *alrDetector) check(now time.Time) bool {
	size := d.size()
	if size <= 0 {
		return d.limited
	}
	fill := d.budget / size
	switch {
	case !d.limited:
		d.limited = fill > alrStart
	case fill >= alrStop:
		d.under = time.Time{}
	case d.under.IsZero():
		d.under = now
	case now.Sub(d.under) >= alrStopAfter:
		d.limited, d.under = false, time.Time{}
	}
	return d.limited
}
