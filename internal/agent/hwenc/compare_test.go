//go:build linux

package hwenc

import (
	"os"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agent/h264"
	"github.com/yasinmiran/robin/internal/agent/yuv"
)

type encoder interface {
	Encode(img *yuv.I420, ts time.Duration, keyframe bool) ([]byte, error)
	SetBitrate(bps int) error
	Close() error
}

type contender struct {
	name string
	open func(w, h, fps, bps int) encoder
}

// TestCompare prints openh264 and every working VA-API route side by side
// at 30 fps: time per Encode call and CPU per encoded frame, in the agent and
// in the tool, at 1080p, 1440p and 2160p; the bitrate each sends against a
// 2 Mbit/s target; how a 500, 4000, 500 kbit/s target sequence is followed;
// and how long a requested keyframe takes. It is a measuring aid: set
// ROBIN_HWENC_COMPARE=1 and build with the openh264 tag, as the Makefile builds
// the agent, to run it.
func TestCompare(t *testing.T) {
	if os.Getenv("ROBIN_HWENC_COMPARE") == "" {
		t.Skip("set ROBIN_HWENC_COMPARE=1")
	}
	probe, err := h264.New(64, 64, 30, 1_000_000)
	if err != nil {
		t.Skip(err)
	}
	probe.Close()
	const w, h, fps = 1920, 1080, 30
	contenders := []contender{{"openh264", func(w, h, fps, bps int) encoder {
		e, err := h264.New(w, h, fps, bps)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}}}
	for _, r := range workingRoutes(t, VAAPI) {
		contenders = append(contenders, contender{r.name, func(w, h, fps, bps int) encoder { return newEncoder(&r, w, h, fps, bps) }})
	}

	// Cost at a target high enough that nothing is skipped, then the
	// bitrate each holds against a tight one.
	for _, run := range []struct {
		w, h, bps int
	}{{w, h, 20_000_000}, {2560, 1440, 30_000_000}, {3840, 2160, 40_000_000}, {w, h, 2_000_000}} {
		for _, pat := range patterns {
			frames := render(pat, run.w, run.h, 30)
			for _, c := range contenders {
				r := measure(t, c.open(run.w, run.h, fps, run.bps), frames, 150, fps, nil)
				t.Logf("%dp %2d Mbit/s %-16s %-44s %5.2f ms/call (p95 %5.2f)  cpu/frame %5.2f ms agent + %5.2f ms tool  %5.0f kbit/s  skipped %3d  idr %d",
					run.h, run.bps/1_000_000, pat.name, c.name, r.meanMs, r.p95Ms, r.selfMs, r.toolMs, r.kbps, r.skipped, r.idrs)
			}
		}
	}

	frames := render(patterns[2], w, h, 30)
	for _, c := range contenders {
		var phases []float64
		e := c.open(w, h, fps, 500_000)
		measure(t, e, frames, 270, fps, func(i int, bits int) {
			switch i {
			case 90:
				e.SetBitrate(4_000_000)
			case 180:
				e.SetBitrate(500_000)
			}
			if i%90 == 89 {
				phases = append(phases, float64(bits))
			}
		})
		t.Logf("%-48s target 500 -> 4000 -> 500 kbit/s: sent %.0f -> %.0f -> %.0f kbit/s (each 3 s phase whole)",
			c.name, phases[0]/3000, (phases[1]-phases[0])/3000, (phases[2]-phases[1])/3000)
	}

	for _, c := range contenders {
		e := c.open(w, h, fps, 2_000_000)
		var waits []time.Duration
		for i := range 60 {
			ts := time.Duration(i) * time.Second / fps
			key := i > 0 && i%15 == 0
			start := time.Now()
			if _, err := e.Encode(frames[i%len(frames)], ts, i == 0 || key); err != nil {
				t.Fatal(err)
			}
			if key {
				waits = append(waits, time.Since(start))
			}
		}
		e.Close()
		slices.Sort(waits)
		t.Logf("%-48s requested keyframe: median %v, worst %v", c.name, waits[len(waits)/2].Round(100*time.Microsecond), waits[len(waits)-1].Round(100*time.Microsecond))
	}
}

// render draws n consecutive frames of pat, so the measurement loop times
// encoding alone.
func render(pat pattern, w, h, n int) []*yuv.I420 {
	img := yuv.NewI420(w, h)
	var out []*yuv.I420
	for i := range n {
		pat.draw(img, i)
		f := yuv.NewI420(w, h)
		copy(f.Y, img.Y)
		copy(f.U, img.U)
		copy(f.V, img.V)
		out = append(out, f)
	}
	return out
}

type result struct {
	meanMs, p95Ms, kbps float64
	// selfMs and toolMs are CPU per encoded frame, in this process and in
	// the encoder tool.
	selfMs, toolMs float64
	skipped, idrs  int
}

// measure encodes n frames, cycling through frames, at fps of stream time,
// and closes e. each, when set, runs after every frame with the bits sent
// so far.
func measure(t *testing.T, e encoder, frames []*yuv.I420, n, fps int, each func(i, bits int)) result {
	t.Helper()
	var r result
	var calls []time.Duration
	bits, total := 0, 0
	self0, tool0 := cpuTime()
	for i := range n {
		ts := time.Duration(i) * time.Second / time.Duration(fps)
		start := time.Now()
		au, err := e.Encode(frames[i%len(frames)], ts, i == 0)
		d := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		if len(au) == 0 {
			r.skipped++
		} else {
			calls = append(calls, d)
			if i > 0 && hasIDR(au) {
				r.idrs++
			}
		}
		if i >= fps {
			bits += 8 * len(au)
		}
		total += 8 * len(au)
		if each != nil {
			each(i, total)
		}
	}
	e.Close()
	self, tool := cpuTime()
	perFrame := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) / float64(n-r.skipped) }
	r.selfMs, r.toolMs = perFrame(self-self0), perFrame(tool-tool0)
	r.kbps = float64(bits) / float64(n-fps) * float64(fps) / 1000
	var sum time.Duration
	for _, d := range calls {
		sum += d
	}
	slices.Sort(calls)
	r.meanMs = float64(sum) / float64(len(calls)) / float64(time.Millisecond)
	r.p95Ms = float64(calls[len(calls)*95/100]) / float64(time.Millisecond)
	return r
}

// cpuTime is the CPU this process and its reaped children have used.
func cpuTime() (self, children time.Duration) {
	var s, c syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &s)
	syscall.Getrusage(syscall.RUSAGE_CHILDREN, &c)
	tv := func(v syscall.Timeval) time.Duration { return time.Duration(v.Nano()) }
	return tv(s.Utime) + tv(s.Stime), tv(c.Utime) + tv(c.Stime)
}
