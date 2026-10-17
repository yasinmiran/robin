//go:build linux

package hwenc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// config is what one run of a tool is started with.
type config struct {
	width, height, fps int
	bitrate            int // bits per second
}

// route is one way to reach the hardware: a tool, its arguments and the
// framing of its output.
type route struct {
	name    string // for logs and errors, e.g. "ffmpeg h264_vaapi on /dev/dri/renderD128"
	path    string
	args    func(config) []string
	env     []string // added to the agent's environment
	framing framing
	// stderrFatal means the tool prints only errors, so output there means
	// the frame in flight will not come.
	stderrFatal bool
}

// environ is the tool's environment. A display variable inherited from the
// agent would make GStreamer reach the GPU through that X server, which an
// Xvfb display cannot do; without one it opens the render node directly.
// libva's informational messages are turned off, which keeps the stderr
// that error messages quote down to the errors.
func (r *route) environ() []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return k == "DISPLAY" || k == "WAYLAND_DISPLAY" || k == "LIBVA_MESSAGING_LEVEL"
	})
	return append(append(env, "LIBVA_MESSAGING_LEVEL=1"), r.env...)
}

// gop is the keyframe interval the tools are given, in frames: in effect
// never, since Encoder restarts the tool when it needs an IDR and frames
// arrive only when the screen changes.
const gop = 1 << 20

// The tools run VBR with the target as their peak and averagePercent of it
// as their average. Under CBR the hardware spends the whole target whatever
// the picture: at 20 Mbit/s, all of it on a terminal where VBR sends tens of
// kilobits (measured on an Intel iGPU). The frame budget holds the peak
// either way.
const averagePercent = 70

// gstRoute runs an encoder element between rawvideoparse
// (gst-plugins-base), which cuts stdin into frames, and multipartmux
// (gst-plugins-good), which frames the output. encoder renders the element
// and its properties for a peak bitrate in kbit/s.
func gstRoute(gst, name string, encoder func(kbps int) string, env ...string) route {
	return route{
		name: name, path: gst, env: env, framing: newMultipartReader,
		args: func(c config) []string {
			pipeline := fmt.Sprintf("fdsrc fd=0 blocksize=%d ! rawvideoparse width=%d height=%d format=nv12 framerate=%d/1 ! "+
				"%s ! video/x-h264,profile=constrained-baseline,stream-format=byte-stream,alignment=au ! "+
				"multipartmux boundary=remotejump ! fdsink fd=1 sync=false",
				c.width*c.height*3/2, c.width, c.height, c.fps, encoder(c.bitrate/1000))
			return append([]string{"-q"}, strings.Fields(pipeline)...)
		},
	}
}

// ffmpegRoute runs ffmpeg with input options before the raw NV12 input and
// encoder options after it.
func ffmpegRoute(ffmpeg, name string, input, encoder []string) route {
	return route{
		name: name, path: ffmpeg, framing: newFLVReader, stderrFatal: true,
		args: func(c config) []string {
			args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
			args = append(args, input...)
			args = append(args,
				"-f", "rawvideo", "-pix_fmt", "nv12", "-s", fmt.Sprintf("%dx%d", c.width, c.height),
				"-r", strconv.Itoa(c.fps), "-i", "pipe:0")
			args = append(args, encoder...)
			peak := strconv.Itoa(c.bitrate)
			return append(args,
				"-b:v", strconv.Itoa(c.bitrate*averagePercent/100), "-maxrate", peak, "-bufsize", peak,
				"-g", strconv.Itoa(gop), "-bf", "0",
				"-flush_packets", "1", "-flvflags", "no_duration_filesize", "-f", "flv", "pipe:1")
		},
	}
}

