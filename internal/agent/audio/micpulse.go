//go:build linux

package audio

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strconv"
	"syscall"

	"github.com/jfreymuth/pulse/proto"

	"github.com/yasinmiran/robin/internal/agent/opus"
)

// On PulseAudio the microphone is a null sink the agent plays into, and a
// remap source over that sink's monitor, which is what programs record. A
// remap source rather than the monitor itself, since programs leave
// monitors out of their lists of microphones.
const (
	// spawnMicFeed and spawnMicSource are their names on a spawned
	// desktop's private server, which loads them at startup.
	spawnMicFeed   = "robin-mic-feed"
	spawnMicSource = "robin-mic"
	// micFeedPrefix and micSourcePrefix, followed by the agent's pid, name
	// them on the user's own server, where the agent loads them and a later
	// agent unloads those whose agent died without doing so.
	micFeedPrefix   = "robin-mic-feed-"
	micSourcePrefix = "robin-mic-"
	// micFrameBytes is 20 ms of the microphone: 16-bit mono.
	micFrameBytes = opus.FrameSamples * 2
)

// paModule is a module to load and its arguments.
type paModule struct{ name, args string }

// micModules make a microphone of feed and source, for a .pa script or
// LoadModule.
func micModules(feed, source string) []paModule {
	return []paModule{
		{"module-null-sink", "sink_name=" + feed + " rate=48000 channels=1 channel_map=mono norewinds=1 " +
			`sink_properties='device.description="` + MicName + ` feed"'`},
		{"module-remap-source", "master=" + feed + ".monitor source_name=" + source + " channels=1 channel_map=mono " +
			`source_properties='device.description="` + MicName + `"'`},
	}
}

// micScript is what a spawned desktop's private server loads for the
// microphone: the two modules, the source made the default.
func micScript() string {
	var s string
	for _, m := range micModules(spawnMicFeed, spawnMicSource) {
		s += "load-module " + m.name + " " + m.args + "\n"
	}
	return s + "set-default-source " + spawnMicSource + "\n"
}

// pulseMic plays the microphone into its feed sink over one connection.
// Modules it loaded go when it closes.
type pulseMic struct {
	c       *proto.Client
	conn    net.Conn
	stream  uint32
	modules []uint32
	log     *slog.Logger
}

// openPulseMic connects to the server at socket and starts playing into the
// microphone's feed. On the user's own server (load set) it first unloads
// microphones that dead agents left and loads this agent's.
func openPulseMic(socket string, cookie []byte, load bool, log *slog.Logger) (_ *pulseMic, err error) {
	p := &pulseMic{log: log}
	c, conn, err := connect(socket, cookie, p.notice)
	if err != nil {
		return nil, err
	}
	p.c, p.conn = c, conn
	defer func() {
		if err != nil {
			p.close()
		}
	}()

	feed := spawnMicFeed
	if load {
		p.sweep()
		pid := strconv.Itoa(os.Getpid())
		feed = micFeedPrefix + pid
		for _, m := range micModules(feed, micSourcePrefix+pid) {
			var reply proto.LoadModuleReply
			if err := c.Request(&proto.LoadModule{Name: m.name, Args: m.args}, &reply); err != nil {
				return nil, fmt.Errorf("load %s: %w", m.name, err)
			}
			p.modules = append(p.modules, reply.ModuleIndex)
		}
	}
	var reply proto.CreatePlaybackStreamReply
	if err := c.Request(&proto.CreatePlaybackStream{
		SampleSpec:            proto.SampleSpec{Format: proto.FormatInt16LE, Channels: 1, Rate: opus.SampleRate},
		ChannelMap:            proto.ChannelMap{proto.ChannelMono},
		SinkIndex:             proto.Undefined,
		SinkName:              feed,
		BufferMaxLength:       10 * micFrameBytes,
		BufferTargetLength:    3 * micFrameBytes,
		BufferPrebufferLength: micFrameBytes,
		BufferMinimumRequest:  micFrameBytes,
		SyncID:                proto.Undefined,
		ChannelVolumes:        proto.ChannelVolumes{proto.VolumeNorm},
		NoMove:                true,
		Properties:            proto.PropList{"media.name": proto.PropListString(MicName)},
	}, &reply); err != nil {
		return nil, fmt.Errorf("play into %s: %w", feed, err)
	}
	p.stream = reply.StreamIndex
	return p, nil
}

// notice runs on the client's read goroutine for what the server says
// unasked. Writing more than the server requested is allowed up to the
// stream's maximum, so requests go unanswered.
func (p *pulseMic) notice(msg any) {
	switch msg.(type) {
	case *proto.PlaybackStreamKilled:
		p.log.Warn("microphone: the sound server dropped it")
	case *proto.ConnectionClosed:
		p.log.Warn("microphone: the sound server closed the connection")
	}
}

func (p *pulseMic) write(pcm []byte) error { return p.c.Send(p.stream, pcm) }

// close unloads what openPulseMic loaded, the source before the sink it
// reads, and drops the connection with the stream on it.
func (p *pulseMic) close() error {
	for i := len(p.modules) - 1; i >= 0; i-- {
		if err := p.c.Request(&proto.UnloadModule{ModuleIndex: p.modules[i]}, nil); err != nil {
			p.log.Warn("microphone: unload module", "index", p.modules[i], "err", err)
		}
	}
	p.modules = nil
	return p.conn.Close()
}

// micOwner matches the module arguments of a microphone on the user's
// server, capturing the pid of the agent that loaded it.
var micOwner = regexp.MustCompile(`(?:^|\s)(?:sink_name=` + micFeedPrefix + `|source_name=` + micSourcePrefix + `)(\d+)(?:\s|$)`)

// sweep unloads microphones whose agent is gone: one killed outright had no
// chance to unload its own. Sources go first, before the sinks they read.
func (p *pulseMic) sweep() {
	var list proto.GetModuleInfoListReply
	if err := p.c.Request(&proto.GetModuleInfoList{}, &list); err != nil {
		p.log.Warn("microphone: list modules", "err", err)
		return
	}
	for _, name := range []string{"module-remap-source", "module-null-sink"} {
		for _, m := range list {
			if m.ModuleName != name {
				continue
			}
			sub := micOwner.FindStringSubmatch(m.ModuleArgs)
			if sub == nil {
				continue
			}
			pid, err := strconv.Atoi(sub[1])
			if err != nil || pid == os.Getpid() || !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				continue
			}
			if err := p.c.Request(&proto.UnloadModule{ModuleIndex: m.ModuleIndex}, nil); err != nil {
				p.log.Warn("microphone: unload a dead agent's module", "index", m.ModuleIndex, "err", err)
				continue
			}
			p.log.Info("microphone: unloaded a dead agent's module", "module", name, "agent", pid)
		}
	}
}
