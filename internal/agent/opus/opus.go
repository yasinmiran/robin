// Package opus is the agent's audio codec: libopus, statically linked,
// turning 20 ms of 48 kHz stereo into one packet for a WebRTC Opus track,
// and the browser's microphone packets back into sound. Builds without the
// opus tag get a stub whose NewEncoder and NewDecoder fail.
package opus

const (
	// SampleRate is the only rate the encoder takes, and what WebRTC's Opus
	// clock runs at.
	SampleRate = 48000
	// Channels is the number of interleaved channels in a frame.
	Channels = 2
	// FrameSamples is the length of one 20 ms frame, per channel.
	FrameSamples = SampleRate / 50
	// MaxFrameSamples is the longest packet Opus codes, 120 ms, per channel.
	MaxFrameSamples = 6 * FrameSamples
	// Bitrate is the target for stereo desktop sound: enough that music
	// survives, a rounding error next to the video.
	Bitrate = 96_000
	// Complexity is libopus's CPU dial, 0 to 10.
	Complexity = 10
	// ExpectedLoss is the packet loss the encoder plans for, in percent.
	// Without some loss to plan for, in-band FEC never codes anything.
	ExpectedLoss = 5
)
