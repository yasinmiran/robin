// Package yuv converts captured BGRX frames to the I420 layout the encoder
// takes.
package yuv

import (
	"runtime"
	"sync"
)

// I420 is a planar 4:2:0 image with tightly packed planes. Width and Height
// are even.
type I420 struct {
	Width, Height int
	Y, U, V       []byte
}

// NewI420 allocates an image; odd dimensions are rounded down to even.
func NewI420(width, height int) *I420 {
	w, h := width&^1, height&^1
	return &I420{
		Width: w, Height: h,
		Y: make([]byte, w*h),
		U: make([]byte, w*h/4),
		V: make([]byte, w*h/4),
	}
}

// FromBGRX fills dst from src, 4 bytes per pixel in B, G, R, X order with rows
// stride bytes apart, as X11 delivers 24-bit depth ZPixmaps on little-endian
// hosts. src must cover dst's dimensions. It uses BT.601 limited range, which
// is what browsers assume for an H.264 stream without colour metadata.
func FromBGRX(dst *I420, src []byte, stride int) {
	pairs := dst.Height / 2
	workers := min(runtime.GOMAXPROCS(0), 4, pairs)
	if workers <= 1 {
		convertRows(dst, src, stride, 0, pairs)
		return
	}
	var wg sync.WaitGroup
	per := (pairs + workers - 1) / workers
	for start := 0; start < pairs; start += per {
		end := min(start+per, pairs)
		wg.Add(1)
		go func() {
			defer wg.Done()
			convertRows(dst, src, stride, start, end)
		}()
	}
	wg.Wait()
}

// convertRows converts row pairs [from, to): each pair yields two luma rows
// and one chroma row averaged over 2x2 blocks.
func convertRows(dst *I420, src []byte, stride, from, to int) {
	w := dst.Width
	cw := w / 2
	for p := from; p < to; p++ {
		r0 := src[2*p*stride : 2*p*stride+4*w]
		r1 := src[(2*p+1)*stride : (2*p+1)*stride+4*w]
		y0 := dst.Y[2*p*w : 2*p*w+w]
		y1 := dst.Y[(2*p+1)*w : (2*p+1)*w+w]
		u := dst.U[p*cw : p*cw+cw]
		v := dst.V[p*cw : p*cw+cw]
		for x := 0; x < cw; x++ {
			i := 8 * x
			a := r0[i : i+8 : i+8]
			b := r1[i : i+8 : i+8]
			bs := int32(a[0]) + int32(a[4]) + int32(b[0]) + int32(b[4])
			gs := int32(a[1]) + int32(a[5]) + int32(b[1]) + int32(b[5])
			rs := int32(a[2]) + int32(a[6]) + int32(b[2]) + int32(b[6])
			y0[2*x] = luma(a[2], a[1], a[0])
			y0[2*x+1] = luma(a[6], a[5], a[4])
			y1[2*x] = luma(b[2], b[1], b[0])
			y1[2*x+1] = luma(b[6], b[5], b[4])
			// Sums of four samples: the extra >>2 averages them.
			u[x] = uint8(((-38*rs - 74*gs + 112*bs + 512) >> 10) + 128)
			v[x] = uint8(((112*rs - 94*gs - 18*bs + 512) >> 10) + 128)
		}
	}
}

func luma(r, g, b uint8) uint8 {
	return uint8(((66*int32(r) + 129*int32(g) + 25*int32(b) + 128) >> 8) + 16)
}
