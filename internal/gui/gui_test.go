package gui

import (
	"strings"
	"testing"

	"github.com/yasinmiran/robin/internal/probe"
	"github.com/yasinmiran/robin/internal/protocol"
)

func TestStartMessageDefaults(t *testing.T) {
	for _, tc := range []struct {
		name         string
		o            Options
		cpus         int
		w, h, fps, k int
	}{
		{"defaults", Options{}, 8, 1920, 1080, 30, 10_000},
		{"small host", Options{}, 2, 1280, 720, 20, 10_000},
		{"unknown cores", Options{}, 0, 1920, 1080, 30, 10_000},
		{"asked for", Options{MaxWidth: 2560, MaxHeight: 1440, FPS: 60, MaxKbps: 4000}, 2, 2560, 1440, 60, 4000},
	} {
		s, _ := startMessage(tc.o, &probe.Report{CPUs: tc.cpus})
		if s.MaxWidth != tc.w || s.MaxHeight != tc.h || s.FPS != tc.fps || s.MaxKbps != tc.k {
			t.Errorf("%s: %dx%d at %d fps, %d kbps; want %dx%d at %d fps, %d kbps",
				tc.name, s.MaxWidth, s.MaxHeight, s.FPS, s.MaxKbps, tc.w, tc.h, tc.fps, tc.k)
		}
	}
}

// TestStartMessageEncoder: auto becomes the probe's pick, judged at the
// session's maximum size, and only a hardware pick may fall back; an
// encoder the user named goes out as named, without fallback.
func TestStartMessageEncoder(t *testing.T) {
	vaapi := probe.Report{
		CPUs: 20, FFmpegEncoders: []string{"h264_vaapi"},
		GPU: probe.GPU{DRI: []probe.DRINode{{Path: "/dev/dri/renderD128", RW: true}}},
	}
	for _, tc := range []struct {
		name     string
		o        Options
		r        probe.Report
		encoder  string
		fallback bool
		why      string
	}{
		{"auto, no GPU", Options{Encoder: "auto"}, probe.Report{CPUs: 8}, "software", false, "no GPU encoder"},
		{"auto, 1080p on 20 CPUs", Options{Encoder: "auto"}, vaapi, "software", false, "cheap enough"},
		{"auto, 4K on 20 CPUs", Options{Encoder: "auto", MaxWidth: 3840, MaxHeight: 2160}, vaapi, "vaapi", true, "3840x2160"},
		{"named vaapi", Options{Encoder: "vaapi"}, vaapi, "vaapi", false, ""},
		{"named software", Options{Encoder: "software", MaxWidth: 3840, MaxHeight: 2160}, vaapi, "software", false, ""},
	} {
		s, why := startMessage(tc.o, &tc.r)
		if s.Encoder != tc.encoder || s.EncoderFallback != tc.fallback || !strings.Contains(why, tc.why) || (tc.why == "") != (why == "") {
			t.Errorf("%s: encoder %s, fallback %v, why %q; want %s, %v, %q",
				tc.name, s.Encoder, s.EncoderFallback, why, tc.encoder, tc.fallback, tc.why)
		}
		if s.Encoder == protocol.EncoderAuto {
			t.Errorf("%s: auto went out to the agent", tc.name)
		}
	}
}
