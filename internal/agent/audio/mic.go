//go:build linux

package audio

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/jfreymuth/pulse/proto"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"

	"github.com/yasinmiran/robin/internal/agent/opus"
)

// MicName is what the desktop's programs see the browser's microphone as.
const MicName = "Robin microphone"

// micQueue bounds the sound waiting for the server, about 200 ms in 20 ms
// pieces. A server that falls further behind loses the newest piece rather
// than holding up the track.
const micQueue = 10

// RTPReader yields the packets of one media track. *webrtc.TrackRemote is
// one.
type RTPReader interface {
	ReadRTP() (*rtp.Packet, interceptor.Attributes, error)
}

// MicOptions say where the browser's microphone goes.
type MicOptions struct {
	// Source is a spawned desktop's sound, started with Options.Mic, whose
	// private pulseaudio has the microphone as its default source. Nil puts
	// the microphone on the user's own sound server, for an attached
	// desktop.
	Source *Source
	Log    *slog.Logger
}

// micSink is where decoded sound goes: a playback stream into PulseAudio,
// or a pipeline into PipeWire.
type micSink interface {
	write(pcm []byte) error
	close() error
}

// Mic is the browser's microphone as a source on the desktop's sound
// server. It is there, silent, from StartMic until Close; Play fills it
// while the browser sends.
type Mic struct {
	// Where says which server carries it, for the log.
	Where string

	log     *slog.Logger
	sink    micSink
	queue   chan []byte
	drained chan struct{} // closed once the writer has emptied queue

	mu      sync.Mutex // guards closed and dropped against Play
	closed  bool
	dropped int

	playing sync.Mutex // held by the one Play at a time
}

// StartMic adds the microphone to the desktop's sound server. ctx bounds
// startup only.
func StartMic(ctx context.Context, o MicOptions) (*Mic, error) {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	if src := o.Source; src != nil {
		if src.srv == nil || !src.mic {
			return nil, errors.New("audio: this sound was started without a microphone")
		}
		cookie, err := os.ReadFile(src.srv.cookie)
		if err != nil {
			return nil, err
		}
		p, err := openPulseMic(src.srv.socket, cookie, false, log)
		if err != nil {
			return nil, fmt.Errorf("play into the private pulseaudio: %w", err)
		}
		return newMic(p, "the private pulseaudio", log), nil
	}

	socket, cookie := userSocket(), userCookie()
	if _, err := os.Stat(socket); err != nil {
		return nil, fmt.Errorf("no sound server for this user at %s", socket)
	}
	c, conn, err := connect(socket, cookie, nil)
	if err != nil {
		return nil, fmt.Errorf("the sound server at %s: %w", socket, err)
	}
	defer conn.Close()
	var info proto.GetServerInfoReply
	if err := c.Request(&proto.GetServerInfo{}, &info); err != nil {
		return nil, fmt.Errorf("ask the sound server what it is: %w", err)
	}
	if strings.Contains(info.PackageName, "PipeWire") {
		p, err := startPipeWireMic(ctx, c, log)
		if err != nil {
			return nil, err
		}
		return newMic(p, "PipeWire", log), nil
	}
	p, err := openPulseMic(socket, cookie, true, log)
	if err != nil {
		return nil, fmt.Errorf("add the microphone to PulseAudio: %w", err)
	}
	return newMic(p, "PulseAudio", log), nil
}

func newMic(sink micSink, where string, log *slog.Logger) *Mic {
	m := &Mic{Where: where, log: log, sink: sink, queue: make(chan []byte, micQueue), drained: make(chan struct{})}
	go m.write()
	log.Info("microphone up", "on", where, "name", MicName)
	return m
}

// write hands the queue to the sink, on a goroutine of its own so a slow
// server stalls nothing but this.
func (m *Mic) write() {
	defer close(m.drained)
	failed := 0
	for b := range m.queue {
		if err := m.sink.write(b); err != nil {
			if failed++; failed%250 == 1 {
				m.log.Warn("microphone: the sound server took no sound", "err", err, "times", failed)
			}
		}
	}
}

// Play feeds the microphone from one track of the browser's until the
// track ends. Tracks take turns: a second Play waits for the first.
func (m *Mic) Play(r RTPReader) error {
	m.playing.Lock()
	defer m.playing.Unlock()
	dec, err := opus.NewDecoder(1)
	if err != nil {
		return err
	}
	defer dec.Close()
	st := newMicStream(dec, m.enqueue)
	defer func() {
		m.log.Info("microphone track ended", "packets", st.packets, "lost", st.lost, "late", st.late,
			"recovered", st.recovered, "concealed", st.concealed, "undecodable", st.failed)
	}()
	for {
		p, _, err := r.ReadRTP()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		st.push(p)
	}
}

// enqueue queues one piece of sound as little-endian bytes, dropping it when
// the queue is full.
func (m *Mic) enqueue(pcm []int16) {
	b := make([]byte, 2*len(pcm))
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	select {
	case m.queue <- b:
	default:
		if m.dropped++; m.dropped%micQueue == 1 {
			m.log.Warn("microphone: the sound server is behind; dropping sound", "dropped", m.dropped)
		}
	}
}

// Close takes the microphone off the sound server. It is safe to call more
// than once; a Play still running feeds nothing after it.
func (m *Mic) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	close(m.queue)
	m.mu.Unlock()
	<-m.drained
	return m.sink.close()
}
