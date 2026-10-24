package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"testing"
)

// The page builds and reads these by hand, so the wire form is the contract.
func TestTilesWireForm(t *testing.T) {
	for _, c := range []struct {
		name string
		v    Control
		want string
	}{
		{"ask", Control{T: ControlTiles, On: true}, `{"t":"tiles","on":true}`},
		{"unsupported", Control{T: ControlTiles, Tiles: &Tiles{Unsupported: "no"}}, `{"t":"tiles","tiles":{"unsupported":"no"}}`},
		{"off", Control{T: ControlTiles, Tiles: &Tiles{}}, `{"t":"tiles","tiles":{}}`},
		{"manifest", Control{T: ControlFrame, Frame: &Frame{RTP: 4294967295, Gen: 2, W: 100, H: 70, Gone: []int{0, 2}}},
			`{"t":"frame","frame":{"rtp":4294967295,"gen":2,"w":100,"h":70,"gone":[0,2]}}`},
		{"manifest of no change", Control{T: ControlFrame, Frame: &Frame{RTP: 9, Gen: 1, W: 64, H: 64}}, `{"t":"frame","frame":{"rtp":9,"gen":1,"w":64,"h":64}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(c.v)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != c.want {
				t.Errorf("marshals to\n%s\nwant\n%s", b, c.want)
			}
		})
	}
}

// atlasHead is the start of a PNG of w by h pixels: its signature and
// IHDR, with no picture after.
func atlasHead(w, h int) []byte {
	p := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR"), make([]byte, 8)...)
	binary.BigEndian.PutUint32(p[16:], uint32(w))
	binary.BigEndian.PutUint32(p[20:], uint32(h))
	return p
}

func TestTileAtlasSize(t *testing.T) {
	for n, want := range map[int][2]int{1: {64, 64}, 16: {1024, 64}, 17: {1024, 128}, MaxTileBatch: {1024, 1024}} {
		if w, h := TileAtlasSize(n); w != want[0] || h != want[1] {
			t.Errorf("TileAtlasSize(%d) = %dx%d, want %dx%d", n, w, h, want[0], want[1])
		}
	}
}

func TestTileColumns(t *testing.T) {
	for w, want := range map[int]int{1: 1, 64: 1, 65: 2, 1920: 30, 2560: 40} {
		if got := TileColumns(w); got != want {
			t.Errorf("TileColumns(%d) = %d, want %d", w, got, want)
		}
	}
}

func TestTileBatchRoundTrip(t *testing.T) {
	b := TileBatch{RTP: 0xdeadbeef, Gen: 7, Tiles: []int{0, 31, 1 << 20}, PNG: atlasHead(192, 64)}
	p := b.Marshal()
	want := []byte{0xde, 0xad, 0xbe, 0xef, 0, 0, 0, 7, 0, 3, 0, 0, 0, 0, 0, 0, 0, 31, 0, 0x10, 0, 0}
	if !bytes.HasPrefix(p, want) {
		t.Fatalf("head % x, want % x", p[:len(want)], want)
	}
	got, err := ParseTileBatch(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.RTP != b.RTP || got.Gen != b.Gen || len(got.Tiles) != 3 || got.Tiles[2] != 1<<20 || !bytes.Equal(got.PNG, b.PNG) {
		t.Fatalf("round trip gave %+v", got)
	}
	if _, err := ParseTileBatch(TileBatch{Tiles: make([]int, 17), PNG: atlasHead(1024, 128)}.Marshal()); err != nil {
		t.Fatalf("17 tiles in a 1024x128 atlas: %v", err)
	}
}

func TestParseTileBatchRefuses(t *testing.T) {
	tooMany := TileBatch{Tiles: make([]int, MaxTileBatch+1)}.Marshal()
	one := func(png []byte) []byte { return TileBatch{Tiles: []int{0}, PNG: png}.Marshal() }
	notIHDR := atlasHead(64, 64)
	copy(notIHDR[12:], "tEXt")
	for name, p := range map[string][]byte{
		"short":        {0, 0, 0, 1},
		"empty":        TileBatch{}.Marshal(),
		"too many":     tooMany,
		"cut short":    TileBatch{Tiles: []int{1, 2}}.Marshal()[:tileBatchHead+5],
		"no PNG":       one(nil),
		"not a PNG":    one(append([]byte("GIF89a"), make([]byte, 18)...)),
		"no IHDR":      one(notIHDR),
		"IHDR cut":     one(atlasHead(64, 64)[:20]),
		"huge picture": one(atlasHead(65535, 65535)),
		"too narrow":   TileBatch{Tiles: make([]int, 17), PNG: atlasHead(64, 128)}.Marshal(),
		"too short":    TileBatch{Tiles: make([]int, 17), PNG: atlasHead(1024, 64)}.Marshal(),
	} {
		if _, err := ParseTileBatch(p); !errors.Is(err, ErrTileBatch) {
			t.Errorf("%s: %v, want ErrTileBatch", name, err)
		}
	}
}
