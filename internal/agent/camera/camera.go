//go:build linux

// Package camera puts the browser's camera on the host as a device its
// programs can open. The browser sends Constrained Baseline H.264; the
// agent decodes it with openh264, fits each picture into one fixed size and
// writes it to a gst-launch-1.0 child that ends in a PipeWire Video/Source
// or in a v4l2loopback device.
package camera

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"

	"github.com/yasinmiran/robin/internal/agent/gstsink"
	"github.com/yasinmiran/robin/internal/agent/h264"
	"github.com/yasinmiran/robin/internal/agent/track"
	"github.com/yasinmiran/robin/internal/agent/xdg"
)

// The picture the host's programs see. The page asks the browser for this
// size; whatever arrives is fitted into it.
const (
	Width, Height = 1280, 720
	FPS           = 30
	// Name is what the host's programs see the camera as on PipeWire.
	Name = "Robin camera"
)

// Timings, variables so tests can shorten them.
var (
	// idleEvery is how often the camera shows black while the browser
	// sends nothing: at once, so the device has a picture to open, and on
	// as long as the camera is off, so the last face it showed does not
	// stay on the host's screen.
	idleEvery = time.Second
	// keyframeEvery spaces the requests for a keyframe while the pictures
	// will not decode.
	keyframeEvery = 500 * time.Millisecond
)

// maxLate is how many packets the sample builder waits for one that went
// missing, and maxDelay how long; past either it drops the frame and asks
// for a keyframe.
const (
	maxLate  = 128
	maxDelay = 100 * time.Millisecond
)

// Options say where the camera goes.
type Options struct {
	// Device is a v4l2loopback device to write. Empty makes a PipeWire node
	// of the user's instead.
	Device string
	Log    *slog.Logger
}

// Camera is the browser's camera as a device on the host. It is there,
// black, from Start until Close; Play shows the browser's pictures on it.
type Camera struct {
	where  string
	log    *slog.Logger
	sink   frameWriter
	frames chan frame // fitted, for the writer
	free   chan frame // frames the writer is done with
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once

	playing sync.Mutex // held by the one Play at a time
}

// frameWriter is where finished frames go; gstsink.Sink is one.
type frameWriter interface {
	Write([]byte) error
	Close() error
}

// pipeline builds the gst-launch-1.0 arguments for the camera's child,
// which reads frames of the fixed size on stdin. The leaky queue drops
// frames the device cannot take yet rather than backing them up into the
// agent.
func pipeline(device, node string) []string {
	args := []string{
		"-q",
		"fdsrc", "fd=0", "blocksize=" + strconv.Itoa(Width*Height*3/2),
		"!", "rawvideoparse", "use-sink-caps=false", "format=i420",
		"width=" + strconv.Itoa(Width), "height=" + strconv.Itoa(Height), "framerate=" + strconv.Itoa(FPS) + "/1",
		"!", "queue", "leaky=downstream", "max-size-buffers=2", "max-size-bytes=0", "max-size-time=0",
		"!", "videoconvert",
	}
	if device != "" {
		return append(args, "!", "v4l2sink", "device="+device, "sync=false")
	}
	props := "props,media.class=Video/Source,media.role=Camera,node.name=" + node + ",node.description=" + gstsink.Quote(Name)
	return append(args, "!", "pipewiresink", "mode=provide", "sync=false", "stream-properties="+gstsink.Quote(props))
}

// Start puts the camera on the host, black until Play.
func Start(o Options) (*Camera, error) {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	if o.Device == "" {
		if err := havePipeWire(); err != nil {
			return nil, err
		}
	}
	if err := haveDecoder(); err != nil {
		return nil, err
	}
	where := o.Device
	if where == "" {
		where = "PipeWire"
	}
	s, err := gstsink.Start(pipeline(o.Device, "robin-camera-"+strconv.Itoa(os.Getpid())), gstsink.Options{Name: "camera", Log: log})
	if err != nil {
		return nil, err
	}
	c := newCamera(s, where, log)
	log.Info("camera up", "on", where)
	return c, nil
}

