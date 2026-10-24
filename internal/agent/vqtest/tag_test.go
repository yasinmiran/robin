package vqtest

import (
	"bytes"
	"testing"
)

func TestTagRoundTrips(t *testing.T) {
	sps := []byte{0, 0, 0, 1, 0x67, 1, 2}
	pps := []byte{0, 0, 1, 0x68, 3}
	idr := []byte{0, 0, 0, 1, 0x65, 9, 9}
	au := bytes.Join([][]byte{sps, pps, idr}, nil)
	tagged := Tag(au, 1234)
	if n, ok := FrameIndex(tagged); !ok || n != 1234 {
		t.Fatalf("FrameIndex = %d, %v", n, ok)
	}
	sei := bytes.Index(tagged, []byte{0, 0, 0, 1, nalSEI})
	if sei < len(sps)+len(pps) || !bytes.HasSuffix(tagged, idr) {
		t.Fatalf("the SEI is not just before the slice: % x", tagged)
	}
	if !bytes.Equal(au, bytes.Join([][]byte{sps, pps, idr}, nil)) {
		t.Fatal("Tag changed its input")
	}
	if !IsKeyframe(tagged) || IsKeyframe(Tag([]byte{0, 0, 1, 0x41, 7}, 1)) {
		t.Fatal("IsKeyframe is wrong")
	}
	// A slice after a three-byte start code, and no slice at all.
	if got := Tag([]byte{0, 0, 1, 0x41, 7}, 5); !bytes.HasSuffix(got, []byte{0, 0, 1, 0x41, 7}) {
		t.Fatalf("% x", got)
	}
	if got := Tag(sps, 6); !bytes.HasPrefix(got, sps) {
		t.Fatalf("% x", got)
	}
}

func TestFrameIndexIgnoresOtherSEI(t *testing.T) {
	other := []byte{0, 0, 1, nalSEI, 5, 20}
	other = append(append(other, []byte("not-our-uuid-here")...), '1', '2', '3', 0x80)
	if _, ok := FrameIndex(other); ok {
		t.Fatal("took another SEI for a tag")
	}
	bad := append([]byte{0, 0, 1, nalSEI, 5, byte(len(tagUUID) + 2)}, tagUUID...)
	bad = append(bad, 'x', 'y', 0x80)
	if _, ok := FrameIndex(bad); ok {
		t.Fatal("parsed a tag with no number")
	}
	cut := []byte{0, 0, 1, nalSEI, 5}
	if _, ok := FrameIndex(cut); ok {
		t.Fatal("parsed a cut-off SEI")
	}
	short := append([]byte{0, 0, 1, nalSEI, 5, 40}, tagUUID...)
	if _, ok := FrameIndex(short); ok {
		t.Fatal("parsed a tag shorter than its size")
	}
	if n, ok := FrameIndex(append(other, Tag(nil, 7)...)); !ok || n != 7 {
		t.Fatalf("missed a tag after another SEI: %d, %v", n, ok)
	}
}
