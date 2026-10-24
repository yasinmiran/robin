//go:build linux && cgo && openh264

package h264

import (
	"math"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agent/yuv"
)

// camera fills img with a gradient that moves by frame, the colour of a
// face under a lamp.
func camera(img *yuv.I420, frame int) {
	for row := range img.Height {
		for col := range img.Width {
			img.Y[row*img.Width+col] = byte(60 + (col+row+frame*4)%120)
		}
	}
	for i := range img.U {
		img.U[i], img.V[i] = 110, 150
	}
}

func encodeFrames(t *testing.T, w, h, n int) ([][]byte, []*yuv.I420) {
	t.Helper()
	enc, err := New(w, h, 30, 1_500_000)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	var aus [][]byte
	var imgs []*yuv.I420
	for i := range n {
		img := yuv.NewI420(w, h)
		camera(img, i)
		au, err := enc.Encode(img, time.Duration(i)*time.Second/30, i == 0)
		if err != nil {
			t.Fatal(err)
		}
		aus = append(aus, append([]byte(nil), au...))
		imgs = append(imgs, img)
	}
	return aus, imgs
}

func psnr(a []byte, aStride int, b *yuv.I420) float64 {
	var sum float64
	for row := range b.Height {
		for col := range b.Width {
			d := float64(a[row*aStride+col]) - float64(b.Y[row*b.Width+col])
			sum += d * d
		}
	}
	mse := sum / float64(b.Width*b.Height)
	if mse == 0 {
		return 99
	}
	return 10 * math.Log10(255*255/mse)
}

func TestDecoderRoundTrip(t *testing.T) {
	aus, imgs := encodeFrames(t, 640, 360, 10)
	d, err := NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for i, au := range aus {
		if len(au) == 0 {
			continue // rate control skipped it
		}
		pic, err := d.Decode(au)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if pic == nil {
			t.Fatalf("frame %d: no picture", i)
		}
		if pic.Width != 640 || pic.Height != 360 || pic.YStride < 640 || pic.CStride < 320 {
			t.Fatalf("frame %d: %dx%d strides %d/%d", i, pic.Width, pic.Height, pic.YStride, pic.CStride)
		}
		if p := psnr(pic.Y, pic.YStride, imgs[i]); p < 30 {
			t.Errorf("frame %d decodes at %.1f dB", i, p)
		}
		if u, v := pic.U[pic.CStride*10+10], pic.V[pic.CStride*10+10]; absDiff(u, 110) > 4 || absDiff(v, 150) > 4 {
			t.Errorf("frame %d chroma %d/%d, want 110/150", i, u, v)
		}
	}
}

func absDiff(a, b byte) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}

// TestDecoderNeedsAKeyframe: a stream joined after its IDR, or one that lost
// a frame, fails to decode until the next IDR comes.
func TestDecoderNeedsAKeyframe(t *testing.T) {
	aus, _ := encodeFrames(t, 320, 240, 6)
	d, err := NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Decode(aus[3]); err == nil {
		t.Error("a P frame with nothing before it decoded")
	}
	if pic, err := d.Decode(aus[0]); err != nil || pic == nil {
		t.Fatalf("the IDR after: %v, %v", pic, err)
	}
	if pic, err := d.Decode(nil); pic != nil || err != nil {
		t.Errorf("an empty unit gave %v, %v", pic, err)
	}
	d.Close()
	if _, err := d.Decode(aus[1]); err == nil {
		t.Error("a closed decoder decoded")
	}
	d.Close()
}

func TestDecoderRejectsGarbage(t *testing.T) {
	d, err := NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if pic, err := d.Decode([]byte{0, 0, 0, 1, 0x65, 0xff, 0x00, 0x12}); err == nil && pic != nil {
		t.Error("a slice with no parameter sets decoded to a picture")
	}
}