// haveDecoder fails when this build has no openh264, before a child is
// started for a camera that could show nothing.
func haveDecoder() error {
	d, err := h264.NewDecoder()
	if err == nil {
		d.Close()
	}
	return err
}

func newCamera(sink frameWriter, where string, log *slog.Logger) *Camera {
	c := &Camera{
		where: where, log: log, sink: sink,
		frames: make(chan frame, 1), free: make(chan frame, 3),
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	for range cap(c.free) {
		c.free <- newFrame()
	}
	go c.write()
	return c
}

// Where names what carries the camera on the host, for Ready: the
// v4l2loopback device's path, or PipeWire.
func (c *Camera) Where() string { return c.where }

// write hands frames to the device, and black while there are none.
func (c *Camera) write() {
	defer close(c.done)
	black := newFrame()
	black.black()
	idle := time.NewTimer(0)
	defer idle.Stop()
	failed := 0
	for {
		var f frame
		select {
		case <-c.stop:
			return
		case f = <-c.frames:
		case <-idle.C:
			f = black
		}
		if err := c.sink.Write(f); err != nil {
			if failed++; failed%100 == 1 {
				c.log.Warn("camera: the device took no picture", "err", err, "times", failed)
			}
		}
		if &f[0] != &black[0] {
			c.free <- f
		}
		idle.Reset(idleEvery)
	}
}

// Play shows one track of the browser's camera until the track ends.
// keyframe asks the browser for one; Play calls it when the pictures stop
// decoding. Tracks take turns: a second Play waits for the first.
func (c *Camera) Play(r track.Reader, keyframe func()) error {
	c.playing.Lock()
	defer c.playing.Unlock()
	dec, err := h264.NewDecoder()
	if err != nil {
		return err
	}
	defer dec.Close()
	sb := samplebuilder.New(maxLate, &codecs.H264Packet{}, 90000, samplebuilder.WithMaxTimeDelay(maxDelay))
	var fit *fitter
	var lastAsk time.Time
	var decoded, failed, dropped int
	defer func() {
		c.log.Info("camera track ended", "pictures", decoded, "undecodable", failed, "dropped", dropped)
	}()
	for {
		p, _, err := r.ReadRTP()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		sb.Push(p)
		broken := false
		for s := sb.Pop(); s != nil; s = sb.Pop() {
			if s.PrevDroppedPackets > 0 {
				broken = true
			}
			pic, err := dec.Decode(s.Data)
			if err != nil {
				failed++
				broken = true
				continue
			}
			if pic == nil {
				continue
			}
			decoded++
			if fit == nil || fit.srcW != pic.Width || fit.srcH != pic.Height {
				fit = newFitter(pic.Width, pic.Height)
				c.log.Info("camera picture", "width", pic.Width, "height", pic.Height)
			}
			if !c.show(fit, pic) {
				dropped++
			}
		}
		if broken && time.Since(lastAsk) >= keyframeEvery {
			lastAsk = time.Now()
			keyframe()
		}
	}
}

// show fits pic into a free frame and queues it, or reports false when the
// writer has none free or one already waiting.
func (c *Camera) show(fit *fitter, pic *h264.Picture) bool {
	var f frame
	select {
	case f = <-c.free:
	default:
		return false
	}
	fit.fit(f, pic)
	select {
	case c.frames <- f:
		return true
	default:
		c.free <- f
		return false
	}
}

// Close takes the camera off the host. It is safe to call more than once;
// a Play still running shows nothing after it.
func (c *Camera) Close() error {
	var err error
	c.once.Do(func() {
		close(c.stop)
		<-c.done
		err = c.sink.Close()
	})
	return err
}

// havePipeWire checks for the user's PipeWire before a pipeline needs it:
// pipewiresink without one fails in words that never say so.
func havePipeWire() error {
	if os.Getenv("PIPEWIRE_REMOTE") != "" {
		return nil
	}
	socket := filepath.Join(xdg.RuntimeDir(), "pipewire-0")
	if _, err := os.Stat(socket); err != nil {
		return fmt.Errorf("no PipeWire for this user at %s, and no v4l2loopback device this user can write", socket)
	}
	return nil
}
