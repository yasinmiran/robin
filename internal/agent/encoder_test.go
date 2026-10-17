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
	"testing"
	"time"

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

// encoders records which constructors ran, at what size; broken names fail
// with "no render node".
type encoders struct {
	opened []string
	broken map[string]bool
}

func (e *encoders) newFunc(name string) NewEncoderFunc {
	return func(w, h, _, _ int) (Encoder, error) {
		e.opened = append(e.opened, fmt.Sprintf("%s %dx%d", name, w, h))
		if e.broken[name] {
			return nil, errors.New("no render node")
		}
		return &fakeEncoder{sizes: make(chan [2]int, 1)}, nil
	}
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
			name, newEncoder, why, err := chooseEncoder(tc.start, e.table(), quietLog())
			switch {
			case tc.err != "":
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err %v, want %q", err, tc.err)
				}
			case err != nil:
				t.Fatal(err)
			case name != tc.want || newEncoder == nil || (tc.why == "") != (why == "") || !strings.Contains(why, tc.why):
				t.Fatalf("chose %s (why %q), want %s (%q)", name, why, tc.want, tc.why)
			}
			if !slices.Equal(e.opened, tc.opened) {
				t.Errorf("opened %q, want %q", e.opened, tc.opened)
			}
		})
	}
}

// TestPickedEncoderFallsBackForGood: hardware that passes the trial and
// fails at the session's size gives way to software, and is not tried again
// at every resize after that.
func TestPickedEncoderFallsBackForGood(t *testing.T) {
	e := &encoders{broken: map[string]bool{}}
	name, newEncoder, why, err := chooseEncoder(protocol.Start{Encoder: "vaapi", EncoderFallback: true}, e.table(), quietLog())
	if err != nil || name != "vaapi" || why != "" {
		t.Fatalf("chose %s (%q, %v)", name, why, err)
	}
	e.broken["vaapi"] = true
	for _, size := range [][2]int{{1920, 1080}, {1280, 720}} {
		if _, err := newEncoder(size[0], size[1], 30, startBitrate); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"vaapi 320x240", "vaapi 1920x1080", "software 1920x1080", "software 1280x720"}
	if !slices.Equal(e.opened, want) {
		t.Errorf("opened %q, want %q", e.opened, want)
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
