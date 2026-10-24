//go:build linux

package agent

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/yasinmiran/robin/internal/agent/desktop"
	"github.com/yasinmiran/robin/internal/agent/peer"
	"github.com/yasinmiran/robin/internal/agent/yuv"
	"github.com/yasinmiran/robin/internal/protocol"
)

// Encoder turns I420 frames into H.264 access units in Annex-B form.
type Encoder interface {
	// Encode compresses one frame taken at ts since the stream started. It
	// may return no data when the rate controller skips the frame.
	Encode(img *yuv.I420, ts time.Duration, keyframe bool) ([]byte, error)
	SetBitrate(bps int) error
	Close() error
}

// qpReporter is an Encoder that can say which quantiser its last frame
// used; QP returns -1 when it cannot.
type qpReporter interface{ QP() int }

// NewEncoderFunc builds an encoder for one frame size. The session recreates
// the encoder whenever the display is resized.
type NewEncoderFunc func(width, height, fps, bitrate int) (Encoder, error)

// minKeyframeGap bounds how often loss reports can force an IDR frame: each
// one is large, and a burst of them would deepen the congestion that caused
// the loss.
const minKeyframeGap = 300 * time.Millisecond

// refreshEvery is how long a still screen goes without a frame. Chrome asks
// for a keyframe after 3 s without one, and each such keyframe resets the
// picture to whatever quality rate control gave it; encoding the unchanged
// picture again instead costs a few hundred bytes and lets rate control
// spend what it has spare sharpening it.
const refreshEvery = 500 * time.Millisecond

// stream captures, encodes and sends frames at up to fps while a peer is
// attached. An idle screen costs one cheap DAMAGE check per tick and an
// encode of the unchanged picture every refreshEvery.
type stream struct {
	desk desktop.Desktop
	fps  int
	// maxW and maxH cap the encoded size. A spawned display never exceeds
	// them; an attached one can, and its frames are scaled down to fit.
	maxW, maxH int
	newEncoder NewEncoderFunc
	log        *slog.Logger
	// refresh is refreshEvery, or 0 to send nothing while the screen is
	// still.
	refresh time.Duration

	peer     atomic.Pointer[peer.Peer]
	keyframe atomic.Bool
	bitrate  atomic.Int64
	// scale is how frames are shrunk on their way out, nil while they go
	// out at desktop size.
	scale atomic.Pointer[scale]
	// tilesWanted is the page's latest ControlTiles, nil until it sends one.
	tilesWanted atomic.Pointer[tileRequest]
}

// tileRequest is one ControlTiles from the page. Each is a value of its own,
// so a request starts the tiles afresh even when the one before it, the
// other way, came and went between two frames.
type tileRequest struct{ on bool }

// scale is a desktop size and the smaller one its frames are encoded at.
type scale struct{ deskW, deskH, w, h int }

// toDesktop maps the position in a pointer event, which the page reports in
// video pixels, to the desktop pixel at the centre of that video pixel.
func (s *stream) toDesktop(m protocol.Control) protocol.Control {
	sc := s.scale.Load()
	if sc == nil || (m.T != protocol.ControlMove && m.T != protocol.ControlButton) {
		return m
	}
	m.X = min((2*m.X+1)*sc.deskW/(2*sc.w), sc.deskW-1)
	m.Y = min((2*m.Y+1)*sc.deskH/(2*sc.h), sc.deskH-1)
	return m
}

func newStream(desk desktop.Desktop, fps, maxW, maxH int, newEncoder NewEncoderFunc, log *slog.Logger) *stream {
	s := &stream{desk: desk, fps: fps, maxW: maxW, maxH: maxH, newEncoder: newEncoder, log: log, refresh: refreshEvery}
	s.bitrate.Store(startBitrate)
	return s
}

func (s *stream) setPeer(p *peer.Peer) {
	s.tilesWanted.Store(nil)
	s.peer.Store(p)
	s.keyframe.Store(true)
}

