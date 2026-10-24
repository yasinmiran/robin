package vqtest

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"math/rand/v2"
	"sync"
)

// screenPNG is a 2560x1440 screen of small text: an editor with two code
// panes, a document and a terminal, rendered by Chrome (testdata/render.sh).
//
//go:embed testdata/screen.png
var screenPNG []byte

// Background is the still picture a clip's motion happens over.
type Background int

const (
	// Captured is the screen in testdata/screen.png, cropped about its
	// centre: real antialiased 12 to 14 px text.
	Captured Background = iota
	// Synthetic is rows of glyph-sized blocks on a light background, the
	// pattern the encoder packages tune rate control with.
	Synthetic
)

// Kind is what happens on the screen during a segment of a clip.
type Kind int

// The kinds of segment.
const (
	// Still shows the background and then nothing changes.
	Still Kind = iota
	// Typing reveals a line of text a character every third frame.
	Typing
	// Scroll moves the editor area up by 9 px a frame.
	Scroll
	// Drag moves a window across the screen by 12 px right and 6 px up a
	// frame, bouncing off the edges.
	Drag
	// Video plays fresh noise over a moving gradient in a third of the
	// screen.
	Video
)

var kindNames = [...]string{"still", "typing", "scroll", "drag", "video"}

func (k Kind) String() string {
	if k < 0 || int(k) >= len(kindNames) {
		return fmt.Sprintf("kind(%d)", int(k))
	}
	return kindNames[k]
}

// Segment is a stretch of a clip.
type Segment struct {
	Kind   Kind
	Frames int
}

// Clip is a deterministic screen recording, drawn on demand: frame i is the
// same whoever asks for it, so the stream and the scorer each draw their
// own copy instead of holding hundreds of large frames.
type Clip struct {
	W, H   int
	segs   []Segment
	starts []int
	base   []byte // BGRX, W*H*4
	// lineH and charW place typed text on the background's text grid.
	lineH, lineY int
	charW        float64
}

// NewClip makes a w x h clip over bg from segs. A captured background is at
// most 2560x1440.
func NewClip(bg Background, w, h int, segs ...Segment) (*Clip, error) {
	if w < 64 || h < 64 || w%2 != 0 || h%2 != 0 {
		return nil, fmt.Errorf("vqtest: bad clip size %dx%d", w, h)
	}
	c := &Clip{W: w, H: h, segs: segs}
	n := 0
	for _, s := range segs {
		if s.Frames <= 0 {
			return nil, fmt.Errorf("vqtest: %s segment of %d frames", s.Kind, s.Frames)
		}
		c.starts = append(c.starts, n)
		n += s.Frames
	}
	switch bg {
	case Captured:
		screen, err := capturedScreen()
		if err != nil {
			return nil, err
		}
		sw, sh := screen.Rect.Dx(), screen.Rect.Dy()
		if w > sw || h > sh {
			return nil, fmt.Errorf("vqtest: the captured screen is %dx%d, smaller than %dx%d", sw, sh, w, h)
		}
		x0, y0 := (sw-w)/2, (sh-h)/2
		c.base = make([]byte, w*h*4)
		for y := range h {
			row := screen.Pix[(y0+y)*screen.Stride+4*x0:]
			for x := range w {
				r, g, b := row[4*x], row[4*x+1], row[4*x+2]
				c.base[4*(y*w+x)], c.base[4*(y*w+x)+1], c.base[4*(y*w+x)+2] = b, g, r
			}
		}
		// Editor lines are 18 px apart from y 34 of the screen, in
		// Liberation Mono 13 px.
		c.lineH, c.charW = 18, 7.8
		c.lineY = ((34-y0)%18 + 18) % 18
	case Synthetic:
		c.base = make([]byte, w*h*4)
		for y := range h {
			for x := range w {
				v := byte(235)
				if y%24 < 14 && x%9 < 6 && ((y/24)*131+(x/9)*71)%5 != 0 {
					v = byte(20 + (y*7+x*13)%40)
				}
				p := c.base[4*(y*w+x):]
				p[0], p[1], p[2] = v, v, v
			}
		}
		c.lineH, c.lineY, c.charW = 24, 0, 9
	default:
		return nil, fmt.Errorf("vqtest: unknown background %d", bg)
	}
	return c, nil
}

var screenOnce = sync.OnceValues(func() (*image.RGBA, error) {
	img, err := png.Decode(bytes.NewReader(screenPNG))
	if err != nil {
		return nil, fmt.Errorf("vqtest: captured screen: %w", err)
	}
	rgba := image.NewRGBA(img.Bounds())
	draw.Draw(rgba, rgba.Rect, img, img.Bounds().Min, draw.Src)
	return rgba, nil
})

