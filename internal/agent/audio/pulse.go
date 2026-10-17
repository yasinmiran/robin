//go:build linux

package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/jfreymuth/pulse/proto"

	"github.com/yasinmiran/remotejump/internal/agent/opus"
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
	// sink is the sink whose monitor is recorded.
	sink string
}

// record connects to the server at socket and starts recording. onEnd runs
// once if the server ends the stream or the connection; closing the recorder
// does not call it.
func record(socket string, cookie []byte, onFrame func([]int16), onEnd func(error)) (_ *recorder, err error) {
	conn, err := net.DialTimeout("unix", socket, requestTimeout)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			conn.Close()
		}
	}()

	f := framer{onFrame: onFrame}
	ended := false
	end := func(err error) {
		if !ended {
			ended = true
			onEnd(err)
		}
	}
	c := &proto.Client{}
	// The client runs Callback on its read goroutine, the only place frames
	// and endings arrive, so f and ended need no lock. It is set before Open
	// starts that goroutine. The connection carries one stream, so every data
	// packet is ours.
	c.Callback = func(msg any) {
		switch m := msg.(type) {
		case *proto.DataPacket:
			f.write(m.Data)
		case *proto.RecordStreamKilled:
			end(errors.New("the sound server dropped the recording"))
		case *proto.ConnectionClosed:
			end(errors.New("the sound server closed the connection"))
		}
	}
	c.Open(conn)
	c.SetTimeout(requestTimeout)

	var auth proto.AuthReply
	if err := c.Request(&proto.Auth{Version: c.Version(), Cookie: cookie}, &auth); err != nil {
		return nil, fmt.Errorf("authenticate: %w", err)
	}
	c.SetVersion(auth.Version)
	if err := c.Request(&proto.SetClientName{Props: proto.PropList{
		"application.name":       proto.PropListString("remotejump"),
		"application.process.id": proto.PropListString(fmt.Sprint(os.Getpid())),
	}}, &proto.SetClientNameReply{}); err != nil {
		return nil, fmt.Errorf("introduce the client: %w", err)
	}
	var info proto.GetServerInfoReply
	if err := c.Request(&proto.GetServerInfo{}, &info); err != nil {
		return nil, fmt.Errorf("ask for the default sink: %w", err)
	}
	if info.DefaultSinkName == "" {
		return nil, errors.New("the sound server has no output to capture")
	}
	var reply proto.CreateRecordStreamReply
	if err := c.Request(&proto.CreateRecordStream{
		SampleSpec:         proto.SampleSpec{Format: proto.FormatInt16LE, Channels: opus.Channels, Rate: opus.SampleRate},
		ChannelMap:         proto.ChannelMap{proto.ChannelLeft, proto.ChannelRight},
		SourceIndex:        proto.Undefined,
		SourceName:         info.DefaultSinkName + ".monitor",
		BufferMaxLength:    10 * frameBytes,
		BufferFragSize:     frameBytes,
		AdjustLatency:      true,
		DirectOnInputIndex: proto.Undefined,
		Properties:         proto.PropList{"media.name": proto.PropListString("remotejump desktop sound")},
		ChannelVolumes:     proto.ChannelVolumes{proto.VolumeNorm, proto.VolumeNorm},
	}, &reply); err != nil {
		return nil, fmt.Errorf("record %s.monitor: %w", info.DefaultSinkName, err)
	}
	return &recorder{conn: conn, sink: info.DefaultSinkName}, nil
}

// close drops the connection, and with it the stream.
func (r *recorder) close() { r.conn.Close() }

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
func userSocket() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "pulse", "native")
	}
	return fmt.Sprintf("/run/user/%d/pulse/native", os.Getuid())
}

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
