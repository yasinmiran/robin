//go:build linux

// Package hwenc encodes H.264 on the host's GPU, through VA-API or NVENC.
//
// The agent is a static musl binary and cannot load the host's libva or
// NVIDIA libraries, so the encoder is a host tool run as a subprocess:
// ffmpeg or GStreamer's gst-launch-1.0, fed NV12 frames on stdin and writing
// one framed access unit per frame on stdout. A running tool cannot be asked
// for an IDR or told a new bitrate, so Encoder restarts it for both, which
// costs tens of milliseconds, and between restarts holds the congestion
// controller's target by skipping frames.
package hwenc

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/yasinmiran/remotejump/internal/agent/yuv"
)

// Kind is a family of hardware encoders.
type Kind string

// The kinds the agent offers, named as the CLI's --encoder flag names them.
const (
	VAAPI Kind = "vaapi"
	NVENC Kind = "nvenc"
)

// Opener finds a working route to one kind of hardware encoder, once per
// process, and opens encoders on it.
type Opener struct {
	kind   Kind
	log    *slog.Logger
	routes func() ([]route, error)

	once  sync.Once
	route *route
	err   error
}

// NewOpener returns an Opener for kind. Nothing is checked until the first
// Open.
func NewOpener(kind Kind, log *slog.Logger) *Opener {
	o := &Opener{kind: kind, log: log}
	switch kind {
	case VAAPI:
		o.routes = vaapiRoutes
	case NVENC:
		o.routes = nvencRoutes
	default:
		o.routes = func() ([]route, error) { return nil, fmt.Errorf("unknown hardware encoder %q", kind) }
	}
	return o
}

// minSize is the smallest frame side every supported encoder accepts.
const minSize = 64

// Open returns an encoder for frames of one size. The first call picks the
// route, trying each tool this host has until one encodes a test frame;
// later calls reuse that route, or its error.
func (o *Opener) Open(width, height, fps, bitrate int) (*Encoder, error) {
	if width < minSize || height < minSize || width%2 != 0 || height%2 != 0 {
		return nil, fmt.Errorf("hwenc: bad frame size %dx%d", width, height)
	}
	o.once.Do(o.choose)
	if o.err != nil {
		return nil, o.err
	}
	e := newEncoder(o.route, width, height, fps, bitrate)
	e.log = o.log
	return e, nil
}

// chooseTimeout bounds all the trials together, so a host whose tools hang
// gets the reasons from the agent before the CLI gives up waiting for it.
const chooseTimeout = 40 * time.Second

var hints = map[Kind]string{
	VAAPI: "VA-API encoding needs ffmpeg with h264_vaapi, or gst-launch-1.0 with GStreamer's va or vaapi plugin and gst-plugins-good, " +
		"a VA-API driver that encodes H.264, and a /dev/dri render node this user can open",
	NVENC: "NVENC encoding needs the NVIDIA driver, and ffmpeg with h264_nvenc or gst-launch-1.0 with GStreamer's nvcodec plugin and gst-plugins-good",
}

