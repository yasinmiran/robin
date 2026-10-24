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
	"time"

	"github.com/jfreymuth/pulse/proto"

	"github.com/yasinmiran/robin/internal/agent/opus"
	"github.com/yasinmiran/robin/internal/agent/track"
	"github.com/yasinmiran/robin/internal/protocol"
)

// micQueue bounds the sound waiting for the server, about 200 ms in 20 ms
// pieces. A server that falls further behind loses the newest piece rather
// than holding up the track.
const micQueue = 10

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
	where   string
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
		p, err := openPulseMic(src.srv.socket, cookie, spawnMicFeed, log)
		if err != nil {
			return nil, fmt.Errorf("play into the private pulseaudio: %w", err)
		}
		return newMic(p, "the private pulseaudio", nil, log), nil
	}

	socket, cookie := userSocket(), userCookie()
	if _, err := os.Stat(socket); err != nil {
		return nil, fmt.Errorf("no sound server for this user at %s", socket)
	}
	c, conn, err := connect(context.Background(), socket, cookie, nil)
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
		// A PipeWire node that a stream provides negotiates its format, and
		// so appears, only once fed, and a program recording it would wait
		// on it: it gets silence while the browser sends none.
		return newMic(p, "PipeWire", time.Tick(micFrameTime), log), nil
	}
	p, err := loadPulseMic(socket, cookie, log)
	if err != nil {
		return nil, fmt.Errorf("add the microphone to PulseAudio: %w", err)
	}
	return newMic(p, "PulseAudio", nil, log), nil
}

// newMic starts feeding sink. ticks, every micFrameTime, pace the silence a
// sink that needs feeding gets while the browser sends none; nil leaves
// silence to a sink that makes its own.
func newMic(sink micSink, where string, ticks <-chan time.Time, log *slog.Logger) *Mic {
	m := &Mic{where: where, log: log, sink: sink, queue: make(chan []byte, micQueue), drained: make(chan struct{})}
	go m.write(ticks)
	log.Info("microphone up", "on", where, "name", protocol.MicName)
	return m
}

// Where names what carries the microphone on the host, for Ready: the
// private pulseaudio, PulseAudio or PipeWire.
func (m *Mic) Where() string { return m.where }

// micIdleTicks is how many ticks the browser's sound may stop for before
// silence fills in: three packets, past the jitter of a live track.
const micIdleTicks = 3

// write hands the queue to the sink, on a goroutine of its own so a slow
// server stalls nothing but this, and silence on each of ticks once the
// browser's sound has stopped for more than micIdleTicks of them.
func (m *Mic) write(ticks <-chan time.Time) {
	defer close(m.drained)
	silence := make([]byte, micFrameBytes)
	quiet := micIdleTicks // ticks since the browser's sound, which has yet to start
	failed := 0
	put := func(b []byte) {
		if err := m.sink.write(b); err != nil {
			if failed++; failed%250 == 1 {
				m.log.Warn("microphone: the sound server took no sound", "err", err, "times", failed)
			}
		}
	}
	for {
		select {
		case b, ok := <-m.queue:
			if !ok {
				return
			}
			put(b)
			quiet = 0
		case <-ticks:
			if quiet++; quiet > micIdleTicks {
				put(silence)
			}
		}
	}
}

// Play feeds the microphone from one track of the browser's until the
// track ends. Tracks take turns: a second Play waits for the first.
func (m *Mic) Play(r track.Reader) error {
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
