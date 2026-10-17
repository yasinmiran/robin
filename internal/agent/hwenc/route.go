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

// gstRoute runs encoder, an element and its properties, between
// rawvideoparse (gst-plugins-base), which cuts stdin into frames, and
// multipartmux (gst-plugins-good), which frames the output. Every encoder
// element used here takes its bitrate in kbit/s.
func gstRoute(gst, name, encoder string, env ...string) route {
	return route{
		name: name, path: gst, env: env, framing: newMultipartReader,
		args: func(c config) []string {
			pipeline := fmt.Sprintf("fdsrc fd=0 blocksize=%d ! rawvideoparse width=%d height=%d format=nv12 framerate=%d/1 ! "+
				"%s bitrate=%d ! video/x-h264,profile=constrained-baseline,stream-format=byte-stream,alignment=au ! "+
				"multipartmux boundary=remotejump ! fdsink fd=1 sync=false",
				c.width*c.height*3/2, c.width, c.height, c.fps, encoder, c.bitrate/1000)
			return append([]string{"-q"}, strings.Fields(pipeline)...)
		},
	}
}

// ffmpegRoute runs ffmpeg with input options before and encoder options
// after the raw NV12 input.
func ffmpegRoute(ffmpeg, name string, input, encoder func(config) []string) route {
	return route{
		name: name, path: ffmpeg, framing: newFLVReader, stderrFatal: true,
		args: func(c config) []string {
			args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
			args = append(args, input(c)...)
			args = append(args,
				"-f", "rawvideo", "-pix_fmt", "nv12", "-s", fmt.Sprintf("%dx%d", c.width, c.height),
				"-r", strconv.Itoa(c.fps), "-i", "pipe:0")
			args = append(args, encoder(c)...)
			b := strconv.Itoa(c.bitrate)
			return append(args,
				"-b:v", b, "-maxrate", b, "-bufsize", b, "-g", strconv.Itoa(gop), "-bf", "0",
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
				rs = append(rs, ffmpegRoute(ffmpeg, name,
					func(config) []string { return []string{"-vaapi_device", node} },
					func(config) []string {
						// async_depth 1 keeps ffmpeg 5 and later from holding
						// a frame back; older versions ignore it.
						return []string{"-vf", "hwupload", "-c:v", "h264_vaapi", "-profile:v", "constrained_baseline",
							"-rc_mode", "CBR", "-low_power", lp, "-async_depth", "1", "-sei", "0"}
					}))
			}
		}
		if gst == "" {
			continue
		}
		// The va plugin (GStreamer 1.22 and later) names the first device's
		// elements plainly and prefixes the others with the device.
		va := "va"
		if i > 0 {
			va += filepath.Base(node)
		}
		for _, el := range []string{va + "h264enc", va + "h264lpenc"} {
			rs = append(rs, gstRoute(gst, "gst-launch-1.0 "+el,
				el+" rate-control=cbr key-int-max=1024 b-frames=0 ref-frames=1 cabac=false dct8x8=false aud=true"))
		}
		for _, tune := range []string{"none", "low-power"} {
			rs = append(rs, gstRoute(gst, "gst-launch-1.0 vaapih264enc tune="+tune+" on "+node,
				fmt.Sprintf("vaapih264enc tune=%s rate-control=cbr keyframe-period=%d max-bframes=0 refs=1 aud=true", tune, gop),
				"GST_VAAPI_DRM_DEVICE="+node))
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
		rs = append(rs, ffmpegRoute(ffmpeg, "ffmpeg h264_nvenc",
			func(config) []string { return nil },
			func(config) []string {
				return []string{"-c:v", "h264_nvenc", "-profile:v", "baseline", "-preset", "llhp",
					"-zerolatency", "1", "-delay", "0", "-rc", "cbr"}
			}))
	}
	if gst != "" {
		rs = append(rs, gstRoute(gst, "gst-launch-1.0 nvh264enc",
			"nvh264enc rc-mode=cbr gop-size=-1 bframes=0 zerolatency=true preset=low-latency-hp aud=true"))
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
