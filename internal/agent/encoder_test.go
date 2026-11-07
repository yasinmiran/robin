//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/robin/internal/agent/peer"
	"github.com/yasinmiran/robin/internal/agent/yuv"
	"github.com/yasinmiran/robin/internal/protocol"
)

// TestSessionReportsBrokenEncoder checks that an encoder whose host tools
// fail is reported before the desktop starts, not at the first frame.
func TestSessionReportsBrokenEncoder(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	cli := protocol.NewConn(outR, inW, protocol.DefaultPingInterval, 0)
	defer cli.Close()
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), inR, outW, Options{
			RunRoot: t.TempDir(),
			Encoders: map[string]NewEncoderFunc{
				protocol.EncoderVAAPI: func(int, int, int, int) (Encoder, error) {
					return nil, errors.New("no render node")
				},
			},
		})
		outW.Close()
	}()
	cli.Send(protocol.Message{Type: protocol.TypeStart, Start: &protocol.Start{Version: protocol.Version, Encoder: protocol.EncoderVAAPI}})
	m, err := cli.Recv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != protocol.TypeError || !strings.Contains(m.Error, "vaapi") || !strings.Contains(m.Error, "no render node") {
		t.Fatalf("got %+v", m)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("session ended cleanly")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session still running")
	}
}

// encoders records which constructors ran, at what size, and each frame
// their encoders were asked for, with whether it was to be a keyframe;
// broken names fail with "no render node", and failing names open but fail
// every frame, as a host tool that cannot encode the size does.
type encoders struct {
	opened  []string
	encodes []string
	broken  map[string]bool
	failing map[string]bool
}

func (e *encoders) newFunc(name string) NewEncoderFunc {
	return func(w, h, _, _ int) (Encoder, error) {
		e.opened = append(e.opened, fmt.Sprintf("%s %dx%d", name, w, h))
		switch {
		case e.broken[name]:
			return nil, errors.New("no render node")
		case e.failing[name]:
			return loggedEncoder{brokenToolEncoder{new(atomic.Int64)}, name, &e.encodes}, nil
		}
		return loggedEncoder{&fakeEncoder{sizes: make(chan [2]int, 1)}, name, &e.encodes}, nil
	}
}

// loggedEncoder appends each frame it is asked for to log.
type loggedEncoder struct {
	Encoder
	name string
	log  *[]string
}

func (l loggedEncoder) Encode(img *yuv.I420, ts time.Duration, keyframe bool) ([]byte, error) {
	*l.log = append(*l.log, fmt.Sprintf("%s %dx%d keyframe=%t", l.name, img.Width, img.Height, keyframe))
	return l.Encoder.Encode(img, ts, keyframe)
}

func (e *encoders) table() map[string]NewEncoderFunc {
	return map[string]NewEncoderFunc{
		protocol.EncoderSoftware: e.newFunc("software"),
		protocol.EncoderVAAPI:    e.newFunc("vaapi"),
	}
}

func TestChooseEncoder(t *testing.T) {
	for _, tc := range []struct {
		name     string
		start    protocol.Start
		broken   string
		want     string
		why, err string // substrings; "" wants none
		opened   []string
	}{
		{"software by default", protocol.Start{}, "", "software", "", "", []string{"software 320x240"}},
		{"named hardware works", protocol.Start{Encoder: "vaapi"}, "", "vaapi", "", "", []string{"vaapi 320x240"}},
		{"named hardware fails", protocol.Start{Encoder: "vaapi"}, "vaapi", "", "", "encoder vaapi: no render node", []string{"vaapi 320x240"}},
		{"picked hardware works", protocol.Start{Encoder: "vaapi", EncoderFallback: true}, "", "vaapi", "", "", []string{"vaapi 320x240"}},
		{"picked hardware fails", protocol.Start{Encoder: "vaapi", EncoderFallback: true}, "vaapi", "software", "vaapi: no render node", "",
			[]string{"vaapi 320x240", "software 320x240"}},
		{"named encoder this agent lacks", protocol.Start{Encoder: "nvenc"}, "", "", "", `encoder "nvenc" is not available`, nil},
		{"picked encoder this agent lacks", protocol.Start{Encoder: "nvenc", EncoderFallback: true}, "", "software", "nvenc is not available", "", nil},
		{"software itself never falls", protocol.Start{Encoder: "software", EncoderFallback: true}, "software", "", "", "encoder software: no render node",
			[]string{"software 320x240"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &encoders{broken: map[string]bool{tc.broken: true}}
			v, err := chooseEncoder(tc.start, e.table(), quietLog())
			switch {
			case tc.err != "":
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err %v, want %q", err, tc.err)
				}
			case err != nil:
				t.Fatal(err)
			default:
				if name, why := v.inUse(); name != tc.want || (tc.why == "") != (why == "") || !strings.Contains(why, tc.why) {
					t.Fatalf("chose %s (why %q), want %s (%q)", name, why, tc.want, tc.why)
				}
			}
			if !slices.Equal(e.opened, tc.opened) {
				t.Errorf("opened %q, want %q", e.opened, tc.opened)
			}
		})
	}
}

