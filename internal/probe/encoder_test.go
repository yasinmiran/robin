package probe

import (
	"strings"
	"testing"
)

func TestCheckEncoder(t *testing.T) {
	ubuntu := Distro{ID: "ubuntu", IDLike: []string{"debian"}}
	intel := GPU{
		DRI:      []DRINode{{Path: "/dev/dri/card1", RW: true}, {Path: "/dev/dri/renderD128", RW: true}},
		VAInfo:   "/usr/bin/vainfo",
		VADriver: "Intel iHD driver for Intel(R) Gen Graphics - 24.1.0 ()", VAH264Encode: true,
	}
	nvidia := GPU{NvidiaGPUs: []string{"NVIDIA GeForce RTX 3060"}}
	gst := map[string]string{"gst-launch-1.0": "/usr/bin/gst-launch-1.0"}
	for _, tc := range []struct {
		name, encoder string
		r             Report
		want          []string // substrings of the error; none means no error
	}{
		{"software needs nothing", "software", Report{}, nil},
		{"default is software", "", Report{}, nil},
		{"unknown", "quicksync", Report{}, []string{`unknown encoder "quicksync"`}},

		{"vaapi through gstreamer", "vaapi", Report{GPU: intel, Commands: gst, GstPlugins: []string{"vaapi"}}, nil},
		{"vaapi through the va plugin", "vaapi", Report{GPU: intel, Commands: gst, GstPlugins: []string{"va"}}, nil},
		{"vaapi through ffmpeg without vainfo", "vaapi",
			Report{GPU: GPU{DRI: []DRINode{{Path: "/dev/dri/renderD128", RW: true}}}, FFmpegEncoders: []string{"h264_vaapi"}}, nil},
		{"no render node", "vaapi", Report{GPU: GPU{DRI: []DRINode{{Path: "/dev/dri/card0", RW: true}}}, FFmpegEncoders: []string{"h264_vaapi"}},
			[]string{"no GPU render node"}},
		{"render node not ours", "vaapi",
			Report{User: "alice", GPU: GPU{DRI: []DRINode{{Path: "/dev/dri/renderD128"}}}, FFmpegEncoders: []string{"h264_vaapi"}},
			[]string{"alice cannot open /dev/dri/renderD128", "render"}},
		{"driver without h264 encode", "vaapi",
			Report{GPU: GPU{DRI: intel.DRI, VAInfo: "/usr/bin/vainfo", VADriver: "Mesa Gallium driver"}, FFmpegEncoders: []string{"h264_vaapi"}},
			[]string{"no H.264 encoding on /dev/dri/renderD128 (Mesa Gallium driver)"}},
		{"gstreamer without a vaapi plugin", "vaapi", Report{Distro: ubuntu, UID: 1000, GPU: intel, Commands: gst, GstPlugins: []string{"x264"}},
			[]string{"needs ffmpeg with h264_vaapi", "install with: sudo apt install ffmpeg"}},
		{"plugin without gst-launch", "vaapi", Report{Distro: ubuntu, UID: 0, GPU: intel, GstPlugins: []string{"vaapi"}},
			[]string{"needs ffmpeg", "install with: apt install ffmpeg"}},
		{"no hint on an unknown distro", "vaapi", Report{Distro: Distro{ID: "gentoo"}, GPU: intel}, []string{"needs ffmpeg"}},

		{"nvenc through ffmpeg", "nvenc", Report{GPU: nvidia, FFmpegEncoders: []string{"h264_nvenc"}}, nil},
		{"nvenc through gstreamer", "nvenc", Report{GPU: nvidia, Commands: gst, GstPlugins: []string{"nvcodec"}}, nil},
		{"no nvidia gpu", "nvenc", Report{GPU: intel, FFmpegEncoders: []string{"h264_nvenc"}}, []string{"no NVIDIA GPU"}},
		// Seen on a workstation with the NVIDIA utilities and no NVIDIA card.
		{"nvidia-smi without a driver", "nvenc", Report{GPU: GPU{NvidiaSMI: "/usr/bin/nvidia-smi"}, FFmpegEncoders: []string{"h264_nvenc"}},
			[]string{"no NVIDIA GPU"}},
		{"nvidia without a tool", "nvenc", Report{Distro: Distro{ID: "arch"}, UID: 1000, GPU: nvidia, FFmpegEncoders: []string{"h264_vaapi"}},
			[]string{"needs ffmpeg with h264_nvenc", "install with: sudo pacman -S ffmpeg"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.r.CheckEncoder(tc.encoder)
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("no error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error lacks %q:\n%v", w, err)
				}
			}
			if strings.Contains(err.Error(), "install with") && !strings.Contains(strings.Join(tc.want, " "), "install with") {
				t.Errorf("unexpected install hint:\n%v", err)
			}
		})
	}
}

func TestCheckEncoderOnRealProbe(t *testing.T) {
	// A root container on an Intel iGPU with vainfo and nothing to drive it.
	r := parseFile(t, "real-ubuntu-24.04-intel-vaapi.txt")
	err := r.CheckEncoder("vaapi")
	if err == nil || !strings.Contains(err.Error(), "install with: apt install ffmpeg") {
		t.Fatalf("got %v", err)
	}
	r.FFmpegEncoders = []string{"h264_vaapi"}
	if err := r.CheckEncoder("vaapi"); err != nil {
		t.Fatal(err)
	}
}
