//go:build linux

package audio

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/jfreymuth/pulse/proto"

	"github.com/yasinmiran/robin/internal/agent/gstsink"
	"github.com/yasinmiran/robin/internal/agent/opus"
)

// micNodeTimeout bounds the wait for the microphone's PipeWire node to show
// up on the sound server, as programs will look for it.
const micNodeTimeout = 5 * time.Second

// pipewireMicPipeline builds the gst-launch-1.0 arguments for a PipeWire
// Audio/Source named node, fed 48 kHz mono on stdin. The node lives as long
// as the child.
//
// node.group puts it with PipeWire's dummy driver. Nothing else drives a
// source that a stream provides, so without it a recording through
// pipewire-pulse got 0.2 s of sound in 4 s; with it, all 4. The leaky queue
// drops sound nobody records instead of letting it back up into the agent.
func pipewireMicPipeline(node string) []string {
	props := "props,media.class=Audio/Source,node.name=" + node +
		",node.description=" + gstsink.Quote(MicName) + ",node.group=pipewire.dummy"
	return []string{
		"-q",
		"fdsrc", "fd=0", "do-timestamp=true",
		"!", "rawaudioparse", "use-sink-caps=false", "format=pcm", "pcm-format=s16le",
		"sample-rate=" + strconv.Itoa(opus.SampleRate), "num-channels=1",
		"!", "queue", "leaky=downstream", "max-size-time=100000000", "max-size-buffers=0", "max-size-bytes=0",
		"!", "audioconvert",
		"!", "pipewiresink", "mode=provide", "sync=false", "stream-properties=" + gstsink.Quote(props),
	}
}

// pipewireMic is the microphone as a gst-launch-1.0 child's PipeWire node.
type pipewireMic struct{ s *gstsink.Sink }

func (p pipewireMic) write(pcm []byte) error { return p.s.Write(pcm) }
func (p pipewireMic) close() error           { return p.s.Close() }

// startPipeWireMic starts the child and waits for its node to appear as a
// source on c, the user's pipewire-pulse.
func startPipeWireMic(ctx context.Context, c *proto.Client, log *slog.Logger) (micSink, error) {
	node := micSourcePrefix + strconv.Itoa(os.Getpid())
	s, err := gstsink.Start(pipewireMicPipeline(node), gstsink.Options{
		Name: "microphone", Env: []string{"XDG_RUNTIME_DIR=" + runtimeDir()}, Log: log,
	})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, micNodeTimeout)
	defer cancel()
	for {
		var info proto.GetSourceInfoReply
		if c.Request(&proto.GetSourceInfo{SourceIndex: proto.Undefined, SourceName: node}, &info) == nil {
			return pipewireMic{s}, nil
		}
		select {
		case <-ctx.Done():
			s.Close()
			return nil, fmt.Errorf("the microphone's PipeWire node %s never reached the sound server", node)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
