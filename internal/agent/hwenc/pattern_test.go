package hwenc

import (
	"math/rand/v2"

	"github.com/yasinmiran/robin/internal/agent/yuv"
)

// The patterns are the h264 package's rate-control content, at any size.

// drawDesktop draws a light background with rows of glyph-like blocks
// shifted up by scroll pixels, like a terminal or an editor.
func drawDesktop(img *yuv.I420, scroll int) {
	w := img.Width
	for row := 0; row < img.Height; row++ {
		line := row + scroll
		for col := 0; col < w; col++ {
			c := byte(235)
			if line%24 < 14 && col%9 < 6 && ((line/24)*131+(col/9)*71)%5 != 0 {
				c = byte(20 + (line*7+col*13)%40)
			}
			img.Y[row*w+col] = c
		}
	}
	for i := range img.U {
		img.U[i], img.V[i] = 128, 128
	}
}

// drawNoise fills a rectangle with a moving gradient under fresh noise,
// which is what video playback looks like to an encoder.
func drawNoise(img *yuv.I420, x0, y0, w, h, frame int) {
	r := rand.New(rand.NewPCG(uint64(frame), 7))
	for row := y0; row < y0+h; row++ {
		for col := x0; col < x0+w; col++ {
			g := (col+frame*6)/4 + (row+frame*3)/6
			img.Y[row*img.Width+col] = byte(g + int(r.Uint32()%17) - 8)
		}
	}
	cw := img.Width / 2
	for row := y0 / 2; row < (y0+h)/2; row++ {
		for col := x0 / 2; col < (x0+w)/2; col++ {
			img.U[row*cw+col] = byte(100 + (col+frame)%60)
			img.V[row*cw+col] = byte(140 - (row+frame)%50)
		}
	}
}

type pattern struct {
	name string
	draw func(img *yuv.I420, frame int)
}

var patterns = []pattern{
	{"typing", func(img *yuv.I420, i int) {
		if i == 0 {
			drawDesktop(img, 0)
		}
		// One glyph cell changes every frame, like keystrokes at the rate
		// the stream would encode them.
		x, y := 40+i*9%(img.Width-80), img.Height/2
		for row := y; row < y+14; row++ {
			for col := x; col < x+6; col++ {
				img.Y[row*img.Width+col] = byte(30 + i%50)
			}
		}
	}},
	{"scroll", func(img *yuv.I420, i int) { drawDesktop(img, i*6) }},
	{"video-window", func(img *yuv.I420, i int) {
		if i == 0 {
			drawDesktop(img, 0)
		}
		drawNoise(img, img.Width/5, img.Height/4, img.Width*4/9&^1, img.Height*4/9&^1, i)
	}},
	{"video-fullscreen", func(img *yuv.I420, i int) { drawNoise(img, 0, 0, img.Width, img.Height, i) }},
}

// sliceType returns slice_type mod 5 of a slice NAL unit (0 P, 1 B, 2 I),
// or -1 when nal is not a slice.
func sliceType(nal []byte) int {
	if t := nalType(nal); t != nalSlice && t != nalIDR {
		return -1
	}
	b := bitReader{data: unescape(nal[1:min(len(nal), 32)])}
	b.ue() // first_mb_in_slice
	return b.ue() % 5
}

// unescape removes emulation prevention bytes.
func unescape(rbsp []byte) []byte {
	out := make([]byte, 0, len(rbsp))
	zeros := 0
	for _, c := range rbsp {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, c)
	}
	return out
}

type bitReader struct {
	data []byte
	pos  int
}

func (b *bitReader) bit() int {
	if b.pos >= 8*len(b.data) {
		return 0
	}
	v := int(b.data[b.pos/8]>>(7-b.pos%8)) & 1
	b.pos++
	return v
}

// ue reads an unsigned Exp-Golomb code.
func (b *bitReader) ue() int {
	zeros := 0
	for b.bit() == 0 && zeros < 32 {
		zeros++
	}
	v := 1
	for range zeros {
		v = v<<1 | b.bit()
	}
	return v - 1
}
