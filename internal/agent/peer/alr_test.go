package peer

import (
	"testing"
	"time"
)

// alrRun sends at bps against a target of targetBps in 10 ms steps for d,
// from *now, and returns how long into it the detector first said limited
// and first said not limited, or -1 for either it never said.
func alrRun(d *alrDetector, now *time.Time, bps int, dur time.Duration) (limitedAt, unlimitedAt time.Duration) {
	limitedAt, unlimitedAt = -1, -1
	const step = 10 * time.Millisecond
	for t := time.Duration(0); t < dur; t += step {
		*now = now.Add(step)
		var limited bool
		if n := bps / 8 * int(step) / int(time.Second); n > 0 {
			limited = d.sent(*now, n)
		} else {
			limited = d.at(*now)
		}
		if limited && limitedAt < 0 {
			limitedAt = t + step
		}
		if !limited && unlimitedAt < 0 {
			unlimitedAt = t + step
		}
	}
	return limitedAt, unlimitedAt
}

func TestALRStartsOnceTheSenderGoesQuiet(t *testing.T) {
	now := time.Unix(0, 0)
	var d alrDetector
	d.setTarget(2_000_000)
	if limited, _ := alrRun(&d, &now, 2_000_000, 3*time.Second); limited >= 0 {
		t.Fatalf("limited %v into sending at the target", limited)
	}
	limited, _ := alrRun(&d, &now, 0, 3*time.Second)
	if limited < 850*time.Millisecond || limited > 950*time.Millisecond {
		t.Fatalf("limited %v after the sender went quiet, want about 0.9 s", limited)
	}
}

func TestALREndsSoonAfterTheSenderFillsTheTarget(t *testing.T) {
	now := time.Unix(0, 0)
	var d alrDetector
	d.setTarget(2_000_000)
	if limited, _ := alrRun(&d, &now, 0, 60*time.Second); limited < 0 {
		t.Fatal("a minute of silence is not limited")
	}
	_, unlimited := alrRun(&d, &now, 2_000_000, 2*time.Second)
	if unlimited < 400*time.Millisecond || unlimited > 500*time.Millisecond {
		t.Fatalf("limited until %v into filling the target, want about 0.46 s", unlimited)
	}
}

// A sender keeping above alrUsage of the target is filling it; one well
// under it, typing on a still screen, is not.
func TestALRFollowsTheShareOfTheTargetSent(t *testing.T) {
	for _, tc := range []struct {
		share   float64
		limited bool
	}{
		{0.9, false}, {0.7, false}, {0.5, true}, {0.1, true},
	} {
		now := time.Unix(0, 0)
		var d alrDetector
		d.setTarget(4_000_000)
		alrRun(&d, &now, 4_000_000, time.Second)
		alrRun(&d, &now, int(tc.share*4_000_000), 10*time.Second)
		if got := d.at(now); got != tc.limited {
			t.Errorf("sending %.0f%% of the target: limited %v, want %v", 100*tc.share, got, tc.limited)
		}
	}
}

// A lower target shrinks the budget with it, so a sender that fills the
// new target is not limited for the old one's sake.
func TestALRBudgetFollowsTheTarget(t *testing.T) {
	now := time.Unix(0, 0)
	var d alrDetector
	d.setTarget(10_000_000)
	alrRun(&d, &now, 0, 5*time.Second)
	if !d.at(now) {
		t.Fatal("silence is not limited")
	}
	d.setTarget(1_000_000)
	if d.budget > d.size() {
		t.Fatalf("budget %.0f past its size %.0f", d.budget, d.size())
	}
	if _, unlimited := alrRun(&d, &now, 1_000_000, 2*time.Second); unlimited < 0 || unlimited > 500*time.Millisecond {
		t.Fatalf("filling the lowered target: limited until %v", unlimited)
	}
}

func TestALRWithoutATargetKeepsItsState(t *testing.T) {
	now := time.Unix(0, 0)
	var d alrDetector
	d.setTarget(0)
	if d.sent(now, 1000) || d.at(now.Add(time.Hour)) {
		t.Fatal("limited with no target")
	}
	// Time that runs backwards neither fills nor empties the budget.
	d.setTarget(1_000_000)
	d.at(now.Add(time.Hour))
	before := d.budget
	d.at(now)
	if d.budget != before {
		t.Fatalf("budget moved from %.0f to %.0f going back in time", before, d.budget)
	}
}
