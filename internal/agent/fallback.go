//go:build linux

package agent

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/yasinmiran/robin/internal/agent/yuv"
	"github.com/yasinmiran/robin/internal/protocol"
)

// trialWidth and trialHeight size the frame a new session encodes before
// the desktop comes up.
const trialWidth, trialHeight = 320, 240

// chooseEncoder returns the encoder the session uses. A hardware encoder
// rests on tools installed on the host, so it is tried on a small frame
// now, not when the browser's first frame is due. A failure ends the
// session when the user named the encoder; when the CLI picked it
// (Start.EncoderFallback), software takes over instead.
func chooseEncoder(start protocol.Start, encoders map[string]NewEncoderFunc, log *slog.Logger) (*videoEncoder, error) {
	name := start.Encoder
	if name == "" {
		name = protocol.EncoderSoftware
	}
	v := &videoEncoder{name: name, log: log}
	software := encoders[protocol.EncoderSoftware]
	if start.EncoderFallback && name != protocol.EncoderSoftware && software != nil {
		v.sw = software
	}
	var ok bool
	if v.hw, ok = encoders[name]; !ok {
		if v.sw == nil {
			return nil, fmt.Errorf("encoder %q is not available in this agent", name)
		}
		v.err = fmt.Errorf("%s is not available in this agent", name)
		log.Warn("using software video", "why", v.err)
		return v, nil
	}
	enc, err := v.open(trialWidth, trialHeight, start.FPS, startBitrate)
	if err != nil {
		return nil, fmt.Errorf("encoder %s: %w", name, err)
	}
	enc.Close()
	return v, nil
}

// videoEncoder opens the session's encoders. A hardware encoder the CLI
// picked gives way to software from the first time it fails, to open at a
// size or to encode a frame, for the rest of the session: the pick was a
// guess from the probe, and software video beats none. The session opens
// and drives encoders from one goroutine at a time; inUse is safe from any.
type videoEncoder struct {
	name   string // the encoder Start asked for
	hw, sw NewEncoderFunc
	log    *slog.Logger

	mu  sync.Mutex
	err error // why hw was given up; nil while it works or when sw is nil
}

// inUse reports the encoder the session uses now and, once a picked
// encoder has given way to software, why: what Ready says.
func (v *videoEncoder) inUse() (name, why string) {
	if err := v.givenUp(); err != nil {
		return protocol.EncoderSoftware, err.Error()
	}
	return v.name, ""
}

func (v *videoEncoder) givenUp() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.err
}

func (v *videoEncoder) giveUp(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.err == nil {
		v.err = fmt.Errorf("%s: %w", v.name, err)
		v.log.Warn("hardware encoder failed; software video from now on", "encoder", v.name, "err", err)
	}
}

// open is the session's NewEncoderFunc.
func (v *videoEncoder) open(width, height, fps, bitrate int) (Encoder, error) {
	if v.givenUp() == nil {
		enc, err := v.hw(width, height, fps, bitrate)
		switch {
		case err == nil && v.sw == nil:
			return enc, nil
		case err == nil:
			return &pickedEncoder{Encoder: enc, v: v, width: width, height: height, fps: fps, bitrate: bitrate}, nil
		case v.sw == nil:
			return nil, err
		}
		v.giveUp(err)
	}
	return v.sw(width, height, fps, bitrate)
}

// pickedEncoder is a picked hardware encoder at one size. A hardware tool
// may start only at the first frame, so a size it cannot encode fails there,
// not at open: from the first frame it fails, software takes its place and
// encodes that frame again, as a keyframe since its stream starts there.
type pickedEncoder struct {
	Encoder // the hardware encoder, then software once soft
	v       *videoEncoder
	soft    bool

	width, height, fps, bitrate int
}

func (e *pickedEncoder) Encode(img *yuv.I420, ts time.Duration, keyframe bool) ([]byte, error) {
	au, err := e.Encoder.Encode(img, ts, keyframe)
	if err == nil || e.soft {
		return au, err
	}
	e.v.giveUp(err)
	e.Encoder.Close()
	sw, swErr := e.v.sw(e.width, e.height, e.fps, e.bitrate)
	if swErr != nil {
		return nil, fmt.Errorf("%w; software: %w", err, swErr)
	}
	e.Encoder, e.soft = sw, true
	return sw.Encode(img, ts, true)
}

func (e *pickedEncoder) SetBitrate(bps int) error {
	e.bitrate = bps
	return e.Encoder.SetBitrate(bps)
}
