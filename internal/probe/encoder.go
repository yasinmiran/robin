package probe

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/yasinmiran/robin/internal/protocol"
)

// CheckEncoder reports whether the host has what the named video encoder
// needs. The software encoder is built into the agent. The hardware ones
// drive the GPU through ffmpeg or GStreamer on the host, so they need the
// GPU and one of those tools with the right encoder; the error says what is
// missing and, for a missing tool on a distribution whose ffmpeg is known to
// have both encoders, what to install.
func (r *Report) CheckEncoder(name string) error {
	var problem string
	var noTool bool
	switch name {
	case protocol.EncoderSoftware, "":
		return nil
	case protocol.EncoderVAAPI:
		problem, noTool = r.vaapiProblem()
	case protocol.EncoderNVENC:
		problem, noTool = r.nvencProblem()
	default:
		return fmt.Errorf("unknown encoder %q; want %s, %s or %s",
			name, protocol.EncoderSoftware, protocol.EncoderVAAPI, protocol.EncoderNVENC)
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
