//go:build linux

package audio

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jfreymuth/pulse/proto"

	"github.com/yasinmiran/robin/internal/agent/opus"
	"github.com/yasinmiran/robin/internal/agent/xdg"
)

const (
	// frameBytes is one 20 ms frame of 16-bit stereo.
	frameBytes = opus.FrameSamples * opus.Channels * 2
	// requestTimeout bounds each protocol request; the library's default of
	// a second is tight for a server that has only just started.
	requestTimeout = 5 * time.Second
	cookieSize     = 256
)

// recorder reads the default sink's monitor over the PulseAudio native
// protocol, which pipewire-pulse speaks too. It hands each 20 ms of
// interleaved stereo to onFrame on the protocol's read goroutine.
type recorder struct {
	conn net.Conn
	c    *proto.Client
	// sink is the sink whose monitor the recording started on.
	sink string
	// stream is the record stream whose data counts; follow replaces it.
	stream *atomic.Uint32
	mu     sync.Mutex // serializes follow
}

// record connects to the server at socket and starts recording. onEnd runs
// once if the server ends the stream or the connection; closing the recorder
// does not call it.
func record(socket string, cookie []byte, onFrame func([]int16), onEnd func(error)) (_ *recorder, err error) {
	f := framer{onFrame: onFrame}
	ended := false
	end := func(err error) {
		if !ended {
			ended = true
			onEnd(err)
		}
	}
	// The callback is the only place frames and endings arrive, so f and
	// ended need no lock.
	stream := new(atomic.Uint32)
	stream.Store(proto.Undefined)
	c, conn, err := connect(context.Background(), socket, cookie, func(msg any) {
		switch m := msg.(type) {
		case *proto.DataPacket:
			if m.StreamIndex == stream.Load() {
				f.write(m.Data)
			}
		case *proto.RecordStreamKilled:
			if m.StreamIndex == stream.Load() {
				end(errors.New("the sound server dropped the recording"))
			}
		case *proto.ConnectionClosed:
			end(errors.New("the sound server closed the connection"))
		}
	})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			conn.Close()
		}
	}()
	var info proto.GetServerInfoReply
	if err := c.Request(&proto.GetServerInfo{}, &info); err != nil {
		return nil, fmt.Errorf("ask for the default sink: %w", err)
	}
	if info.DefaultSinkName == "" {
		return nil, errors.New("the sound server has no output to capture")
	}
	r := &recorder{conn: conn, c: c, sink: info.DefaultSinkName, stream: stream}
	index, err := r.open(info.DefaultSinkName + ".monitor")
	if err != nil {
		return nil, err
	}
	stream.Store(index)
	return r, nil
}

// open starts a record stream on source.
func (r *recorder) open(source string) (uint32, error) {
	var reply proto.CreateRecordStreamReply
	if err := r.c.Request(&proto.CreateRecordStream{
		SampleSpec:         proto.SampleSpec{Format: proto.FormatInt16LE, Channels: opus.Channels, Rate: opus.SampleRate},
		ChannelMap:         proto.ChannelMap{proto.ChannelLeft, proto.ChannelRight},
		SourceIndex:        proto.Undefined,
		SourceName:         source,
		BufferMaxLength:    10 * frameBytes,
		BufferFragSize:     frameBytes,
		AdjustLatency:      true,
		DirectOnInputIndex: proto.Undefined,
		Properties:         proto.PropList{"media.name": proto.PropListString("robin desktop sound")},
		ChannelVolumes:     proto.ChannelVolumes{proto.VolumeNorm, proto.VolumeNorm},
	}, &reply); err != nil {
		return 0, fmt.Errorf("record %s: %w", source, err)
	}
	return reply.StreamIndex, nil
}

// close drops the connection, and with it the stream.
func (r *recorder) close() { r.conn.Close() }

// follow records source from now on, or for "" the monitor the recording
// started on. It opens a stream there and drops the old one rather than
// moving it: WirePlumber 0.4 remembers where a stream was moved as its
// program's preference.
func (r *recorder) follow(source string) error {
	if source == "" {
		source = r.sink + ".monitor"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	index, err := r.open(source)
	if err != nil {
		return err
	}
	old := r.stream.Swap(index)
	if err := r.c.Request(&proto.DeleteRecordStream{StreamIndex: old}, nil); err != nil {
		return fmt.Errorf("stop recording the old source: %w", err)
	}
	return nil
}

// connect opens a connection to the server at socket and introduces itself.
// onMsg, when set, runs on the connection's read goroutine for whatever the
// server sends that answers no request. A request made from onMsg would
// wait for its reply on that same goroutine, for ever. ctx bounds the dial,
// and its deadline, if any, the connection's whole life.
func connect(ctx context.Context, socket string, cookie []byte, onMsg func(any)) (_ *proto.Client, _ net.Conn, err error) {
	dialer := net.Dialer{Timeout: requestTimeout}
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, nil, err
	}
	if d, ok := ctx.Deadline(); ok {
		conn.SetDeadline(d)
	}
	defer func() {
		if err != nil {
			conn.Close()
		}
	}()
	c := &proto.Client{}
	if onMsg != nil {
		c.Callback = onMsg // before Open starts the read goroutine
	}
	c.Open(conn)
	c.SetTimeout(requestTimeout)

	var auth proto.AuthReply
	if err := c.Request(&proto.Auth{Version: c.Version(), Cookie: cookie}, &auth); err != nil {
		return nil, nil, fmt.Errorf("authenticate: %w", err)
	}
	c.SetVersion(auth.Version)
	if err := c.Request(&proto.SetClientName{Props: proto.PropList{
		"application.name":       proto.PropListString("robin"),
		"application.process.id": proto.PropListString(fmt.Sprint(os.Getpid())),
	}}, &proto.SetClientNameReply{}); err != nil {
		return nil, nil, fmt.Errorf("introduce the client: %w", err)
	}
	return c, conn, nil
}

// framer cuts the byte stream the server sends into whole frames.
type framer struct {
	buf     [frameBytes]byte
	n       int
	pcm     [frameBytes / 2]int16
	onFrame func([]int16)
}

func (f *framer) write(b []byte) {
	for len(b) > 0 {
		c := copy(f.buf[f.n:], b)
		f.n += c
		b = b[c:]
		if f.n < frameBytes {
			return
		}
		for i := range f.pcm {
			f.pcm[i] = int16(binary.LittleEndian.Uint16(f.buf[2*i:]))
		}
		f.n = 0
		f.onFrame(f.pcm[:])
	}
}

// userSocket is where the logged-in user's sound server listens, as libpulse
// would find it.
func userSocket() string { return filepath.Join(xdg.RuntimeDir(), "pulse", "native") }

// userCookie is the user's PulseAudio cookie, as libpulse would find it.
// PipeWire ignores cookies, and PulseAudio accepts a client of the same user
// without one, so a missing cookie is sent as zeros.
func userCookie() []byte {
	var paths []string
	if p := os.Getenv("PULSE_COOKIE"); p != "" {
		paths = append(paths, p)
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		paths = append(paths, filepath.Join(dir, "pulse", "cookie"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "pulse", "cookie"), filepath.Join(home, ".pulse-cookie"))
	}
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil && len(b) == cookieSize {
			return b
		}
	}
	return make([]byte, cookieSize)
}
