//go:build linux && cgo && openh264

package h264

import (
	"bytes"
	"math/rand/v2"
	"os"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agent/yuv"
)

const (
	tw, th = 1920, 1080
	tfps   = 30
)

// desktop draws a light background with rows of glyph-like blocks shifted up
// by scroll pixels: close enough to a terminal or editor for rate control.
func desktop(img *yuv.I420, scroll int) {
	for row := 0; row < th; row++ {
		line := row + scroll
		for col := 0; col < tw; col++ {
			c := byte(235)
			if line%24 < 14 && col%9 < 6 && ((line/24)*131+(col/9)*71)%5 != 0 {
				c = byte(20 + (line*7+col*13)%40)
			}
			img.Y[row*tw+col] = c
		}
	}
	for i := range img.U {
		img.U[i], img.V[i] = 128, 128
	}
}

// noisyGradient fills a rectangle with a moving gradient under fresh noise,
// which is what video playback looks like to an encoder.
func noisyGradient(img *yuv.I420, x0, y0, w, h, frame int) {
	r := rand.New(rand.NewPCG(uint64(frame), 7))
	for row := y0; row < y0+h; row++ {
		for col := x0; col < x0+w; col++ {
			g := (col+frame*6)/4 + (row+frame*3)/6
			img.Y[row*tw+col] = byte(g + int(r.Uint32()%17) - 8)
		}
	}
	for row := y0 / 2; row < (y0+h)/2; row++ {
		for col := x0 / 2; col < (x0+w)/2; col++ {
			img.U[row*tw/2+col] = byte(100 + (col+frame)%60)
			img.V[row*tw/2+col] = byte(140 - (row+frame)%50)
		}
	}
}

type pattern struct {
	name  string
	frame func(img *yuv.I420, i int)
}

var patterns = []pattern{
	{"idle", func(img *yuv.I420, i int) {
		if i == 0 {
			desktop(img, 0)
		}
	}},
	{"typing", func(img *yuv.I420, i int) {
		if i == 0 {
			desktop(img, 0)
		}
		// One glyph cell changes every third frame, like keystrokes.
		if i%3 == 0 {
			x, y := 40+(i/3)*9%1800, 600
			for row := y; row < y+14; row++ {
				for col := x; col < x+6; col++ {
					img.Y[row*tw+col] = byte(30 + i%50)
				}
			}
		}
	}},
	{"scroll", func(img *yuv.I420, i int) { desktop(img, i*6) }},
	{"video-window", func(img *yuv.I420, i int) {
		if i == 0 {
			desktop(img, 0)
		}
		noisyGradient(img, 400, 300, 854, 480, i)
	}},
	{"video-fullscreen", func(img *yuv.I420, i int) { noisyGradient(img, 0, 0, tw, th, i) }},
}

type result struct {
	kbps        float64 // over frames after the first second
	idrs        int     // after frame 0
	skipped     int
	fps         float64
	maxFrameKiB int
}

func run(t *testing.T, p Params, pat pattern, frames int, forceAt int) result {
	t.Helper()
	enc, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	img := yuv.NewI420(tw, th)
	var r result
	var bits int
	var spent time.Duration
	for i := 0; i < frames; i++ {
		pat.frame(img, i)
		start := time.Now()
		au, err := enc.Encode(img, time.Duration(i)*time.Second/tfps, i == 0 || i == forceAt)
		spent += time.Since(start)
		if err != nil {
			t.Fatalf("%s frame %d: %v", pat.name, i, err)
		}
		switch {
		case len(au) == 0:
			r.skipped++
		case i > 0 && enc.LastFrameType() == FrameIDR:
			r.idrs++
		}
		if i >= tfps {
			bits += 8 * len(au)
		}
		r.maxFrameKiB = max(r.maxFrameKiB, len(au)>>10)
	}
	r.kbps = float64(bits) / float64(frames-tfps) * tfps / 1000
	r.fps = float64(frames) / spent.Seconds()
	return r
}