func capturedScreen() (*image.RGBA, error) { return screenOnce() }

// Len is the number of frames.
func (c *Clip) Len() int {
	if len(c.segs) == 0 {
		return 0
	}
	return c.starts[len(c.starts)-1] + c.segs[len(c.segs)-1].Frames
}

// At reports the segment frame i falls in and how far into it.
func (c *Clip) At(i int) (Kind, int) {
	s := len(c.starts) - 1
	for s > 0 && c.starts[s] > i {
		s--
	}
	return c.segs[s].Kind, i - c.starts[s]
}

// Changed reports whether frame i differs from frame i-1.
func (c *Clip) Changed(i int) bool {
	switch kind, j := c.At(i); kind {
	case Still:
		return j == 0
	case Typing:
		return j%3 == 0
	default:
		return true
	}
}

// Draw writes frame i into pix, BGRX with rows 4*W bytes apart.
func (c *Clip) Draw(pix []byte, i int) {
	kind, j := c.At(i)
	copy(pix, c.base)
	switch kind {
	case Typing:
		c.typing(pix, j)
	case Scroll:
		c.scroll(pix, j)
	case Drag:
		c.drag(pix, j)
	case Video:
		c.video(pix, j)
	}
}

// typing blanks a line about 45% down the screen and reveals, from its
// left, the text two lines below it, one character more every third frame.
func (c *Clip) typing(pix []byte, j int) {
	w := c.W
	y0 := c.lineY + (c.H*45/100-c.lineY)/c.lineH*c.lineH
	x0, x1 := c.W/5, c.W*3/5
	src := y0 + 2*c.lineH
	if src+c.lineH > c.H {
		src = y0 - 2*c.lineH
	}
	shown := x0 + int(float64(j/3+1)*c.charW)
	blank := c.base[4*(y0*w+x1) : 4*(y0*w+x1)+4]
	for y := range c.lineH {
		row := pix[4*(y0+y)*w:]
		from := c.base[4*(src+y)*w:]
		for x := x0; x < x1; x++ {
			if x < shown {
				copy(row[4*x:4*x+4], from[4*x:4*x+4])
			} else {
				copy(row[4*x:4*x+4], blank)
			}
		}
	}
}

// scroll moves the editor area up, wrapping its top back in at the bottom.
func (c *Clip) scroll(pix []byte, j int) {
	w := c.W
	x0, x1 := c.W*11/100, c.W*80/100
	y0, y1 := c.H*3/100, c.H*73/100
	rh := y1 - y0
	off := 9 * (j + 1) % rh
	for y := y0; y < y1; y++ {
		from := y0 + (y-y0+off)%rh
		copy(pix[4*(y*w+x0):4*(y*w+x1)], c.base[4*(from*w+x0):4*(from*w+x1)])
	}
}

// drag draws a copy of part of the screen, a window, at a position that
// moves every frame.
func (c *Clip) drag(pix []byte, j int) {
	w := c.W
	sx, sy := c.W/10, c.H/2
	ww, wh := c.W*35/100, c.H*30/100
	x := bounce(sx+12*(j+1), c.W-ww)
	y := bounce(sy-6*(j+1), c.H-wh)
	for r := range wh {
		copy(pix[4*((y+r)*w+x):4*((y+r)*w+x+ww)], c.base[4*((sy+r)*w+sx):4*((sy+r)*w+sx+ww)])
	}
}

// bounce folds p into [0, limit] as if it bounced off both ends.
func bounce(p, limit int) int {
	if limit <= 0 {
		return 0
	}
	p %= 2 * limit
	if p < 0 {
		p += 2 * limit
	}
	if p > limit {
		p = 2*limit - p
	}
	return p
}

// video fills a third of the screen with a moving colour gradient under
// fresh noise, which is what playback looks like to an encoder.
func (c *Clip) video(pix []byte, j int) {
	w := c.W
	x0, y0 := c.W*55/100, c.H*20/100
	vw, vh := c.W/3, c.H/3
	r := rand.New(rand.NewPCG(uint64(j), 7))
	for y := y0; y < y0+vh; y++ {
		row := pix[4*y*w:]
		for x := x0; x < x0+vw; x++ {
			g := (x+j*6)/4 + (y+j*3)/6
			n := int(r.Uint32()%17) - 8
			p := row[4*x : 4*x+4]
			p[0] = byte(g + n)
			p[1] = byte(g/2 + 60 + n)
			p[2] = byte(200 - g/3 + n)
		}
	}
}
