package camera

import (
	"bytes"
	"testing"

	"github.com/yasinmiran/robin/internal/agent/h264"
)

// maxMacroblocks is the most 16x16 macroblocks a picture of H.264 level
// 5.2, the highest openh264 decodes, holds.
const maxMacroblocks = 36864

// FuzzFitter fits pictures of any size the decoder can report, with any
// padding past each row, into the camera's frame: nothing reads outside
// the picture's planes, and every byte of the frame is written.
func FuzzFitter(f *testing.F) {
	for _, s := range [][2]uint16{{1280, 720}, {640, 360}, {640, 480}, {333, 187}, {1, 1}, {1, 8688}, {8688, 1}, {2560, 1080}, {720, 720}, {1281, 721}} {
		f.Add(s[0], s[1], uint8(0), uint8(0), byte(7))
		f.Add(s[0], s[1], uint8(33), uint8(17), byte(200))
	}
	f.Fuzz(func(t *testing.T, w, h uint16, ypad, cpad uint8, seed byte) {
		srcW, srcH := int(w), int(h)
		if srcW == 0 || srcH == 0 || ((srcW+15)/16)*((srcH+15)/16) > maxMacroblocks {
			return
		}
		ys, cs := srcW+int(ypad), (srcW+1)/2+int(cpad)
		pic := &h264.Picture{
			Width: srcW, Height: srcH, YStride: ys, CStride: cs,
			Y: make([]byte, ys*srcH), U: make([]byte, cs*((srcH+1)/2)), V: make([]byte, cs*((srcH+1)/2)),
		}
		for _, p := range [][]byte{pic.Y, pic.U, pic.V} {
			for i := range p {
				p[i] = seed + byte(i*7)
			}
		}
		fit := newFitter(srcW, srcH)
		if fit.w < 2 || fit.h < 2 || fit.w > Width || fit.h > Height || fit.x0+fit.w > Width || fit.y0+fit.h > Height {
			t.Fatalf("a %dx%d picture fits as %dx%d at %d,%d", srcW, srcH, fit.w, fit.h, fit.x0, fit.y0)
		}
		// A byte the fitter does not write keeps what the frame held.
		a, b := newFrame(), newFrame()
		fillBytes(b, 0xff)
		fit.fit(a, pic)
		fit.fit(b, pic)
		if i := mismatch(a, b); i >= 0 {
			t.Fatalf("fitting a %dx%d picture left byte %d of the frame as it was", srcW, srcH, i)
		}
	})
}

func mismatch(a, b []byte) int {
	if bytes.Equal(a, b) {
		return -1
	}
	for i := range a {
		if a[i] != b[i] {
			return i
		}
	}
	return len(a)
}