// TestPickedEncoderFallsBackForGood: hardware that passes the trial and
// fails at the session's size where a host tool does, at its first frame,
// gives way to software, which encodes that frame as a keyframe, since the
// page's decoder has only the hardware stream before it; hardware is not
// tried again at every resize after that, and Ready says why.
func TestPickedEncoderFallsBackForGood(t *testing.T) {
	e := &encoders{failing: map[string]bool{}}
	v, err := chooseEncoder(protocol.Start{Encoder: "vaapi", EncoderFallback: true}, e.table(), quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if name, why := v.inUse(); name != "vaapi" || why != "" {
		t.Fatalf("chose %s (%q)", name, why)
	}
	e.failing["vaapi"] = true
	for _, size := range [][2]int{{1920, 1080}, {1280, 720}} {
		enc, err := v.open(size[0], size[1], 30, startBitrate)
		if err != nil {
			t.Fatal(err)
		}
		if au, err := enc.Encode(yuv.NewI420(size[0], size[1]), 0, false); err != nil || len(au) == 0 {
			t.Fatalf("%dx%d: encoded %d bytes, err %v", size[0], size[1], len(au), err)
		}
		enc.Close()
	}
	want := []string{"vaapi 320x240", "vaapi 1920x1080", "software 1920x1080", "software 1280x720"}
	if !slices.Equal(e.opened, want) {
		t.Errorf("opened %q, want %q", e.opened, want)
	}
	want = []string{"vaapi 1920x1080 keyframe=false", "software 1920x1080 keyframe=true", "software 1280x720 keyframe=false"}
	if !slices.Equal(e.encodes, want) {
		t.Errorf("encoded %q, want %q", e.encodes, want)
	}
	r := (&session{encoder: v}).resumedReady(&protocol.Ready{Encoder: "vaapi"})
	if r.Encoder != protocol.EncoderSoftware || !strings.Contains(r.EncoderError, "vaapi: hwenc") {
		t.Errorf("a resumed ready reports encoder %s (%q), want software because vaapi failed", r.Encoder, r.EncoderError)
	}
}

// TestNamedEncoderDoesNotFallBack: an encoder the user named that fails at
// the session's size keeps failing, and software never opens.
func TestNamedEncoderDoesNotFallBack(t *testing.T) {
	e := &encoders{failing: map[string]bool{}}
	v, err := chooseEncoder(protocol.Start{Encoder: "vaapi"}, e.table(), quietLog())
	if err != nil {
		t.Fatal(err)
	}
	e.failing["vaapi"] = true
	enc, err := v.open(1920, 1080, 30, startBitrate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Encode(yuv.NewI420(1920, 1080), 0, true); err == nil {
		t.Fatal("a named encoder's failure was hidden")
	}
	if name, why := v.inUse(); name != "vaapi" || why != "" || slices.Contains(e.opened, "software 1920x1080") {
		t.Errorf("in use %s (%q) after opening %q, want vaapi alone", name, why, e.opened)
	}
}

// TestSessionFallsBackToSoftware runs a whole session whose picked hardware
// encoder fails: the desktop still comes up, on software video, and Ready
// says why.
func TestSessionFallsBackToSoftware(t *testing.T) {
	e := &encoders{broken: map[string]bool{"vaapi": true}}
	h := startAgent(t, func(o *Options) {
		o.Encoders[protocol.EncoderVAAPI] = e.newFunc("vaapi")
	})
	ready := h.start(func(s *protocol.Start) { s.Encoder, s.EncoderFallback = protocol.EncoderVAAPI, true })
	if ready.Encoder != protocol.EncoderSoftware || !strings.Contains(ready.EncoderError, "vaapi: no render node") {
		t.Errorf("ready reports encoder %s (%q), want software because vaapi failed", ready.Encoder, ready.EncoderError)
	}
	h.stdin.Close()
	h.waitGone(ready.Display)
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// brokenToolEncoder opens like a hardware encoder whose host tool starts
// and fails at every frame, as hwenc does when the tool cannot encode the
// session's size: the tool runs only from the first Encode.
type brokenToolEncoder struct{ encodes *atomic.Int64 }

func (e brokenToolEncoder) Encode(*yuv.I420, time.Duration, bool) ([]byte, error) {
	e.encodes.Add(1)
	return nil, errors.New("hwenc: ffmpeg h264_vaapi: exited: unsupported resolution")
}
func (brokenToolEncoder) SetBitrate(int) error { return nil }
func (brokenToolEncoder) Close() error         { return nil }

// TestPickedEncoderFallsBackWhenEncodeFails: a hardware encoder the CLI
// picked that opens at the session's size and then fails on every frame
// must give way to software. The trial frame succeeds: the stand-in for it
// encodes.
func TestPickedEncoderFallsBackWhenEncodeFails(t *testing.T) {
	var hwEncodes atomic.Int64
	swEncoded := make(chan [2]int, 1)
	ended := streamThroughVAAPI(t, true, func(int, int, int, int) (Encoder, error) { return &fakeEncoder{sizes: swEncoded}, nil },
		func() Encoder { return brokenToolEncoder{&hwEncodes} })
	select {
	case <-swEncoded:
	case <-time.After(3 * time.Second):
		t.Fatalf("hardware failed %d frames and software encoded none: the session has no video", hwEncodes.Load())
	}
	select {
	case err := <-ended:
		t.Errorf("the stream ended (%v) on a picked encoder with software to take over", err)
	default:
	}
}

// streamThroughVAAPI runs a stream of a still desktop to a peer through
// vaapi, picked by the CLI or named by the user, whose trial frame encodes
// and whose encoders after it come from next; software opens what sw
// makes. It returns a channel that yields what the stream's run returned
// once it has, then is closed.
func streamThroughVAAPI(t *testing.T, picked bool, sw NewEncoderFunc, next func() Encoder) <-chan error {
	t.Helper()
	var opens atomic.Int64
	table := map[string]NewEncoderFunc{
		protocol.EncoderSoftware: sw,
		protocol.EncoderVAAPI: func(int, int, int, int) (Encoder, error) {
			if opens.Add(1) == 1 {
				return &fakeEncoder{sizes: make(chan [2]int, 1)}, nil // the trial frame
			}
			return next(), nil
		},
	}
	v, err := chooseEncoder(protocol.Start{Encoder: protocol.EncoderVAAPI, EncoderFallback: picked}, table, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if name, why := v.inUse(); name != protocol.EncoderVAAPI || why != "" {
		t.Fatalf("chose %s (%q)", name, why)
	}
	s := mustStream(newStream(&stillDesktop{}, 100, 64, 64, v.open, quietLog()))
	// A still screen goes out again soon, so frames keep coming to fail or
	// encode.
	s.refresh = 10 * time.Millisecond
	p, err := peer.New(peer.Config{
		MinBitrate: minBitrate, StartBitrate: startBitrate, MaxBitrate: protocol.DefaultMaxKbps * 1000, Log: quietLog(),
		Video: s.video,
	}, peer.Handlers{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	s.subscribe(p)
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan error, 1)
	go func() {
		ended <- s.run(ctx)
		close(ended)
	}()
	t.Cleanup(func() {
		cancel()
		for range ended {
		}
	})
	return ended
}

// noSoftware is the software encoder of a test where only the named
// hardware one may open.
func noSoftware(t *testing.T) NewEncoderFunc {
	return func(int, int, int, int) (Encoder, error) {
		t.Error("software opened in place of the encoder the user named")
		return nil, errors.New("software is not for this test")
	}
}

// backoffEncoder fails as hwenc does when its host tool cannot encode the
// size: the tool exits on a frame, and until it is started again a second
// later each frame gets no data and no error, as one rate control skipped.
// quiet is how many frames that pause lasts.
type backoffEncoder struct {
	calls *atomic.Int64
	quiet int64
}

func (e backoffEncoder) Encode(*yuv.I420, time.Duration, bool) ([]byte, error) {
	if (e.calls.Add(1)-1)%(e.quiet+1) != 0 {
		return nil, nil
	}
	return nil, errors.New("hwenc: ffmpeg h264_vaapi: exited: unsupported resolution")
}
func (backoffEncoder) SetBitrate(int) error { return nil }
func (backoffEncoder) Close() error         { return nil }

// TestNamedEncoderThatKeepsFailingEndsTheStream: an encoder the user named
// that passes its trial and then fails every frame at the session's size,
// with the pauses hwenc keeps between restarts of its tool, gives up the
// stream at its maxFailedFrames'th failure with the encoder's error, which
// ends the session, instead of leaving the page without video for ever.
func TestNamedEncoderThatKeepsFailingEndsTheStream(t *testing.T) {
	var calls atomic.Int64
	const quiet = 4
	ended := streamThroughVAAPI(t, false, noSoftware(t), func() Encoder { return backoffEncoder{&calls, quiet} })
	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("the stream ended without saying why")
		}
		for _, want := range []string{"64x64", fmt.Sprintf("%d failed frames", maxFailedFrames), "unsupported resolution"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the stream gave up with %q, want it to say %q", err, want)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the encoder failed %d times in %d frames and the stream goes on without video", (calls.Load()+quiet)/(quiet+1), calls.Load())
	}
	if got, want := calls.Load(), int64((maxFailedFrames-1)*(quiet+1)+1); got != want {
		t.Errorf("the encoder was asked for %d frames, want %d: up to its last failure and none after", got, want)
	}
}

// flakyEncoder fails maxFailedFrames-1 frames running, one fewer than gives
// up the stream, then encodes one, over and over.
type flakyEncoder struct {
	calls   *atomic.Int64
	encoded chan struct{}
}

func (e flakyEncoder) Encode(*yuv.I420, time.Duration, bool) ([]byte, error) {
	if e.calls.Add(1)%maxFailedFrames != 0 {
		return nil, errors.New("hwenc: ffmpeg h264_vaapi: exited: signal: killed")
	}
	select {
	case e.encoded <- struct{}{}:
	default:
	}
	return []byte{0, 0, 0, 1, 0x65, 0x88, 0x84, 0x21}, nil
}
func (flakyEncoder) SetBitrate(int) error { return nil }
func (flakyEncoder) Close() error         { return nil }

// TestNamedEncoderThatFailsSomeFramesKeepsStreaming: a named encoder whose
// failed frames come between frames it encodes only skips those frames.
func TestNamedEncoderThatFailsSomeFramesKeepsStreaming(t *testing.T) {
	encoded := make(chan struct{}, 1)
	ended := streamThroughVAAPI(t, false, noSoftware(t), func() Encoder { return flakyEncoder{new(atomic.Int64), encoded} })
	for i := range 5 {
		select {
		case <-encoded:
		case err := <-ended:
			t.Fatalf("the stream ended (%v) after %d good frames", err, i)
		case <-time.After(5 * time.Second):
			t.Fatalf("%d good frames, then none", i)
		}
	}
	select {
	case err := <-ended:
		t.Fatalf("the stream ended (%v) on an encoder that still encodes", err)
	default:
	}
}

// TestSessionEndsWhenANamedEncoderKeepsFailing runs a whole session whose
// named encoder passes its trial and then fails every frame at the
// desktop's size: the CLI hears the encoder's error and the session ends,
// leaving nothing behind.
func TestSessionEndsWhenANamedEncoderKeepsFailing(t *testing.T) {
	var opens atomic.Int64
	h := startAgent(t, func(o *Options) {
		o.Encoders[protocol.EncoderVAAPI] = func(int, int, int, int) (Encoder, error) {
			if opens.Add(1) == 1 {
				return &fakeEncoder{sizes: make(chan [2]int, 1)}, nil // the trial frame
			}
			return brokenToolEncoder{new(atomic.Int64)}, nil
		}
	})
	ready := h.start(func(s *protocol.Start) { s.Encoder = protocol.EncoderVAAPI })
	if ready.Encoder != protocol.EncoderVAAPI {
		t.Fatalf("ready reports encoder %s (%q), want vaapi", ready.Encoder, ready.EncoderError)
	}
	answer(t, h, offerFromBrowser(t, h, webrtc.RTPCodecTypeVideo))
	wantErr := func(what, got string) {
		for _, want := range []string{"encoder vaapi", "failed frames", "unsupported resolution"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s %q, want it to say %q", what, got, want)
			}
		}
	}
	deadline := time.After(20 * time.Second)
	for heard := false; !heard; {
		select {
		case m, ok := <-h.msgs:
			if !ok {
				t.Fatalf("the agent went without a word to the CLI (Run: %v)", h.runErr)
			}
			if m.Type == protocol.TypeError {
				wantErr("the CLI heard", m.Error)
				heard = true
			}
		case <-deadline:
			t.Fatal("the session goes on without video and the CLI hears nothing")
		}
	}
	err := h.waitExit(ready.Display)
	if err == nil {
		t.Fatal("Run ended cleanly")
	}
	wantErr("Run returned", err.Error())
}