func vaapiRoutes() ([]route, error) {
	nodes := renderNodes()
	if len(nodes) == 0 {
		return nil, errors.New("no /dev/dri render node this user can open for reading and writing")
	}
	ffmpeg, _ := exec.LookPath("ffmpeg")
	gst, _ := exec.LookPath("gst-launch-1.0")
	if ffmpeg == "" && gst == "" {
		return nil, errors.New("neither ffmpeg nor gst-launch-1.0 is installed")
	}
	var rs []route
	for i, node := range nodes {
		if ffmpeg != "" {
			for _, lp := range []string{"0", "1"} {
				name := "ffmpeg h264_vaapi on " + node
				if lp == "1" {
					name += " (low power)"
				}
				// async_depth 1 keeps ffmpeg 5 and later from holding a
				// frame back; older versions ignore it.
				rs = append(rs, ffmpegRoute(ffmpeg, name, []string{"-vaapi_device", node},
					[]string{"-vf", "hwupload", "-c:v", "h264_vaapi", "-profile:v", "constrained_baseline",
						"-rc_mode", "VBR", "-low_power", lp, "-async_depth", "1", "-sei", "0"}))
			}
		}
		if gst == "" {
			continue
		}
		// The va plugin (GStreamer 1.22 and later) names the first device's
		// elements plainly and prefixes the others with the device. Its
		// bitrate is the VBR peak, as vaapih264enc's is.
		va := "va"
		if i > 0 {
			va += filepath.Base(node)
		}
		for _, el := range []string{va + "h264enc", va + "h264lpenc"} {
			rs = append(rs, gstRoute(gst, "gst-launch-1.0 "+el, func(kbps int) string {
				return fmt.Sprintf("%s rate-control=vbr bitrate=%d target-percentage=%d key-int-max=1024 b-frames=0 ref-frames=1 "+
					"cabac=false dct8x8=false aud=true", el, kbps, averagePercent)
			}))
		}
		for _, tune := range []string{"none", "low-power"} {
			rs = append(rs, gstRoute(gst, "gst-launch-1.0 vaapih264enc tune="+tune+" on "+node, func(kbps int) string {
				return fmt.Sprintf("vaapih264enc tune=%s rate-control=vbr bitrate=%d target-percentage=%d keyframe-period=%d "+
					"max-bframes=0 refs=1 aud=true", tune, kbps, averagePercent, gop)
			}, "GST_VAAPI_DRM_DEVICE="+node))
		}
	}
	return rs, nil
}

func nvencRoutes() ([]route, error) {
	ffmpeg, _ := exec.LookPath("ffmpeg")
	gst, _ := exec.LookPath("gst-launch-1.0")
	if ffmpeg == "" && gst == "" {
		return nil, errors.New("neither ffmpeg nor gst-launch-1.0 is installed")
	}
	var rs []route
	if ffmpeg != "" {
		rs = append(rs, ffmpegRoute(ffmpeg, "ffmpeg h264_nvenc", nil,
			[]string{"-c:v", "h264_nvenc", "-profile:v", "baseline", "-preset", "llhp",
				"-zerolatency", "1", "-delay", "0", "-rc", "vbr"}))
	}
	if gst != "" {
		// nvh264enc's bitrate is the VBR average; max-bitrate is the peak.
		rs = append(rs, gstRoute(gst, "gst-launch-1.0 nvh264enc", func(kbps int) string {
			return fmt.Sprintf("nvh264enc rc-mode=vbr bitrate=%d max-bitrate=%d gop-size=-1 bframes=0 zerolatency=true "+
				"preset=low-latency-hp aud=true", kbps*averagePercent/100, kbps)
		}))
	}
	return rs, nil
}

// renderNodes lists the DRM render nodes this user can open read-write.
func renderNodes() []string {
	nodes, _ := filepath.Glob("/dev/dri/renderD*")
	slices.Sort(nodes)
	return slices.DeleteFunc(nodes, func(n string) bool {
		f, err := os.OpenFile(n, os.O_RDWR, 0)
		if err != nil {
			return true
		}
		f.Close()
		return false
	})
}

// trialTimeout covers GStreamer building its plugin registry, which the
// first run on a host does and which can take seconds.
const trialTimeout = 20 * time.Second

// trial encodes one small frame through r and checks the result is a
// Baseline IDR, delivered before a second frame is sent: a tool that holds
// frames back would pair every picture with the previous one.
func trial(r *route, timeout time.Duration) error {
	c := config{width: 320, height: 240, fps: 30, bitrate: 1_000_000}
	p, err := startProc(r, c)
	if err != nil {
		return err
	}
	defer p.kill()
	y := make([]byte, c.width*c.height)
	uv := make([]byte, c.width*c.height/2)
	for i := range y {
		y[i] = 128
	}
	for i := range uv {
		uv[i] = 128
	}
	au, err := p.encode(y, uv, time.Now().Add(timeout))
	if err != nil {
		return err
	}
	idc, err := profile(au)
	if err != nil {
		return fmt.Errorf("first access unit: %w", err)
	}
	// The agent negotiates Constrained Baseline. Baseline without the
	// constraint flag decodes the same, since no hardware encoder uses the
	// tools (FMO, ASO) that set the two apart.
	if idc != profileBaseline {
		return fmt.Errorf("emits H.264 profile_idc %d, not Baseline", idc)
	}
	if !hasIDR(au) {
		return errors.New("first access unit is not an IDR")
	}
	return nil
}
