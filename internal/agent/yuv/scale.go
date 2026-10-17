package yuv

import (
	"runtime"
	"sync"
)

// weightBits is the fixed-point precision of a filter weight. Two passes of
// 10-bit weights on 8-bit samples stay inside a uint32.
const weightBits = 10

// Scaler converts BGRX frames of one size to I420 frames of a smaller one in
// a single pass, averaging each output pixel over the source area it covers
// (a box filter, exact at fractional ratios), which keeps text legible where
// sampling would drop strokes.
type Scaler struct {
	srcW, srcH int
	dstW, dstH int
	cols, rows taps
	work       []scratch // one per worker
}

// taps maps each output column or row to the n source columns or rows from
// first on, weighted by w[n*i:n*i+n], which sum to 1<<weightBits.
type taps struct {
	n     int
	first []int
	w     []uint32
}

type scratch struct {
	acc  []uint32  // a source row filtered vertically, 3 channels per pixel
	rows [2][]byte // two output rows of BGRX
}

// NewScaler prepares to shrink srcW x srcH to dstW x dstH. The destination
// must be even and no larger than the source in either dimension.
func NewScaler(srcW, srcH, dstW, dstH int) *Scaler {
	if dstW&1 != 0 || dstH&1 != 0 || dstW <= 0 || dstH <= 0 || dstW > srcW || dstH > srcH {
		panic("yuv: bad scale")
	}
	s := &Scaler{srcW: srcW, srcH: srcH, dstW: dstW, dstH: dstH,
		cols: newTaps(srcW, dstW), rows: newTaps(srcH, dstH)}
	s.work = make([]scratch, workers(dstH/2))
	for i := range s.work {
		s.work[i] = scratch{acc: make([]uint32, 3*srcW), rows: [2][]byte{make([]byte, 4*dstW), make([]byte, 4*dstW)}}
	}
	return s
}

// Fits reports whether s converts srcW x srcH frames to dstW x dstH.
func (s *Scaler) Fits(srcW, srcH, dstW, dstH int) bool {
	return s.srcW == srcW && s.srcH == srcH && s.dstW == dstW && s.dstH == dstH
}

// newTaps computes box filter weights from src samples down to dst. Output i
// covers source positions [i*src/dst, (i+1)*src/dst); in units of 1/dst of a
// source sample that is [i*src, (i+1)*src), so coverage is exact.
func newTaps(src, dst int) taps {
	t := taps{first: make([]int, dst)}
	for i := range dst {
		lo, hi := i*src/dst, ((i+1)*src+dst-1)/dst
		t.n = max(t.n, hi-lo)
	}
	t.w = make([]uint32, t.n*dst)
	one := uint32(1) << weightBits
	for i := range dst {
		lo, hi := i*src/dst, ((i+1)*src+dst-1)/dst
		// Keep all n taps inside the source; the extra ones weigh nothing.
		first := min(lo, src-t.n)
		t.first[i] = first
		w := t.w[t.n*i : t.n*i+t.n]
		var sum uint32
		big := 0
		for j := lo; j < hi; j++ {
			cover := min((j+1)*dst, (i+1)*src) - max(j*dst, i*src)
			k := j - first
			w[k] = uint32(cover) * one / uint32(src)
			sum += w[k]
			if w[k] > w[big] {
				big = k
			}
		}
		w[big] += one - sum // rounding leftovers go to the heaviest tap
	}
	return t
}

// Convert fills dst, which must be dstW x dstH, from src, srcW x srcH BGRX
// pixels with rows stride bytes apart.
func (s *Scaler) Convert(dst *I420, src []byte, stride int) {
	pairs := s.dstH / 2
	n := len(s.work)
	if n <= 1 {
		s.convertPairs(&s.work[0], dst, src, stride, 0, pairs)
		return
	}
	var wg sync.WaitGroup
	per := (pairs + n - 1) / n
	for i, start := 0, 0; start < pairs; i, start = i+1, start+per {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.convertPairs(&s.work[i], dst, src, stride, start, min(start+per, pairs))
		}()
	}
	wg.Wait()
}

func (s *Scaler) convertPairs(sc *scratch, dst *I420, src []byte, stride, from, to int) {
	w, cw := s.dstW, s.dstW/2
	for p := from; p < to; p++ {
		s.scaleRow(sc, sc.rows[0], src, stride, 2*p)
		s.scaleRow(sc, sc.rows[1], src, stride, 2*p+1)
		convertPair(sc.rows[0], sc.rows[1],
			dst.Y[2*p*w:2*p*w+w], dst.Y[(2*p+1)*w:(2*p+1)*w+w],
			dst.U[p*cw:p*cw+cw], dst.V[p*cw:p*cw+cw])
	}
}

// scaleRow computes output row y as BGRX: the source rows it covers are
// blended into sc.acc, then each output pixel blends the columns it covers.
func (s *Scaler) scaleRow(sc *scratch, out, src []byte, stride, y int) {
	acc := sc.acc
	n := s.rows.n
	first := s.rows.first[y]
	for k, wy := range s.rows.w[n*y : n*y+n] {
		row := src[(first+k)*stride : (first+k)*stride+4*s.srcW]
		if k == 0 {
			for x := range s.srcW {
				p := row[4*x : 4*x+3 : 4*x+3]
				a := acc[3*x : 3*x+3 : 3*x+3]
				a[0], a[1], a[2] = uint32(p[0])*wy, uint32(p[1])*wy, uint32(p[2])*wy
			}
			continue
		}
		if wy == 0 {
			continue
		}
		for x := range s.srcW {
			p := row[4*x : 4*x+3 : 4*x+3]
			a := acc[3*x : 3*x+3 : 3*x+3]
			a[0] += uint32(p[0]) * wy
			a[1] += uint32(p[1]) * wy
			a[2] += uint32(p[2]) * wy
		}
	}

	n = s.cols.n
	const round = 1 << (2*weightBits - 1)
	for x := range s.dstW {
		c := acc[3*s.cols.first[x] : 3*(s.cols.first[x]+n)]
		var b, g, r uint32
		for k, wx := range s.cols.w[n*x : n*x+n] {
			a := c[3*k : 3*k+3 : 3*k+3]
			b += a[0] * wx
			g += a[1] * wx
			r += a[2] * wx
		}
		o := out[4*x : 4*x+3 : 4*x+3]
		o[0], o[1], o[2] = uint8((b+round)>>(2*weightBits)), uint8((g+round)>>(2*weightBits)), uint8((r+round)>>(2*weightBits))
	}
}

// workers is how many goroutines convert a frame of the given row pairs.
func workers(pairs int) int {
	return max(min(runtime.GOMAXPROCS(0), 4, pairs), 1)
}
