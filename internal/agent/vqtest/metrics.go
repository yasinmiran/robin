package vqtest

import (
	"math"
)

// Score is how close a shown picture is to the source, on luma.
type Score struct {
	// PSNR in dB, capped at MaxPSNR for identical pictures.
	PSNR float64
	// SSIM is the mean structural similarity of 8x8 blocks, 1 when equal.
	SSIM float64
	// WorstTile is the SSIM of the worst 64x64 tile. Blocky text in one
	// window barely moves the mean of a large, mostly flat screen; it
	// shows here.
	WorstTile float64
}

// MaxPSNR stands for identical pictures, whose PSNR is infinite.
const MaxPSNR = 100

// ssimBlock and ssimTile are the sides of the blocks SSIM is computed over
// and of the tiles WorstTile averages them in.
const (
	ssimBlock = 8
	ssimTile  = 64
)

// Compare scores the luma plane got (gotW x gotH) against want (w x h).
// A picture of another size is first scaled to the source's with bilinear
// filtering, as a browser scales video to its element.
func Compare(want []byte, w, h int, got []byte, gotW, gotH int) Score {
	if gotW != w || gotH != h {
		got = Resize(got, gotW, gotH, w, h)
	}
	var sse float64
	for i := range w * h {
		d := float64(int(want[i]) - int(got[i]))
		sse += d * d
	}
	s := Score{PSNR: MaxPSNR}
	if sse > 0 {
		s.PSNR = min(10*math.Log10(255*255*float64(w*h)/sse), MaxPSNR)
	}
	s.SSIM, s.WorstTile = ssim(want, got, w, h)
	return s
}

// ssim returns the mean SSIM over whole 8x8 blocks and the lowest mean of
// any 64x64 tile's blocks.
func ssim(a, b []byte, w, h int) (mean, worst float64) {
	const (
		c1 = (0.01 * 255) * (0.01 * 255)
		c2 = (0.03 * 255) * (0.03 * 255)
		n  = ssimBlock * ssimBlock
	)
	bw, bh := w/ssimBlock, h/ssimBlock
	if bw == 0 || bh == 0 {
		return 1, 1
	}
	per := ssimTile / ssimBlock
	tw := (bw + per - 1) / per
	tiles := make([]float64, tw*((bh+per-1)/per))
	counts := make([]int, len(tiles))
	var total float64
	for by := range bh {
		for bx := range bw {
			var sa, sb, saa, sbb, sab int
			for y := by * ssimBlock; y < (by+1)*ssimBlock; y++ {
				row := y * w
				for x := bx * ssimBlock; x < (bx+1)*ssimBlock; x++ {
					pa, pb := int(a[row+x]), int(b[row+x])
					sa += pa
					sb += pb
					saa += pa * pa
					sbb += pb * pb
					sab += pa * pb
				}
			}
			ma, mb := float64(sa)/n, float64(sb)/n
			va := float64(saa)/n - ma*ma
			vb := float64(sbb)/n - mb*mb
			cov := float64(sab)/n - ma*mb
			v := (2*ma*mb + c1) * (2*cov + c2) / ((ma*ma + mb*mb + c1) * (va + vb + c2))
			total += v
			t := (by/per)*tw + bx/per
			tiles[t] += v
			counts[t]++
		}
	}
	worst = 1
	for i, t := range tiles {
		if counts[i] > 0 {
			worst = min(worst, t/float64(counts[i]))
		}
	}
	return total / float64(bw*bh), worst
}

// Resize scales a w x h plane to dw x dh with bilinear filtering, sampling
// at pixel centres.
func Resize(src []byte, w, h, dw, dh int) []byte {
	const one = 1 << 16
	dst := make([]byte, dw*dh)
	xs := make([]int, dw)
	xf := make([]int, dw)
	for x := range dw {
		xs[x], xf[x] = position(x, w, dw)
	}
	for y := range dh {
		y0, fy := position(y, h, dh)
		y1 := min(y0+1, h-1)
		r0, r1 := src[y0*w:y0*w+w], src[y1*w:y1*w+w]
		out := dst[y*dw : y*dw+dw]
		for x := range dw {
			x0, fx := xs[x], xf[x]
			x1 := min(x0+1, w-1)
			top := int(r0[x0])*(one-fx) + int(r0[x1])*fx
			bottom := int(r1[x0])*(one-fx) + int(r1[x1])*fx
			out[x] = byte((int64(top)*int64(one-fy) + int64(bottom)*int64(fy) + 1<<31) >> 32)
		}
	}
	return dst
}

// position maps destination index i of n to the source index at or left
// of its centre among m, and the fraction towards the next, in 1/65536.
func position(i, m, n int) (int, int) {
	p := ((2*i+1)*m*(1<<16)/(2*n) - 1<<15)
	if p < 0 {
		return 0, 0
	}
	return min(p>>16, m-1), p & (1<<16 - 1)
}
