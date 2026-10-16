package yuv

import "testing"

func solid(w, h, stride int, r, g, b uint8) []byte {
	src := make([]byte, stride*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*stride + 4*x
			src[i], src[i+1], src[i+2] = b, g, r
		}
	}
	return src
}

func near(a, b uint8) bool { return a >= b-1 && a <= b+1 }

func TestFromBGRXColours(t *testing.T) {
	// Reference values: BT.601 limited range.
	for _, tc := range []struct {
		name    string
		r, g, b uint8
		y, u, v uint8
	}{
		{"black", 0, 0, 0, 16, 128, 128},
		{"white", 255, 255, 255, 235, 128, 128},
		{"red", 255, 0, 0, 82, 90, 240},
		{"green", 0, 255, 0, 145, 54, 34},
		{"blue", 0, 0, 255, 41, 240, 110},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, h, stride := 64, 36, 64*4+32 // padded stride, like a real capture
			dst := NewI420(w, h)
			FromBGRX(dst, solid(w, h, stride, tc.r, tc.g, tc.b), stride)
			for i, got := range dst.Y {
				if !near(got, tc.y) {
					t.Fatalf("Y[%d]=%d want %d", i, got, tc.y)
				}
			}
			for i := range dst.U {
				if !near(dst.U[i], tc.u) || !near(dst.V[i], tc.v) {
					t.Fatalf("UV[%d]=%d,%d want %d,%d", i, dst.U[i], dst.V[i], tc.u, tc.v)
				}
			}
		})
	}
}

func TestFromBGRXOddSizeRoundsDown(t *testing.T) {
	dst := NewI420(1367, 769)
	if dst.Width != 1366 || dst.Height != 768 {
		t.Fatalf("got %dx%d", dst.Width, dst.Height)
	}
	FromBGRX(dst, solid(1367, 769, 1367*4, 10, 20, 30), 1367*4)
}

func TestFromBGRXChromaAveragesBlocks(t *testing.T) {
	// Left column white, right column black inside each 2x2 block: chroma
	// stays neutral, luma keeps the edge.
	w, h := 2, 2
	src := []byte{255, 255, 255, 0, 0, 0, 0, 0, 255, 255, 255, 0, 0, 0, 0, 0}
	dst := NewI420(w, h)
	FromBGRX(dst, src, 8)
	if dst.Y[0] != 235 || dst.Y[1] != 16 || !near(dst.U[0], 128) || !near(dst.V[0], 128) {
		t.Fatalf("got Y=%v U=%v V=%v", dst.Y, dst.U, dst.V)
	}
}

func BenchmarkFromBGRX1080p(b *testing.B) {
	w, h := 1920, 1080
	src := solid(w, h, w*4, 30, 120, 200)
	dst := NewI420(w, h)
	b.SetBytes(int64(len(src)))
	for b.Loop() {
		FromBGRX(dst, src, w*4)
	}
}