// wantTiles applies the page's ControlTiles.
func (s *stream) wantTiles(on bool) { s.tilesWanted.Store(&tileRequest{on: on}) }

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
		scaler         *yuv.Scaler
		appliedBitrate int64
		began          = time.Now()
		lastWrite      time.Time
		lastKey        time.Time
		// lastClock is when the RTP clock last moved: a frame written, or
		// one spent unwritten.
		lastClock time.Time
		tracker   = newTileTracker(s.log)
		setup     tileSetup
	)
	closeEncoder := func() {
		if enc != nil {
			enc.Close()
			enc = nil
		}
	}
	defer closeEncoder()
	defer tracker.close()

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
		refresh := s.refresh > 0 && !lastWrite.IsZero() && now.Sub(lastWrite) >= s.refresh
		f, changed, err := s.desk.Capture(force || stale || refresh)
		if err != nil {
			s.log.Warn("capture", "err", err)
			continue
		}
		withTiles := setup.apply(tracker, p, s.tilesWanted.Load(), f)
		if !changed && !force && !stale && !refresh {
			if withTiles {
				tracker.cut(p, now)
			}
			continue
		}
		// The sample duration advances the RTP clock, so it must be the real
		// gap since the previous frame, not the nominal frame interval.
		d := interval
		if !lastClock.IsZero() {
			d = min(max(now.Sub(lastClock), interval/2), time.Second)
		}
		// The manifest goes out before the frame is converted and encoded, a
		// head start on the frame's packets. A frame that then never goes
		// out still spends its timestamp, so no later frame shares it and
		// a manifest the page holds always covers the whole frame.
		rtp, manifest := uint32(0), false
		if withTiles {
			tracker.update(f, now)
			if rtp, manifest = p.NextFrameRTP(); manifest {
				if err := p.SendControl(tracker.manifest(rtp)); err != nil {
					// The page must never see a later manifest without
					// this one: a new grid drops whatever it holds.
					s.log.Debug("tile manifest", "err", err)
					tracker.reset()
					manifest = false
				}
			}
		}
		skipTimestamp := func() {
			if manifest {
				p.WriteFrame(nil, d)
				lastClock = now
			}
		}
		w, h := f.Width&^1, f.Height&^1
		// An I420 desktop already delivers frames at the size it was asked for.
		scaled := f.Format == desktop.BGRX && (f.Width > s.maxW || f.Height > s.maxH)
		if scaled {
			w, h = fit(f.Width, f.Height, s.maxW, s.maxH)
		}
		if enc == nil || img.Width != w || img.Height != h {
			closeEncoder()
			br := s.bitrate.Load()
			if enc, err = s.newEncoder(w, h, s.fps, int(br)); err != nil {
				s.log.Error("encoder", "w", w, "h", h, "err", err)
				skipTimestamp()
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
		if !scaled && scaler != nil {
			scaler = nil
			s.scale.Store(nil)
		}
		switch {
		case f.Format == desktop.I420:
			yuv.FromI420(img, f.Pix, f.Stride, f.Height)
		case scaled:
			if scaler == nil || !scaler.Fits(f.Width, f.Height, w, h) {
				scaler = yuv.NewScaler(f.Width, f.Height, w, h)
				s.scale.Store(&scale{f.Width, f.Height, w, h})
			}
			scaler.Convert(img, f.Pix, f.Stride)
		default:
			yuv.FromBGRX(img, f.Pix, f.Stride)
		}
		au, err := enc.Encode(img, now.Sub(began), force)
		if err != nil {
			s.log.Warn("encode", "err", err)
			skipTimestamp()
			continue
		}
		if force {
			s.keyframe.Store(false)
			lastKey = now
			attrs := []any{"w", w, "h", h, "bytes", len(au), "kbps", appliedBitrate / 1000}
			if q, ok := enc.(qpReporter); ok {
				attrs = append(attrs, "qp", q.QP())
			}
			s.log.Info("keyframe", attrs...)
		}
		stale = len(au) == 0
		if stale {
			skipTimestamp()
			continue
		}
		if err := p.WriteFrame(au, d); err != nil {
			s.log.Debug("write frame", "err", err)
		}
		lastWrite, lastClock = now, now
		if manifest {
			tracker.wrote(rtp)
			tracker.cut(p, now)
		}
	}
}

// tileSetup is what the stream's tiles were last set up for: the peer, the
// request of that page, and whether it has been answered.
type tileSetup struct {
	peer     *peer.Peer
	req      *tileRequest
	answered bool
}

// apply follows the page's latest request for tiles: a new peer or request
// starts the tiles afresh, and the page hears once whether tiles are on:
// off when it asked them off, and when it asked them on, on or why this
// desktop cannot have them. The answer to an off request comes after every
// manifest, so none follows it. apply reports whether f gets tiles.
func (st *tileSetup) apply(t *tileTracker, p *peer.Peer, req *tileRequest, f *desktop.Frame) bool {
	if p != st.peer || req != st.req {
		t.reset()
		*st = tileSetup{peer: p, req: req}
	}
	if req == nil || (req.on && f == nil) {
		return false
	}
	ok := req.on && f.Format == desktop.BGRX
	if !st.answered {
		answer := protocol.Tiles{On: ok}
		if req.on && !ok {
			answer.Unsupported = noTilesReason
		}
		st.answered = p.SendControl(protocol.Control{T: protocol.ControlTiles, Tiles: &answer}) == nil
	}
	return ok
}
