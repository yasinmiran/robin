package h264

import (
	"testing"
	"unsafe"
)

// The decoder's report of a picture is taken only when its size and
// strides describe I420 planes that are there.
func TestPictureAt(t *testing.T) {
	// Large enough for every plane taken, so that no slice runs past it.
	buf := make([]byte, 1280*720)
	p := unsafe.Pointer(&buf[0])
	for _, l := range [][4]int{{1280, 720, 1280, 640}, {333, 187, 352, 167}, {1, 1, 1, 1}} {
		pic, err := pictureAt(l[0], l[1], l[2], l[3], p, p, p)
		if err != nil {
			t.Errorf("%v: %v", l, err)
			continue
		}
		if ch := (l[1] + 1) / 2; len(pic.Y) != l[2]*l[1] || len(pic.U) != l[3]*ch || len(pic.V) != l[3]*ch {
			t.Errorf("%v: planes of %d, %d and %d bytes", l, len(pic.Y), len(pic.U), len(pic.V))
		}
	}
	for _, l := range [][4]int{{0, 720, 1280, 640}, {1280, 0, 1280, 640}, {-2, 2, 2, 1}, {1280, 720, 1279, 640}, {333, 187, 352, 166}, {1280, 720, 1280, -1}} {
		if _, err := pictureAt(l[0], l[1], l[2], l[3], p, p, p); err == nil {
			t.Errorf("%v taken", l)
		}
	}
	for _, planes := range [][3]unsafe.Pointer{{nil, p, p}, {p, nil, p}, {p, p, nil}} {
		if _, err := pictureAt(2, 2, 2, 1, planes[0], planes[1], planes[2]); err == nil {
			t.Errorf("planes %v taken", planes)
		}
	}
}
