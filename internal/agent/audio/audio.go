//go:build linux

// Package audio captures a desktop's sound and encodes it as Opus for the
// session's audio track.
//
// It speaks the PulseAudio native protocol in Go, so the static agent needs
// no sound libraries on the host; PipeWire serves the same protocol through
// pipewire-pulse. An attached desktop is recorded from the user's own sound
// server. A spawned one gets a private pulseaudio with a null sink, and the
// environment its programs need to play into it.
package audio

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/yasinmiran/robin/internal/agent/opus"
)

// Options say where the sound comes from.
type Options struct {
	// RunDir is the session's private directory, where a spawned desktop's
	// sound server keeps its socket and cookie.
	RunDir string
	// Attached records the user's running sound server instead of starting
	// one.
	Attached bool
	Log      *slog.Logger
}

// queued bounds the packets waiting for the session, one second's worth; a
// consumer that falls further behind loses the newest.
const queued = 50

// Source is a running capture. Packets carries one Opus packet per 20 ms.
type Source struct {
	log *slog.Logger
	srv *server // nil when attached
	rec *recorder

	mu      sync.Mutex // guards the fields below against the recorder's callback
	enc     *opus.Encoder
	packets chan []byte
	closed  bool
	dropped int
}

// Start begins capturing and encoding. env holds what a spawned desktop's
// programs need to reach its sound server, and is empty when attached. ctx
// bounds startup only.
func Start(ctx context.Context, o Options) (_ *Source, env []string, err error) {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	enc, err := opus.NewEncoder(opus.Bitrate, opus.Complexity)
	if err != nil {
		return nil, nil, err
	}
	s := &Source{log: log, enc: enc, packets: make(chan []byte, queued)}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()

	var socket string
	var cookie []byte
	if o.Attached {
		socket, cookie = userSocket(), userCookie()
		if _, err := os.Stat(socket); err != nil {
			return nil, nil, fmt.Errorf("no sound server for this user at %s", socket)
		}
	} else {
		if s.srv, err = startServer(ctx, o.RunDir); err != nil {
			return nil, nil, err
		}
		socket = s.srv.socket
		if cookie, err = os.ReadFile(s.srv.cookie); err != nil {
			return nil, nil, err
		}
		env = s.srv.Env()
	}

	if s.rec, err = record(socket, cookie, s.encode, s.ended); err != nil {
		if s.srv != nil {
			return nil, nil, fmt.Errorf("record the private pulseaudio: %w", err)
		}
		return nil, nil, fmt.Errorf("record the sound server at %s: %w", socket, err)
	}
	where := "the user's sound server"
	if s.srv != nil {
		where = "a private pulseaudio"
	}
	log.Info("audio up", "from", where, "sink", s.rec.sink, "kbps", opus.Bitrate/1000)
	return s, env, nil
}

// Packets delivers Opus packets until the capture ends or Close is called.
func (s *Source) Packets() <-chan []byte { return s.packets }

// Follow records source from now on, such as a silent room's monitor, or
// for "" the monitor the capture started on.
func (s *Source) Follow(source string) error { return s.rec.follow(source) }

// Close stops the capture and any sound server Start began. It is safe to
// call more than once.
func (s *Source) Close() error {
	s.mu.Lock()
	s.finish()
	s.mu.Unlock()
	if s.rec != nil {
		s.rec.close()
	}
	if s.srv != nil {
		s.srv.Close()
	}
	return nil
}

// encode runs on the recorder's read goroutine. It never blocks on the
// session: a full queue drops the packet.
func (s *Source) encode(pcm []int16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	pkt, err := s.enc.Encode(pcm)
	if err != nil {
		s.log.Warn("encode audio", "err", err)
		return
	}
	select {
	case s.packets <- pkt:
	default:
		if s.dropped++; s.dropped%queued == 1 {
			s.log.Warn("audio consumer is behind; dropping packets", "dropped", s.dropped)
		}
	}
}

// ended runs when the server stops the stream underneath the session, which
// then carries on without sound.
func (s *Source) ended(err error) {
	if s.srv != nil {
		// The socket closes a moment before the exit status is in.
		select {
		case <-s.srv.Done():
			err = s.srv.exitError()
		case <-time.After(500 * time.Millisecond):
		}
	}
	s.log.Warn("audio capture ended", "err", err)
	s.mu.Lock()
	s.finish()
	s.mu.Unlock()
}

// finish closes the queue and the encoder once; s.mu must be held.
func (s *Source) finish() {
	if s.closed {
		return
	}
	s.closed = true
	close(s.packets)
	s.enc.Close()
}
