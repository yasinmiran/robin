//go:build linux

package hwenc

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yasinmiran/remotejump/internal/agent/yuv"
)

// requireEnv names the kinds that must work, comma-separated, for a host
// that is known to have them; otherwise the hardware tests skip when a kind
// is missing.
const requireEnv = "RJ_HWENC_REQUIRE"

// workingRoutes returns the routes to kind that pass the trial on this host.
func workingRoutes(t *testing.T, kind Kind) []route {
	t.Helper()
	unavailable := func(format string, args ...any) {
		t.Helper()
		if slices.Contains(strings.Split(os.Getenv(requireEnv), ","), string(kind)) {
			t.Fatalf(format, args...)
		}
		t.Skipf(format, args...)
	}
	routes, err := NewOpener(kind, nil).routes()
	if err != nil {
		unavailable("no %s route: %v", kind, err)
	}
	var working []route
	for _, r := range routes {
		if err := trial(&r, trialTimeout); err != nil {
			t.Logf("%s: %s", r.name, oneLine(err.Error()))
			continue
		}
		working = append(working, r)
	}
	if len(working) == 0 {
		unavailable("no working %s route on this host", kind)
	}
	return working
}

// TestHardware puts every working route through what the stream relies on:
// a Constrained Baseline IDR first, P slices after, an IDR on request, the
// target bitrate held on content the encoder cannot compress to it, a
// restart when the target moves far, a stream a decoder accepts, and no
// process left after Close.
func TestHardware(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	for _, kind := range []Kind{VAAPI, NVENC} {
		t.Run(string(kind), func(t *testing.T) {
			for _, r := range workingRoutes(t, kind) {
				t.Run(strings.ReplaceAll(r.name, "/", "_"), func(t *testing.T) { checkRoute(t, &r) })
			}
		})
	}
}

func checkRoute(t *testing.T, r *route) {
	const w, h, fps = 1280, 720, 30
	const frame = time.Second / fps
	e := newEncoder(r, w, h, fps, 2_000_000)
	defer e.Close()
	img := yuv.NewI420(w, h)
	var stream bytes.Buffer
	i := 0
	encode := func(keyframe bool) []byte {
		t.Helper()
		au, err := e.Encode(img, time.Duration(i)*frame, keyframe)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		stream.Write(au)
		i++
		return au
	}

	drawDesktop(img, 0)
	start := time.Now()
	first := encode(true)
	t.Logf("first access unit: %d bytes after %v, including the tool's start", len(first), time.Since(start).Round(time.Millisecond))
	if idc, err := profile(first); err != nil || idc != profileBaseline || !hasIDR(first) {
		t.Fatalf("first access unit: profile %d (%v), IDR %v", idc, err, hasIDR(first))
	}

	// Scrolling: every frame P, none skipped at a generous target.
	e.SetBitrate(8_000_000)
	for range 30 {
		drawDesktop(img, i*6)
		au := encode(false)
		if len(au) == 0 {
			t.Fatalf("frame %d skipped at 8 Mbit/s", i-1)
		}
		for _, n := range nalUnits(au) {
			if st := sliceType(n); st >= 0 && (nalType(n) == nalIDR || st != 0) {
				t.Fatalf("frame %d: NAL type %d slice type %d, want P", i-1, nalType(n), st)
			}
		}
	}

	// A keyframe on request, which takes a restart.
	start = time.Now()
	key := encode(true)
	t.Logf("requested keyframe: %d bytes after %v", len(key), time.Since(start).Round(time.Millisecond))
	if _, err := profile(key); err != nil || !hasIDR(key) {
		t.Fatalf("requested keyframe is not an IDR with its SPS (%v)", err)
	}

	// Full-screen noise, which no target this low holds: the budget must.
	const target = 1_000_000
	e.SetBitrate(target)
	bits, from := 0, i
	for range 4 * fps {
		drawNoise(img, 0, 0, w, h, i)
		au := encode(false)
		if i-from > fps { // after the first second and the retarget restart
			bits += 8 * len(au)
		}
	}
	got := float64(bits) / 3
	t.Logf("noise at %d kbit/s: %.0f kbit/s sent", target/1000, got/1000)
	if got > 1.3*target {
		t.Errorf("sent %.0f kbit/s against a %d kbit/s target", got/1000, target/1000)
	}
	if e.proc.bitrate != target {
		t.Errorf("tool still runs at %d bit/s after the target fell to %d", e.proc.bitrate, target)
	}

	pid := e.proc.cmd.Process.Pid
	e.Close()
	if err := waitGone(pid, time.Second); err != nil {
		t.Error(err)
	}
	checkDecodes(t, stream.Bytes())
}

// checkDecodes runs the stream through ffmpeg's decoder when the host has
// ffmpeg.
func checkDecodes(t *testing.T, stream []byte) {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Log("no ffmpeg to decode with")
		return
	}
	in := filepath.Join(t.TempDir(), "stream.h264")
	if err := os.WriteFile(in, stream, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(ffmpeg, "-v", "error", "-f", "h264", "-i", in, "-f", "framecrc", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg decode: %v\n%s", err, out)
	}
	frames := 0
	for _, line := range strings.Split(string(out), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			frames++
		}
	}
	want := 0
	for _, n := range nalUnits(stream) {
		if t := nalType(n); t == nalSlice || t == nalIDR {
			want++
		}
	}
	if frames != want {
		t.Fatalf("ffmpeg decoded %d frames of %d", frames, want)
	}
	t.Logf("ffmpeg decoded all %d frames", frames)
}