func TestStreamShape(t *testing.T) {
	enc, err := New(tw, th, tfps, 2_000_000)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	img := yuv.NewI420(tw, th)
	desktop(img, 0)
	first, err := enc.Encode(img, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	// SPS, PPS, IDR slice, each with a start code; profile_idc 66 with
	// constraint_set1 is Constrained Baseline.
	for _, want := range [][]byte{{0, 0, 0, 1, 0x67, 66, 0xc0}, {0, 0, 0, 1, 0x68}, {0, 0, 0, 1, 0x65}} {
		if !bytes.Contains(first, want[:5]) {
			t.Fatalf("first access unit lacks NAL % x", want[:5])
		}
	}
	if i := bytes.Index(first, []byte{0, 0, 0, 1, 0x67}); first[i+5] != 66 || first[i+6]&0x40 == 0 {
		t.Fatalf("SPS is not Constrained Baseline: % x", first[i:i+8])
	}
	if enc.LastFrameType() != FrameIDR {
		t.Fatalf("first frame type %d", enc.LastFrameType())
	}
	desktop(img, 6)
	if _, err := enc.Encode(img, time.Second/tfps, false); err != nil {
		t.Fatal(err)
	}
	// Right after a keyframe rate control may skip; it must not repeat the IDR.
	if ft := enc.LastFrameType(); ft != FrameP && ft != FrameSkip {
		t.Fatalf("second frame type %d, want P or skip", ft)
	}
	if _, err := enc.Encode(img, 2*time.Second/tfps, true); err != nil {
		t.Fatal(err)
	}
	if enc.LastFrameType() != FrameIDR {
		t.Fatalf("forced keyframe came out as type %d", enc.LastFrameType())
	}
	if _, err := enc.Encode(yuv.NewI420(1280, 720), 0, false); err == nil {
		t.Fatal("accepted a frame of the wrong size")
	}
}

// TestRateControl checks the property congestion control depends on: the
// encoder holds roughly the target bitrate for every kind of content, never
// inserts IDR frames of its own, and honours forced keyframes even when rate
// control is skipping frames.
func TestRateControl(t *testing.T) {
	const target = 2_000_000
	for _, pat := range patterns {
		t.Run(pat.name, func(t *testing.T) {
			r := run(t, Defaults(tw, th, tfps, target), pat, 150, 100)
			t.Logf("%-16s %7.0f kbps  idr=%d skipped=%d  max frame %d KiB  %.0f fps", pat.name, r.kbps, r.idrs, r.skipped, r.maxFrameKiB, r.fps)
			if r.kbps > 1.6*target/1000 {
				t.Errorf("%.0f kbps for a %d kbps target", r.kbps, target/1000)
			}
			if r.idrs != 1 {
				t.Errorf("%d IDR frames after the first, want exactly the forced one", r.idrs)
			}
		})
	}
}

// TestQPCeilingTradesFramesForLegibility checks the trade the defaults
// make on a link too slow for the content: frames are skipped rather than
// encoded past the QP ceiling.
func TestQPCeilingTradesFramesForLegibility(t *testing.T) {
	p := Defaults(tw, th, tfps, 800_000)
	enc, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	img := yuv.NewI420(tw, th)
	sent, skipped, worst := 0, 0, 0
	for i := range 150 {
		desktop(img, i*6)
		au, err := enc.Encode(img, time.Duration(i)*time.Second/tfps, i == 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(au) == 0 {
			skipped++
			continue
		}
		sent++
		worst = max(worst, enc.QP())
	}
	t.Logf("scrolling at 800 kbit/s: %d frames sent, %d skipped, worst QP %d", sent, skipped, worst)
	if worst > p.MaxQP {
		t.Errorf("a frame went out at QP %d, over the ceiling of %d", worst, p.MaxQP)
	}
	if skipped == 0 {
		t.Error("nothing skipped: the ceiling was never reached")
	}
	enc.Close()
	if enc.QP() != -1 {
		t.Error("a closed encoder reports a QP")
	}
}

func TestSetBitrateTakesEffect(t *testing.T) {
	enc, err := New(tw, th, tfps, 500_000)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	img := yuv.NewI420(tw, th)
	desktop(img, 0)
	measure := func(from, to int) float64 {
		bits := 0
		for i := from; i < to; i++ {
			noisyGradient(img, 400, 300, 854, 480, i)
			au, err := enc.Encode(img, time.Duration(i)*time.Second/tfps, i == 0)
			if err != nil {
				t.Fatal(err)
			}
			if i >= from+tfps/2 {
				bits += 8 * len(au)
			}
		}
		return float64(bits) / float64(to-from-tfps/2) * tfps / 1000
	}
	low := measure(0, 90)
	if err := enc.SetBitrate(4_000_000); err != nil {
		t.Fatal(err)
	}
	high := measure(90, 180)
	if err := enc.SetBitrate(500_000); err != nil {
		t.Fatal(err)
	}
	back := measure(180, 270)
	t.Logf("target 500 -> 4000 -> 500 kbps: measured %.0f -> %.0f -> %.0f kbps", low, high, back)
	if high < 2.5*low {
		t.Fatalf("raising the target 8x moved the rate only from %.0f to %.0f kbps", low, high)
	}
	if back > high/2.5 {
		t.Fatalf("lowering the target 8x moved the rate only from %.0f to %.0f kbps", high, back)
	}
}

// TestCompareModes prints the screen and camera content modes side by side.
// It is a tuning aid: set ROBIN_H264_COMPARE=1 to run it.
func TestCompareModes(t *testing.T) {
	if os.Getenv("ROBIN_H264_COMPARE") == "" {
		t.Skip("set ROBIN_H264_COMPARE=1")
	}
	for _, target := range []int{1_000_000, 4_000_000} {
		for _, mode := range []struct {
			name string
			edit func(*Params)
		}{
			{"camera", func(*Params) {}},
			{"skipoff-qp16-51", func(p *Params) { p.FrameSkip, p.MaxQP = false, 51 }},
			{"screen", func(p *Params) { p.Screen, p.SceneChange = true, true }},
		} {
			for _, pat := range patterns {
				p := Defaults(tw, th, tfps, target)
				mode.edit(&p)
				r := run(t, p, pat, 150, 100)
				t.Logf("%4d kbps %-16s %-16s %7.0f kbps  idr=%3d skip=%3d  max %4d KiB  %4.0f fps",
					target/1000, mode.name, pat.name, r.kbps, r.idrs, r.skipped, r.maxFrameKiB, r.fps)
			}
		}
	}
}
