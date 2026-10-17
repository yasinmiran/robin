package yuv

import (
	"math"
	"math/rand/v2"
	"testing"
)

// areaAverage is the reference box filter: each output pixel is the mean of
// the source area it covers, in float64.
func areaAverage(src []byte, stride, srcW, srcH, dstW, dstH int) []byte {
	out := make([]byte, 4*dstW*dstH)
	sx, sy := float64(srcW)/float64(dstW), float64(srcH)/float64(dstH)
	for y := range dstH {
		y0, y1 := float64(y)*sy, float64(y+1)*sy
		for x := range dstW {
			x0, x1 := float64(x)*sx, float64(x+1)*sx
			var sum [3]float64
			for j := int(y0); float64(j) < y1 && j < srcH; j++ {
				wy := math.Min(float64(j+1), y1) - math.Max(float64(j), y0)
				for i := int(x0); float64(i) < x1 && i < srcW; i++ {
					wx := math.Min(float64(i+1), x1) - math.Max(float64(i), x0)
					for c := range 3 {
						sum[c] += wx * wy * float64(src[j*stride+4*i+c])
					}
				}
			}
			for c := range 3 {
				out[4*(y*dstW+x)+c] = uint8(math.Round(sum[c] / (sx * sy)))
			}
		}
	}
	return out
}

func noise(w, h, stride int, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, seed))
	src := make([]byte, stride*h)
	for i := range src {
		src[i] = byte(r.Uint32())
	}
	return src
}

func planesNear(t *testing.T, got, want *I420, tol int) {
	t.Helper()
	for _, p := range []struct {
		name      string
		got, want []byte
	}{{"Y", got.Y, want.Y}, {"U", got.U, want.U}, {"V", got.V, want.V}} {
		for i := range p.want {
			if d := int(p.got[i]) - int(p.want[i]); d > tol || d < -tol {
				t.Fatalf("%s[%d] = %d, want %d±%d", p.name, i, p.got[i], p.want[i], tol)
			}
		}
	}
}

func TestScalerMatchesAreaAverage(t *testing.T) {
	for _, tc := range []struct{ sw, sh, dw, dh int }{
		{64, 36, 32, 18},   // exactly 2x
		{64, 36, 48, 26},   // 1.33x, as 2560x1440 to 1920x1080
		{160, 83, 60, 30},  // 2.67x, as 5120x2640 to 1920x990
		{97, 61, 16, 10},   // 6x and odd source sizes
		{50, 40, 50, 40},   // same size
		{51, 41, 50, 40},   // a hair smaller
		{300, 9, 100, 2},   // wide strip
		{9, 300, 2, 100},   // tall strip
		{1001, 3, 1000, 2}, // nearly 1:1 in one axis, not the other
	} {
		stride := 4*tc.sw + 12 // padded, like some captures
		src := noise(tc.sw, tc.sh, stride, uint64(tc.sw*tc.sh))
		got := NewI420(tc.dw, tc.dh)
		NewScaler(tc.sw, tc.sh, tc.dw, tc.dh).Convert(got, src, stride)
		want := NewI420(tc.dw, tc.dh)
		FromBGRX(want, areaAverage(src, stride, tc.sw, tc.sh, tc.dw, tc.dh), 4*tc.dw)
		t.Run("", func(t *testing.T) { planesNear(t, got, want, 1) })
	}
}

func TestScalerSolidColour(t *testing.T) {
	for _, tc := range []struct{ sw, sh, dw, dh int }{{3840, 2160, 1920, 1080}, {5120, 2640, 1920, 990}} {
		src := solid(tc.sw, tc.sh, 4*tc.sw, 200, 30, 90)
		got := NewI420(tc.dw, tc.dh)
		NewScaler(tc.sw, tc.sh, tc.dw, tc.dh).Convert(got, src, 4*tc.sw)
		want := NewI420(tc.dw, tc.dh)
		FromBGRX(want, solid(tc.dw, tc.dh, 4*tc.dw, 200, 30, 90), 4*tc.dw)
		planesNear(t, got, want, 0)
	}
}

func TestScalerCheckerboardAtHalf(t *testing.T) {
	// One-pixel black and white squares average to mid grey; sampling would
	// give all black or all white.
	w, h := 40, 20
	src := make([]byte, 4*w*h)
	for y := range h {
		for x := range w {
			if (x+y)%2 == 0 {
				copy(src[4*(y*w+x):], []byte{255, 255, 255})
			}
		}
	}
	got := NewI420(w/2, h/2)
	NewScaler(w, h, w/2, h/2).Convert(got, src, 4*w)
	grey := luma(128, 128, 128)
	for i, y := range got.Y {
		if y != grey && y != grey-1 {
			t.Fatalf("Y[%d] = %d, want %d", i, y, grey)
		}
	}
}

func TestTapsStayInside(t *testing.T) {
	for src := 1; src <= 70; src++ {
		for dst := 1; dst <= src; dst++ {
			tp := newTaps(src, dst)
			for i := range dst {
				var sum uint32
				for _, w := range tp.w[tp.n*i : tp.n*i+tp.n] {
					sum += w
				}
				if sum != 1<<weightBits || tp.first[i] < 0 || tp.first[i]+tp.n > src {
					t.Fatalf("%d->%d output %d: first %d, n %d, weights sum %d", src, dst, i, tp.first[i], tp.n, sum)
				}
			}
		}
	}
}

func BenchmarkScale4KTo1080p(b *testing.B) { benchmarkScale(b, 3840, 2160, 1920, 1080) }

func BenchmarkScale5KTo1080p(b *testing.B) { benchmarkScale(b, 5120, 2640, 1920, 990) }

func benchmarkScale(b *testing.B, sw, sh, dw, dh int) {
	src := noise(sw, sh, 4*sw, 1)
	dst := NewI420(dw, dh)
	s := NewScaler(sw, sh, dw, dh)
	b.SetBytes(int64(len(src)))
	for b.Loop() {
		s.Convert(dst, src, 4*sw)
	}
}
