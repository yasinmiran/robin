package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

// The page builds and reads these by hand, so the wire form is the contract.
func TestTilesWireForm(t *testing.T) {
	for _, c := range []struct {
		v    any
		want string
	}{
		{Control{T: ControlTiles, On: true}, `{"t":"tiles","on":true}`},
		{Control{T: ControlTiles, Tiles: &Tiles{Unsupported: "no"}}, `{"t":"tiles","tiles":{"unsupported":"no"}}`},
		{Control{T: ControlFrame, Frame: &Frame{RTP: 4294967295, Gen: 2, W: 100, H: 70, Gone: []int{0, 2}}},
			`{"t":"frame","frame":{"rtp":4294967295,"gen":2,"w":100,"h":70,"gone":[0,2]}}`},
		{Control{T: ControlFrame, Frame: &Frame{RTP: 9, Gen: 1, W: 64, H: 64}}, `{"t":"frame","frame":{"rtp":9,"gen":1,"w":64,"h":64}}`},
	} {
		b, err := json.Marshal(c.v)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Errorf("%T marshals to\n%s\nwant\n%s", c.v, b, c.want)
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
	b := TileBatch{RTP: 0xdeadbeef, Gen: 7, Tiles: []int{0, 31, 1 << 20}, PNG: []byte("\x89PNG...")}
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
}

func TestParseTileBatchRefuses(t *testing.T) {
	tooMany := TileBatch{Tiles: make([]int, MaxTileBatch+1)}.Marshal()
	for name, p := range map[string][]byte{
		"short":     {0, 0, 0, 1},
		"empty":     TileBatch{}.Marshal(),
		"too many":  tooMany,
		"cut short": TileBatch{Tiles: []int{1, 2}}.Marshal()[:tileBatchHead+5],
	} {
		if _, err := ParseTileBatch(p); !errors.Is(err, ErrTileBatch) {
			t.Errorf("%s: %v, want ErrTileBatch", name, err)
		}
	}
}
