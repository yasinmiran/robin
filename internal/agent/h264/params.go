package h264

import "runtime"

// Params tune the encoder. Defaults returns the values the agent uses.
type Params struct {
	Width, Height, FPS int
	Bitrate            int // target, bits per second
	Threads            int
	MinQP, MaxQP       int
	// Screen selects openh264's screen-content mode, which always detects
	// scene changes; camera mode lets SceneChange be switched off.
	Screen      bool
	SceneChange bool
	FrameSkip   bool
}

// Defaults returns the agent's encoder settings for a frame size. Camera mode
// with scene-change detection off never inserts IDR frames of its own (screen
// mode turns full-screen motion into an IDR per frame) and encodes 3 to 4
// times faster. Frame skipping is what holds the target on incompressible
// content; its skips cluster after a keyframe, while the link is still busy
// carrying it. Past QP 40 small text breaks into blocks, so beyond that rate
// control skips frames instead: a desktop is better read slowly than not.
func Defaults(width, height, fps, bitrate int) Params {
	return Params{
		Width: width, Height: height, FPS: fps, Bitrate: bitrate,
		Threads: min(max(runtime.NumCPU()/2, 1), 4),
		MinQP:   16, MaxQP: 40,
		Screen: false, SceneChange: false, FrameSkip: true,
	}
}
