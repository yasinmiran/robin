package probe

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/yasinmiran/robin/internal/protocol"
)

// CheckEncoder reports whether the host has what the named video encoder
// needs. The software encoder is built into the agent, and auto falls back
// to it. The hardware ones drive the GPU through ffmpeg or GStreamer on the
// host, so they need the GPU and one of those tools with the right encoder;
// the error says what is missing and, for a missing tool on a distribution
// whose ffmpeg is known to have both encoders, what to install.
func (r *Report) CheckEncoder(name string) error {
	var problem string
	var noTool bool
	switch name {
	case protocol.EncoderSoftware, protocol.EncoderAuto, "":
		return nil
	case protocol.EncoderVAAPI:
		problem, noTool = r.vaapiProblem()
	case protocol.EncoderNVENC:
		problem, noTool = r.nvencProblem()
	default:
		return fmt.Errorf("unknown encoder %q; want %s, %s, %s or %s",
			name, protocol.EncoderAuto, protocol.EncoderSoftware, protocol.EncoderVAAPI, protocol.EncoderNVENC)
	}
	if problem == "" {
		return nil
	}
	msg := fmt.Sprintf("--encoder %s cannot run on this host: %s", name, problem)
	if pkg, ok := encoderTool[r.packageManager()]; ok && noTool {
		sudo := "sudo "
		if r.UID == 0 {
			sudo = ""
		}
		msg += "\ninstall with: " + sudo + installCommands[r.packageManager()] + " " + pkg
	}
	return errors.New(msg)
}

// encoderTool is the package that brings both hardware encoders, on the
// distributions whose ffmpeg is known to be built with them.
var encoderTool = map[string]string{"apt": "ffmpeg", "pacman": "ffmpeg"}

// vaapiProblem says what VA-API encoding lacks, and whether that is the
// tool rather than the GPU.
func (r *Report) vaapiProblem() (string, bool) {
	var nodes, usable []string
	for _, n := range r.GPU.DRI {
		if strings.Contains(n.Path, "/renderD") {
			nodes = append(nodes, n.Path)
			if n.RW {
				usable = append(usable, n.Path)
			}
		}
	}
	switch {
	case len(nodes) == 0:
		return "no GPU render node in /dev/dri", false
	case len(usable) == 0:
		user := r.User
		if user == "" {
			user = "this user"
		}
		return fmt.Sprintf("%s cannot open %s; add them to the group that owns it, usually render", user, strings.Join(nodes, ", ")), false
	// vainfo is optional; when it ran and found no H.264 encoder, believe it.
	case r.GPU.VAInfo != "" && !r.GPU.VAH264Encode:
		driver := r.GPU.VADriver
		if driver == "" {
			driver = "no working VA-API driver"
		}
		return fmt.Sprintf("vainfo reports no H.264 encoding on %s (%s)", usable[0], driver), false
	}
	gst := r.Commands["gst-launch-1.0"] != "" &&
		(slices.Contains(r.GstPlugins, "va") || slices.Contains(r.GstPlugins, "vaapi"))
	if gst || slices.Contains(r.FFmpegEncoders, "h264_vaapi") {
		return "", false
	}
	return "it needs ffmpeg with h264_vaapi, or gst-launch-1.0 with GStreamer's va or vaapi plugin", true
}

func (r *Report) nvencProblem() (string, bool) {
	// nvidia-smi alone proves nothing: it is installed on hosts with no GPU.
	if len(r.GPU.NvidiaGPUs) == 0 {
		return "no NVIDIA GPU with its driver loaded", false
	}
	gst := r.Commands["gst-launch-1.0"] != "" && slices.Contains(r.GstPlugins, "nvcodec")
	if gst || slices.Contains(r.FFmpegEncoders, "h264_nvenc") {
		return "", false
	}
	return "it needs ffmpeg with h264_nvenc, or gst-launch-1.0 with GStreamer's nvcodec plugin", true
}

// Where a hardware encoder pays for the latency it adds: a few milliseconds
// a frame through the pipe and about 40 ms per requested keyframe, against
// the CPU openh264 needs. See the ADR's measurements.
const (
	// autoHardwareMaxCPUs is the most cores a host may have for the GPU to
	// take a 1080p session: openh264 needs up to a quarter of a fast core at
	// 1080p30, which a host this small feels.
	autoHardwareMaxCPUs = 4
	// autoHardwareMinPixels is the session size above which the GPU encodes
	// on any host: at 2160p openh264 spends up to 28 ms of CPU a frame on
	// busy content, ffmpeg's VA-API route 12 ms. At 1440p, the default on
	// hosts with more than autoHardwareMaxCPUs cores, openh264 needs 5 to
	// 17 ms, which such a host affords for the latency it saves.
	autoHardwareMinPixels = 2560*1440 + 1
)

// AutoEncoder picks the encoder for --encoder auto, for a session of at most
// maxWidth x maxHeight, and says why. It takes the GPU only where the probe
// shows a working route and the GPU pays: on a host with few cores, or for a
// session larger than 1080p through ffmpeg. GStreamer's VA-API route spends
// a whole frame interval on a 2160p frame for little less CPU than
// openh264, so it counts on small hosts only. VA-API comes before NVENC,
// which has not run on NVIDIA hardware.
func (r *Report) AutoEncoder(maxWidth, maxHeight int) (string, string) {
	small := r.CPUs > 0 && r.CPUs <= autoHardwareMaxCPUs
	large := maxWidth*maxHeight >= autoHardwareMinPixels
	size := fmt.Sprintf("%dx%d", maxWidth, maxHeight)
	var found []string
	for _, c := range []struct {
		name, ffmpeg string
		problem      func() (string, bool)
	}{
		{protocol.EncoderVAAPI, "h264_vaapi", r.vaapiProblem},
		{protocol.EncoderNVENC, "h264_nvenc", r.nvencProblem},
	} {
		if problem, _ := c.problem(); problem != "" {
			continue
		}
		found = append(found, c.name)
		ffmpeg := slices.Contains(r.FFmpegEncoders, c.ffmpeg)
		switch {
		case small:
			return c.name, fmt.Sprintf("%s: the host has %d CPUs", c.name, r.CPUs)
		case large && ffmpeg:
			return c.name, fmt.Sprintf("%s through ffmpeg: sessions reach %s", c.name, size)
		}
	}
	switch {
	case len(found) == 0:
		return protocol.EncoderSoftware, "software: no GPU encoder on the host"
	case large:
		return protocol.EncoderSoftware, fmt.Sprintf("software: %s needs ffmpeg to pay at %s on %d CPUs",
			strings.Join(found, " and "), size, r.CPUs)
	}
	return protocol.EncoderSoftware, fmt.Sprintf("software: openh264 is cheap enough at %s on %d CPUs, and lower latency than %s",
		size, r.CPUs, strings.Join(found, " or "))
}
