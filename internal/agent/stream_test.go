//go:build linux

package agent

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agent/desktop"
	"github.com/yasinmiran/robin/internal/agent/peer"
	"github.com/yasinmiran/robin/internal/agent/yuv"
	"github.com/yasinmiran/robin/internal/protocol"
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

// stillDesktop is a desktop whose picture never changes.
type stillDesktop struct {
	fakeDesktop
}

func (*stillDesktop) Capture(force bool) (*desktop.Frame, bool, error) {
	return &desktop.Frame{Width: 64, Height: 64, Stride: 4 * 64, Pix: make([]byte, 4*64*64), Format: desktop.BGRX}, force, nil
}

// subscribe adds p to the stream as the one subscriber that takes tiles,
// at the start bitrate, as the owner's page is.
func (s *stream) subscribe(p *peer.Peer) *subscriber {
	sub := newSubscriber(startBitrate, true)
	sub.peer = p
	s.add(sub)
	return sub
}

// countingEncoder counts its encodes and returns a small access unit
// for each.
type countingEncoder struct{ n *atomic.Int64 }

func (e countingEncoder) Encode(*yuv.I420, time.Duration, bool) ([]byte, error) {
	e.n.Add(1)
	return []byte{0, 0, 0, 1, 0x65, 0x88, 0x84, 0x21}, nil
}
func (countingEncoder) SetBitrate(int) error { return nil }
func (countingEncoder) Close() error         { return nil }

// TestStreamRefreshesAStillScreen checks that a still screen is encoded
// again every refresh period, and only once, as the first keyframe, with
// refresh off.
func TestStreamRefreshesAStillScreen(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refresh time.Duration
	}{
		{"every 20 ms", 20 * time.Millisecond},
		{"off", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			var encodes atomic.Int64
			open := func(int, int, int, int) (Encoder, error) { return countingEncoder{&encodes}, nil }
			s := mustStream(newStream(&stillDesktop{}, 100, 64, 64, open, log))
			s.refresh = tc.refresh
			p, err := peer.New(peer.Config{
				MinBitrate: minBitrate, StartBitrate: startBitrate, MaxBitrate: protocol.DefaultMaxKbps * 1000, Log: log,
				Video: s.video,
			}, peer.Handlers{})
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			s.subscribe(p)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				s.run(ctx)
				close(done)
			}()
			defer func() {
				cancel()
				<-done
			}()
			reached := func(n int64) bool {
				for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
					if encodes.Load() >= n {
						return true
					}
				}
				return false
			}
			if tc.refresh > 0 {
				if !reached(3) {
					t.Fatalf("%d encodes of a still screen in 5 s", encodes.Load())
				}
				return
			}
			if !reached(1) {
				t.Fatal("the first picture was never encoded")
			}
			time.Sleep(200 * time.Millisecond)
			if n := encodes.Load(); n != 1 {
				t.Fatalf("%d encodes of a still screen with refresh off, want 1", n)
			}
		})
	}
}
