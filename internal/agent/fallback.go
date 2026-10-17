//go:build linux

package agent

import (
	"fmt"
	"log/slog"

	"github.com/yasinmiran/robin/internal/protocol"
)

// trialWidth and trialHeight size the frame a new session encodes before
// the desktop comes up.
const trialWidth, trialHeight = 320, 240

// chooseEncoder returns the name and constructor of the encoder the session
// uses, and why that is not the one Start named, if it is not. A hardware
// encoder rests on tools installed on the host, so it is tried on a small
// frame now, not when the browser's first frame is due. A failure ends the
// session when the user named the encoder; when the CLI picked it
// (Start.EncoderFallback), software takes over instead.
func chooseEncoder(start protocol.Start, encoders map[string]NewEncoderFunc, log *slog.Logger) (string, NewEncoderFunc, string, error) {
	name := start.Encoder
	if name == "" {
		name = protocol.EncoderSoftware
	}
	software := encoders[protocol.EncoderSoftware]
	canFall := start.EncoderFallback && name != protocol.EncoderSoftware && software != nil
	newEncoder, ok := encoders[name]
	if !ok {
		if !canFall {
			return "", nil, "", fmt.Errorf("encoder %q is not available in this agent", name)
		}
		why := fmt.Sprintf("%s is not available in this agent", name)
		log.Warn("using software video", "why", why)
		return protocol.EncoderSoftware, software, why, nil
	}
	var fb *encoderFallback
	if canFall {
		fb = &encoderFallback{name: name, hw: newEncoder, sw: software, log: log}
		newEncoder = fb.open
	}
	enc, err := newEncoder(trialWidth, trialHeight, start.FPS, startBitrate)
	if err != nil {
		return "", nil, "", fmt.Errorf("encoder %s: %w", name, err)
	}
	enc.Close()
	if fb != nil && fb.err != nil {
		return protocol.EncoderSoftware, software, fb.err.Error(), nil
	}
	return name, newEncoder, "", nil
}

// encoderFallback opens the hardware encoder the CLI picked and, from the
// first time that fails, software for the rest of the session: the pick was
// a guess from the probe, and software video beats no session. The session
// opens encoders from one goroutine at a time.
type encoderFallback struct {
	name   string
	hw, sw NewEncoderFunc
	log    *slog.Logger
	err    error // why hardware was given up; nil while it works
}

func (f *encoderFallback) open(width, height, fps, bitrate int) (Encoder, error) {
	if f.err == nil {
		enc, err := f.hw(width, height, fps, bitrate)
		if err == nil {
			return enc, nil
		}
		f.err = fmt.Errorf("%s: %w", f.name, err)
		f.log.Warn("hardware encoder failed; software video from now on", "encoder", f.name, "err", err)
	}
	return f.sw(width, height, fps, bitrate)
}
