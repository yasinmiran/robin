//go:build linux

package agent

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/yasinmiran/remotejump/internal/agent/peer"
	"github.com/yasinmiran/remotejump/internal/agent/x11"
	"github.com/yasinmiran/remotejump/internal/agent/yuv"
)

// Encoder turns I420 frames into H.264 access units in Annex-B form.
type Encoder interface {
	// Encode compresses one frame taken at ts since the stream started. It
	// may return no data when the rate controller skips the frame.
	Encode(img *yuv.I420, ts time.Duration, keyframe bool) ([]byte, error)
	SetBitrate(bps int) error
	Close() error
}

// NewEncoderFunc builds an encoder for one frame size. The session recreates
// the encoder whenever the display is resized.
type NewEncoderFunc func(width, height, fps, bitrate int) (Encoder, error)

// minKeyframeGap bounds how often loss reports can force an IDR frame: each
// one is large, and a burst of them would deepen the congestion that caused
// the loss.
const minKeyframeGap = 300 * time.Millisecond

// stream captures, encodes and sends frames at up to fps while a peer is
// attached. An idle screen costs one cheap DAMAGE check per tick.
type stream struct {
	x          *x11.Conn
	fps        int
	newEncoder NewEncoderFunc
	log        *slog.Logger

	peer     atomic.Pointer[peer.Peer]
	keyframe atomic.Bool
	bitrate  atomic.Int64
}

func newStream(x *x11.Conn, fps int, newEncoder NewEncoderFunc, log *slog.Logger) *stream {
	s := &stream{x: x, fps: fps, newEncoder: newEncoder, log: log}
	s.bitrate.Store(startBitrate)
	return s
}

func (s *stream) setPeer(p *peer.Peer) {
	s.peer.Store(p)
	s.keyframe.Store(true)
}

func (s *stream) requestKeyframe() { s.keyframe.Store(true) }

func (s *stream) setBitrate(bps int) { s.bitrate.Store(int64(bps)) }

func (s *stream) run(ctx context.Context) {
	interval := time.Second / time.Duration(s.fps)
	tick := time.NewTicker(interval)
	defer tick.Stop()

	var (
		enc Encoder
		// stale means rate control dropped the latest change, so the next tick
		// must encode the screen again even if nothing new was drawn.
		stale          bool
		img            *yuv.I420
		appliedBitrate int64
		began          = time.Now()
		lastWrite      time.Time
		lastKey        time.Time
	)
	closeEncoder := func() {
		if enc != nil {
			enc.Close()
			enc = nil
		}
	}
	defer closeEncoder()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		p := s.peer.Load()
		if p == nil {
			closeEncoder()
			continue
		}
		now := time.Now()
		force := enc == nil || (s.keyframe.Load() && now.Sub(lastKey) >= minKeyframeGap)
		f, changed, err := s.x.Capture(force || stale)
		if err != nil {
			s.log.Warn("capture", "err", err)
			continue
		}
		if !changed && !force && !stale {
			continue
		}
		w, h := f.Width&^1, f.Height&^1
		if enc == nil || img.Width != w || img.Height != h {
			closeEncoder()
			br := s.bitrate.Load()
			if enc, err = s.newEncoder(w, h, s.fps, int(br)); err != nil {
				s.log.Error("encoder", "w", w, "h", h, "err", err)
				continue
			}
			appliedBitrate = br
			img = yuv.NewI420(w, h)
			force = true
		}
		if br := s.bitrate.Load(); br != appliedBitrate {
			if err := enc.SetBitrate(int(br)); err != nil {
				s.log.Warn("set bitrate", "bps", br, "err", err)
			}
			appliedBitrate = br
		}
		yuv.FromBGRX(img, f.Pix, f.Stride)
		au, err := enc.Encode(img, now.Sub(began), force)
		if err != nil {
			s.log.Warn("encode", "err", err)
			continue
		}
		if force {
			s.keyframe.Store(false)
			lastKey = now
			s.log.Info("keyframe", "w", w, "h", h, "bytes", len(au), "kbps", appliedBitrate/1000)
		}
		stale = len(au) == 0
		if stale {
			continue
		}
		// The sample duration advances the RTP clock, so it must be the real
		// gap since the previous frame, not the nominal frame interval.
		d := interval
		if !lastWrite.IsZero() {
			d = min(max(now.Sub(lastWrite), interval/2), time.Second)
		}
		if err := p.WriteFrame(au, d); err != nil {
			s.log.Debug("write frame", "err", err)
		}
		lastWrite = now
	}
}
