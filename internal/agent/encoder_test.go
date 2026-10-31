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
	hwOpens := 0
	table := map[string]NewEncoderFunc{
		protocol.EncoderSoftware: func(int, int, int, int) (Encoder, error) { return &fakeEncoder{sizes: swEncoded}, nil },
		protocol.EncoderVAAPI: func(int, int, int, int) (Encoder, error) {
			hwOpens++
			if hwOpens == 1 {
				return &fakeEncoder{sizes: make(chan [2]int, 1)}, nil // the trial frame
			}
			return brokenToolEncoder{&hwEncodes}, nil
		},
	}
	v, err := chooseEncoder(protocol.Start{Encoder: protocol.EncoderVAAPI, EncoderFallback: true}, table, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if name, why := v.inUse(); name != protocol.EncoderVAAPI || why != "" {
		t.Fatalf("chose %s (%q)", name, why)
	}
	s := mustStream(newStream(&stillDesktop{}, 100, 64, 64, v.open, quietLog()))
	p, err := peer.New(peer.Config{
		MinBitrate: minBitrate, StartBitrate: startBitrate, MaxBitrate: protocol.DefaultMaxKbps * 1000, Log: quietLog(),
		Video: s.video,
	}, peer.Handlers{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	s.subscribe(p)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	select {
	case <-swEncoded:
	case <-time.After(3 * time.Second):
		t.Fatalf("hardware failed %d frames and software encoded none: the session has no video", hwEncodes.Load())
	}
}
