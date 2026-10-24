//go:build linux && cgo && openh264

package vqtest

import (
	"testing"
	"time"

	"github.com/pion/rtp/codecs"

	"github.com/yasinmiran/robin/internal/agent/h264"
	"github.com/yasinmiran/robin/internal/agent/yuv"
)

// encodeClip encodes the first n frames of clip with openh264, a keyframe
// first, and returns the access units.
func encodeClip(t *testing.T, clip *Clip, n, bps int) [][]byte {
	t.Helper()
	enc, err := h264.New(clip.W, clip.H, 30, bps)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	pix := make([]byte, 4*clip.W*clip.H)
	img := yuv.NewI420(clip.W, clip.H)
	var aus [][]byte
	for i := range n {
		clip.Draw(pix, i)
		yuv.FromBGRX(img, pix, 4*clip.W)
		au, err := enc.Encode(img, time.Duration(i)*time.Second/30, i == 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(au) > 0 {
			aus = append(aus, Tag(au, i))
		}
	}
	return aus
}

// packetize splits access units into RTP packets as the agent's track
// does, one timestamp per access unit.
func packetize(aus [][]byte) []Packet {
	var out []Packet
	var pay codecs.H264Payloader
	seq := uint16(65000)
	for i, au := range aus {
		payloads := pay.Payload(1200, au)
		for j, p := range payloads {
			out = append(out, Packet{Seq: seq, Timestamp: uint32(3000 * i), Marker: j == len(payloads)-1, Payload: p})
			seq++
		}
	}
	return out
}

func TestDecodeWhatTheReceiverAssembles(t *testing.T) {
	clip, err := NewClip(Captured, 320, 240, Segment{Scroll, 10})
	if err != nil {
		t.Fatal(err)
	}
	aus := encodeClip(t, clip, 10, 2_000_000)
	dec, err := NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	rx := NewReceiver(Chrome, time.Now())
	shown := 0
	for _, p := range packetize(aus) {
		for _, f := range rx.Push(p, time.Now()) {
			var depay codecs.H264Packet
			var au []byte
			for _, pl := range f.Payloads {
				nal, err := depay.Unmarshal(pl)
				if err != nil {
					t.Fatal(err)
				}
				au = append(au, nal...)
			}
			img, err := dec.Decode(au)
			if err != nil {
				t.Fatalf("frame %d: %v", shown, err)
			}
			if img == nil {
				t.Fatalf("frame %d decoded to nothing", shown)
			}
			i, ok := FrameIndex(au)
			if !ok {
				t.Fatalf("frame %d lost its tag", shown)
			}
			pix := make([]byte, 4*clip.W*clip.H)
			clip.Draw(pix, i)
			src := yuv.NewI420(clip.W, clip.H)
			yuv.FromBGRX(src, pix, 4*clip.W)
			if s := Compare(src.Y, clip.W, clip.H, img.Y, img.Width, img.Height); s.PSNR < 25 || s.SSIM < 0.8 {
				t.Errorf("frame %d scores %+v against its source", i, s)
			}
			shown++
		}
	}
	if shown != len(aus) {
		t.Fatalf("decoded %d of %d access units", shown, len(aus))
	}
	if _, err := dec.Decode(nil); err == nil {
		t.Error("decoded an empty access unit")
	}
	if _, err := dec.Decode([]byte{0, 0, 0, 1, 0x65, 0xff, 0xff}); err == nil {
		t.Error("decoded a broken slice without parameter sets")
	}
	dec.Close()
	if _, err := dec.Decode(aus[0]); err == nil {
		t.Error("a closed decoder decoded")
	}
}
