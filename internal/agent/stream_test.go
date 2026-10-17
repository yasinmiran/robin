//go:build linux

package agent

import (
	"testing"

	"github.com/yasinmiran/remotejump/internal/protocol"
)

func TestToDesktop(t *testing.T) {
	s := &stream{}
	move := protocol.Control{T: protocol.ControlMove, X: 640, Y: 360}
	if got := s.toDesktop(move); got != move {
		t.Errorf("unscaled: %+v became %+v", move, got)
	}

	for _, sc := range []scale{{2560, 1440, 1920, 1080}, {5120, 2640, 1920, 990}, {3840, 2160, 1920, 1080}, {1921, 1081, 1920, 1080}} {
		s.scale.Store(&sc)
		// Every video pixel lands inside the desktop pixels it shows.
		for vx := range sc.w {
			got := s.toDesktop(protocol.Control{T: protocol.ControlButton, X: vx}).X
			if lo, hi := vx*sc.deskW/sc.w, ((vx+1)*sc.deskW+sc.w-1)/sc.w; got < lo || got >= hi {
				t.Fatalf("%+v: video x %d -> %d, outside [%d,%d)", sc, vx, got, lo, hi)
			}
		}
		for vy := range sc.h {
			got := s.toDesktop(protocol.Control{T: protocol.ControlMove, Y: vy}).Y
			if lo, hi := vy*sc.deskH/sc.h, ((vy+1)*sc.deskH+sc.h-1)/sc.h; got < lo || got >= hi {
				t.Fatalf("%+v: video y %d -> %d, outside [%d,%d)", sc, vy, got, lo, hi)
			}
		}
	}

	s.scale.Store(&scale{2560, 1440, 1920, 1080})
	if got := s.toDesktop(move); got.X != 854 || got.Y != 480 {
		t.Errorf("640,360 on 1920x1080 video of 2560x1440 -> %d,%d, want 854,480", got.X, got.Y)
	}
	key := protocol.Control{T: protocol.ControlKey, Key: "a", X: 5, Y: 5}
	if got := s.toDesktop(key); got != key {
		t.Errorf("a key event was remapped: %+v", got)
	}
}
