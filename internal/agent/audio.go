//go:build linux

package agent

import (
	"context"
	"log/slog"
	"time"
)

// AudioFrame is the duration of one packet from an AudioSource.
const AudioFrame = 20 * time.Millisecond

// AudioSource yields the desktop's sound as Opus packets of AudioFrame each.
type AudioSource interface {
	Packets() <-chan []byte
	Close() error
}

// AudioOptions say where the sound comes from.
type AudioOptions struct {
	// RunDir is the session's private directory, for a sound server the agent
	// starts itself.
	RunDir string
	// Attached means the desktop is already running with its own sound
	// server to capture; otherwise the agent provides one for the display
	// it spawns.
	Attached bool
	Log      *slog.Logger
}

// NewAudioFunc starts capturing. env holds variables the spawned desktop's
// programs need to reach the sound server, such as PULSE_SERVER.
type NewAudioFunc func(ctx context.Context, o AudioOptions) (src AudioSource, env []string, err error)
