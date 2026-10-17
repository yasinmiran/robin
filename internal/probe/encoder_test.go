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

func TestAutoEncoder(t *testing.T) {
	intel := GPU{DRI: []DRINode{{Path: "/dev/dri/renderD128", RW: true}}}
	nvidia := GPU{NvidiaGPUs: []string{"NVIDIA GeForce RTX 3060"}}
	both := GPU{DRI: intel.DRI, NvidiaGPUs: nvidia.NvidiaGPUs}
	gst := map[string]string{"gst-launch-1.0": "/usr/bin/gst-launch-1.0"}
	ffVAAPI := []string{"h264_vaapi"}
	ffNVENC := []string{"h264_nvenc"}
	ffBoth := []string{"h264_vaapi", "h264_nvenc"}
	for _, tc := range []struct {
		name string
		r    Report
		w, h int
		want string
		why  string // a substring of the reason
	}{
		{"no GPU", Report{CPUs: 2}, 1280, 720, "software", "no GPU encoder"},
		// A workstation with the NVIDIA utilities and no driver loaded.
		{"nvidia-smi alone", Report{CPUs: 2, GPU: GPU{NvidiaSMI: "/usr/bin/nvidia-smi"}, FFmpegEncoders: ffNVENC}, 1280, 720, "software", "no GPU encoder"},
		{"render node nobody can drive", Report{CPUs: 2, GPU: intel}, 1280, 720, "software", "no GPU encoder"},

		{"vaapi, 20 CPUs, 1080p", Report{CPUs: 20, GPU: intel, FFmpegEncoders: ffVAAPI}, 1920, 1080, "software", "cheap enough at 1920x1080 on 20 CPUs"},
		{"vaapi, 20 CPUs, 4K through ffmpeg", Report{CPUs: 20, GPU: intel, FFmpegEncoders: ffVAAPI}, 3840, 2160, "vaapi", "through ffmpeg: sessions reach 3840x2160"},
		{"vaapi, 20 CPUs, 1440p through ffmpeg", Report{CPUs: 20, GPU: intel, FFmpegEncoders: ffVAAPI}, 2560, 1440, "vaapi", "2560x1440"},
		{"vaapi, 20 CPUs, 4K through GStreamer only", Report{CPUs: 20, GPU: intel, Commands: gst, GstPlugins: []string{"va"}}, 3840, 2160, "software", "needs ffmpeg"},
		{"vaapi, 4 CPUs, GStreamer", Report{CPUs: 4, GPU: intel, Commands: gst, GstPlugins: []string{"vaapi"}}, 1920, 1080, "vaapi", "4 CPUs"},
		{"vaapi, 5 CPUs, 1080p", Report{CPUs: 5, GPU: intel, FFmpegEncoders: ffVAAPI}, 1920, 1080, "software", "5 CPUs"},
		{"vaapi, cores unknown", Report{GPU: intel, FFmpegEncoders: ffVAAPI}, 1920, 1080, "software", "cheap enough"},

		{"nvenc only, 4 CPUs", Report{CPUs: 4, GPU: nvidia, Commands: gst, GstPlugins: []string{"nvcodec"}}, 1920, 1080, "nvenc", "4 CPUs"},
		{"nvenc only, 4K through ffmpeg", Report{CPUs: 16, GPU: nvidia, FFmpegEncoders: ffNVENC}, 3840, 2160, "nvenc", "through ffmpeg"},
		{"both, 2 CPUs", Report{CPUs: 2, GPU: both, FFmpegEncoders: ffBoth}, 1280, 720, "vaapi", "2 CPUs"},
		{"both, 4K through ffmpeg", Report{CPUs: 32, GPU: both, FFmpegEncoders: ffBoth}, 3840, 2160, "vaapi", "through ffmpeg"},
		{"both, 4K, ffmpeg drives NVENC only", Report{CPUs: 32, GPU: both, FFmpegEncoders: ffNVENC, Commands: gst, GstPlugins: []string{"va"}}, 3840, 2160, "nvenc", "nvenc through ffmpeg"},
		{"both, 1080p, 32 CPUs", Report{CPUs: 32, GPU: both, FFmpegEncoders: ffBoth}, 1920, 1080, "software", "vaapi or nvenc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, why := tc.r.AutoEncoder(tc.w, tc.h)
			if got != tc.want || !strings.Contains(why, tc.why) {
				t.Errorf("AutoEncoder(%d, %d) = %s (%q), want %s (%q)", tc.w, tc.h, got, why, tc.want, tc.why)
			}
		})
	}
}

func TestAutoEncoderOnRealProbe(t *testing.T) {
	// 20 CPUs and an Intel iGPU whose iHD driver encodes H.264.
	r := parseFile(t, "real-ubuntu-24.04-intel-vaapi.txt")
	if got, why := r.AutoEncoder(3840, 2160); got != "software" {
		t.Errorf("no tool to drive the GPU, but picked %s (%s)", got, why)
	}
	r.FFmpegEncoders = []string{"h264_vaapi"}
	if got, why := r.AutoEncoder(1920, 1080); got != "software" {
		t.Errorf("1080p on 20 CPUs: picked %s (%s)", got, why)
	}
	if got, why := r.AutoEncoder(3840, 2160); got != "vaapi" {
		t.Errorf("4K with ffmpeg: picked %s (%s)", got, why)
	}
}
