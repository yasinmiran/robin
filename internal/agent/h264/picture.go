package h264

import (
	"fmt"
	"unsafe"
)

// Picture is a decoded frame in I420, its planes rows of YStride and
// CStride bytes. Width and Height are the picture's own size, which the
// strides may exceed.
type Picture struct {
	Width, Height    int
	YStride, CStride int
	Y, U, V          []byte
}

// pictureAt is the picture of size w by h whose planes start at y, u and v
// with strides ys and cs, as the decoder reports it. A size and strides
// that do not describe I420 planes, or a plane missing, are refused before
// anything is read: whoever reads the planes indexes by them.
func pictureAt(w, h, ys, cs int, y, u, v unsafe.Pointer) (*Picture, error) {
	if w <= 0 || h <= 0 || ys < w || cs < (w+1)/2 || y == nil || u == nil || v == nil {
		return nil, fmt.Errorf("h264: decoder reported a %dx%d picture with strides %d and %d", w, h, ys, cs)
	}
	ch := (h + 1) / 2
	return &Picture{
		Width: w, Height: h, YStride: ys, CStride: cs,
		Y: unsafe.Slice((*byte)(y), ys*h),
		U: unsafe.Slice((*byte)(u), cs*ch),
		V: unsafe.Slice((*byte)(v), cs*ch),
	}, nil
}
