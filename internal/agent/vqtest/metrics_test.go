package vqtest

import (
	"math"
	"math/rand/v2"
	"testing"
)

func plane(w, h int, f func(x, y int) byte) []byte {
	p := make([]byte, w*h)
	for y := range h {
		for x := range w {
			p[y*w+x] = f(x, y)
		}
	}
	return p
}

func TestCompareIdentical(t *testing.T) {
	p := plane(128, 64, func(x, y int) byte { return byte(x*3 + y) })
	if s := Compare(p, 128, 64, p, 128, 64); s != (Score{MaxPSNR, 1, 1}) {
		t.Fatalf("identical planes score %+v", s)
	}
}

func TestComparePSNR(t *testing.T) {
	a := plane(64, 64, func(x, y int) byte { return 100 })
	b := plane(64, 64, func(x, y int) byte { return 101 })
	want := 10 * math.Log10(255*255)
	if s := Compare(a, 64, 64, b, 64, 64); math.Abs(s.PSNR-want) > 1e-9 {
		t.Fatalf("PSNR of an off-by-one plane is %f, want %f", s.PSNR, want)
	}
}

// TestWorstTileFindsLocalDamage checks that blocks in one corner of a big
// picture barely move the mean but show in the worst tile.
func TestWorstTileFindsLocalDamage(t *testing.T) {
	const w, h = 1024, 512
	text := func(x, y int) byte {
		if x%7 < 2 && y%11 < 8 {
			return 20
		}
		return 235
	}
	src := plane(w, h, text)
	blocky := plane(w, h, func(x, y int) byte {
		if x < 64 && y < 64 {
			return text(x/8*8, y/8*8)
		}
		return text(x, y)
	})
	s := Compare(src, w, h, blocky, w, h)
	if s.SSIM < 0.99 {
		t.Errorf("one damaged tile of 128 took the mean SSIM to %f", s.SSIM)
	}
	if s.WorstTile > 0.8 {
		t.Errorf("worst tile %f, want the damage to show", s.WorstTile)
	}
}

func TestSSIMFallsWithNoise(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 1))
	src := plane(256, 256, func(x, y int) byte { return byte(x ^ y) })
	var last float64 = 1
	for _, amp := range []int{2, 8, 32} {
		noisy := plane(256, 256, func(x, y int) byte {
			return byte(min(max(int(src[y*256+x])+r.IntN(2*amp+1)-amp, 0), 255))
		})
		s := Compare(src, 256, 256, noisy, 256, 256)
		if s.SSIM >= last {
			t.Errorf("noise of +-%d scores SSIM %f, not below %f", amp, s.SSIM, last)
		}
		last = s.SSIM
	}
}

func TestCompareScalesToTheSource(t *testing.T) {
	src := plane(256, 128, func(x, y int) byte { return byte(x) })
	half := plane(128, 64, func(x, y int) byte { return byte(2*x) + 1 })
	s := Compare(src, 256, 128, half, 128, 64)
	if s.PSNR < 40 {
		t.Fatalf("a gradient at half size scored %+v after scaling up", s)
	}
	flat := plane(96, 48, func(int, int) byte { return 77 })
	if got := Resize(flat, 96, 48, 160, 90); got[0] != 77 || got[len(got)-1] != 77 || got[80*160+33] != 77 {
		t.Fatal("scaling a flat plane changed it")
	}
}

func TestSSIMOfATinyPicture(t *testing.T) {
	p := plane(6, 6, func(int, int) byte { return 1 })
	q := plane(6, 6, func(int, int) byte { return 200 })
	if s := Compare(p, 6, 6, q, 6, 6); s.SSIM != 1 || s.WorstTile != 1 || s.PSNR >= MaxPSNR {
		t.Fatalf("a picture smaller than a block scored %+v", s)
	}
}

func TestPositionStaysInside(t *testing.T) {
	for _, tc := range [][2]int{{1920, 2560}, {2560, 1920}, {7, 3}, {3, 7}} {
		m, n := tc[0], tc[1]
		for i := range n {
			p, f := position(i, m, n)
			if p < 0 || p >= m || f < 0 || f >= 1<<16 {
				t.Fatalf("position(%d, %d, %d) = %d, %d", i, m, n, p, f)
			}
		}
	}
}