func (o *Opener) choose() {
	routes, err := o.routes()
	if err != nil {
		o.err = fmt.Errorf("hwenc: no %s encoder: %w\n%s", o.kind, err, hints[o.kind])
		return
	}
	var why []string
	deadline := time.Now().Add(chooseTimeout)
	for i := range routes {
		r := &routes[i]
		start := time.Now()
		if start.After(deadline) {
			why = append(why, fmt.Sprintf("%s: not tried, the others took %v", r.name, chooseTimeout))
			continue
		}
		if err := trial(r, min(trialTimeout, deadline.Sub(start))); err != nil {
			why = append(why, r.name+": "+oneLine(err.Error()))
			continue
		}
		if o.log != nil {
			o.log.Info("hardware encoder", "kind", o.kind, "route", r.name, "trial", time.Since(start).Round(time.Millisecond))
		}
		o.route = r
		return
	}
	o.err = fmt.Errorf("hwenc: no working %s encoder on this host:\n  %s\n%s", o.kind, strings.Join(why, "\n  "), hints[o.kind])
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// Restart policy. A restart yields an IDR and applies the current bitrate.
const (
	// retargetFactor is how far the target must move from the bitrate the
	// tool runs at before a restart applies it. Each restart costs an IDR,
	// so smaller moves are left to the frame budget.
	retargetFactor = 2
	// retargetGap is the least a tool runs before a restart for bitrate.
	retargetGap = 2 * time.Second
	// budgetWindow is how much unspent bitrate the budget banks: the
	// largest burst it lets through after a quiet spell.
	budgetWindow = 500 * time.Millisecond
	// failureBackoff spaces restarts after a tool fails, so a broken host
	// tool is not respawned on every frame.
	failureBackoff = time.Second

	firstFrameTimeout = 10 * time.Second
	frameTimeout      = time.Second
)

var errClosed = errors.New("hwenc: encoder closed")

// Encoder encodes frames of one size through a host tool. It is not safe
// for concurrent use.
type Encoder struct {
	route *route
	log   *slog.Logger // may be nil
	cfg   config
	// bitrate is the congestion controller's target in bits per second.
	bitrate int
	proc    *proc
	budget  budget
	uv      []byte

	failed   bool
	failedAt time.Duration
	closed   bool
}

// newEncoder makes an encoder on r. The tool starts with the first frame.
func newEncoder(r *route, width, height, fps, bitrate int) *Encoder {
	return &Encoder{
		route: r, cfg: config{width: width, height: height, fps: fps}, bitrate: bitrate,
		uv: make([]byte, width*height/2),
	}
}

// Encode compresses img, taken at ts since the stream began. It returns no
// data, as for a frame rate control skipped, while the output runs ahead of
// the target bitrate, but never for a requested keyframe.
func (e *Encoder) Encode(img *yuv.I420, ts time.Duration, keyframe bool) ([]byte, error) {
	if e.closed {
		return nil, errClosed
	}
	if img.Width != e.cfg.width || img.Height != e.cfg.height {
		return nil, fmt.Errorf("hwenc: frame is %dx%d, encoder is %dx%d", img.Width, img.Height, e.cfg.width, e.cfg.height)
	}
	e.budget.refill(ts, e.bitrate)
	if e.proc != nil && !keyframe && !e.budget.ok() {
		return nil, nil
	}
	if e.proc != nil {
		switch {
		case keyframe && e.proc.frames > 0:
			e.stop()
		case e.offTarget() && ts-e.proc.started >= retargetGap:
			if e.log != nil {
				e.log.Info("hardware encoder retarget", "from_kbps", e.proc.bitrate/1000, "to_kbps", e.bitrate/1000)
			}
			e.stop()
		}
	}
	if e.proc == nil {
		if e.failed && ts-e.failedAt < failureBackoff {
			return nil, nil
		}
		c := e.cfg
		c.bitrate = e.bitrate
		p, err := startProc(e.route, c)
		if err != nil {
			e.failed, e.failedAt = true, ts
			return nil, fmt.Errorf("hwenc: %w", err)
		}
		p.started = ts
		e.proc = p
	}

	for i := range img.U {
		e.uv[2*i], e.uv[2*i+1] = img.U[i], img.V[i]
	}
	timeout := frameTimeout
	if e.proc.frames == 0 {
		timeout = firstFrameTimeout
	}
	au, err := e.proc.encode(img.Y, e.uv, time.Now().Add(timeout))
	if err != nil {
		e.stop()
		e.failed, e.failedAt = true, ts
		return nil, fmt.Errorf("hwenc: %s: %w", e.route.name, err)
	}
	e.failed = false
	e.budget.spend(len(au))
	return au, nil
}

// offTarget reports whether the target has moved far enough from the
// running tool's bitrate to restart it.
func (e *Encoder) offTarget() bool {
	running := e.proc.bitrate
	return e.bitrate >= retargetFactor*running || e.bitrate*retargetFactor <= running
}

// SetBitrate records a new target. The frame budget follows it from the
// next frame; the tool follows it when it is next restarted.
func (e *Encoder) SetBitrate(bps int) error {
	e.bitrate = bps
	return nil
}

func (e *Encoder) stop() {
	if e.proc != nil {
		e.proc.kill()
		e.proc = nil
	}
}

// Close stops the tool.
func (e *Encoder) Close() error {
	e.stop()
	e.closed = true
	return nil
}

// budget is a token bucket of bits, filled at the target bitrate and drawn
// down by every access unit sent.
type budget struct {
	bits    float64
	at      time.Duration
	started bool
}

func (b *budget) refill(ts time.Duration, bps int) {
	full := float64(bps) * budgetWindow.Seconds()
	if !b.started {
		b.bits, b.at, b.started = full, ts, true
		return
	}
	b.bits = min(b.bits+float64(bps)*max(ts-b.at, 0).Seconds(), full)
	b.at = ts
}

func (b *budget) spend(bytes int) { b.bits -= 8 * float64(bytes) }

func (b *budget) ok() bool { return b.bits >= 0 }
