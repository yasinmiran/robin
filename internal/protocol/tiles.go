package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Sharp text: lossless tiles over the parts of the screen that stay still.
//
// The browser sends ControlTiles with Control.On to ask for tiles or to stop
// them, and the agent answers with ControlTiles carrying Tiles. While tiles
// are on, every video frame the agent writes is preceded on the control
// channel by a ControlFrame carrying its Frame, the frame's manifest: the
// tiles it changes. Still tiles travel as BulkTile transfers, each a
// TileBatch.
//
// A tile may be shown over a frame only once the manifests of every frame
// from the one the tile shows up to the one on screen have arrived and none
// of them named it. A page that shows a frame whose manifest it does not
// have yet hides every tile until it does, so an invalidation held up
// behind a lost packet costs blur, never a stale tile.
const (
	ControlTiles ControlType = "tiles"
	ControlFrame ControlType = "frame"
)

// BulkTile is the bulk kind that carries a TileBatch from agent to browser.
const BulkTile = "tile"

// TileSize is the side of a tile in desktop pixels. Tiles on the right and
// bottom edges are cut short by the desktop's size.
const TileSize = 64

// MaxTileBatch is the most tiles one transfer carries, and TileAtlasColumns
// how many of them each row of its atlas holds.
const (
	MaxTileBatch     = 256
	TileAtlasColumns = 16
)

// TileBatchLimit bounds a BulkTile transfer: the index, and an atlas of
// MaxTileBatch tiles at three bytes a pixel with room for PNG's framing.
const TileBatchLimit = 4 << 20

// Tiles is the agent's answer to ControlTiles.
type Tiles struct {
	On bool `json:"on,omitempty"`
	// Unsupported says why this desktop cannot have tiles at all.
	Unsupported string `json:"unsupported,omitempty"`
}

// Frame is the manifest of one video frame.
type Frame struct {
	// RTP is the RTP timestamp of the frame's packets.
	RTP uint32 `json:"rtp"`
	// Gen names the tile grid, W by H desktop pixels in TileSize squares.
	// A manifest with a new Gen drops every tile of the old one.
	Gen uint32 `json:"gen"`
	W   int    `json:"w"`
	H   int    `json:"h"`
	// Gone lists the tiles this frame changes as half-open ranges of tile
	// indexes, start and end in turn. Tile (column, row) has index
	// row*TileColumns(W)+column.
	Gone []int `json:"gone,omitempty"`
}

// TileColumns is how many tiles a row of a grid w pixels wide holds.
func TileColumns(w int) int { return (w + TileSize - 1) / TileSize }

// TileBatch is the payload of a BulkTile transfer.
type TileBatch struct {
	// RTP is the timestamp of the frame whose pixels the tiles show, and
	// Gen the grid they belong to.
	RTP, Gen uint32
	// Tiles indexes the grid, in the order the atlas holds them.
	Tiles []int
	// PNG is the atlas: TileAtlasColumns tiles a row, Tiles[k] in column
	// k%TileAtlasColumns of row k/TileAtlasColumns, each TileSize square
	// with an edge tile's pixels at its top left.
	PNG []byte
}

// tileBatchHead is the fixed part of a marshalled TileBatch: RTP, Gen and
// the tile count.
const tileBatchHead = 4 + 4 + 2

// ErrTileBatch means a TileBatch payload is malformed.
var ErrTileBatch = errors.New("protocol: malformed tile batch")

// Marshal lays b out in network byte order: RTP, Gen, the count of tiles
// in two bytes, each tile's index in four, then the PNG.
func (b TileBatch) Marshal() []byte {
	out := make([]byte, tileBatchHead+4*len(b.Tiles), tileBatchHead+4*len(b.Tiles)+len(b.PNG))
	binary.BigEndian.PutUint32(out, b.RTP)
	binary.BigEndian.PutUint32(out[4:], b.Gen)
	binary.BigEndian.PutUint16(out[8:], uint16(len(b.Tiles)))
	for i, t := range b.Tiles {
		binary.BigEndian.PutUint32(out[tileBatchHead+4*i:], uint32(t))
	}
	return append(out, b.PNG...)
}

// ParseTileBatch reads what Marshal wrote, as the browser does. It refuses
// an empty batch, one of more than MaxTileBatch tiles, and one cut short.
func ParseTileBatch(p []byte) (TileBatch, error) {
	if len(p) < tileBatchHead {
		return TileBatch{}, fmt.Errorf("%w: %d bytes", ErrTileBatch, len(p))
	}
	n := int(binary.BigEndian.Uint16(p[8:]))
	if n == 0 || n > MaxTileBatch {
		return TileBatch{}, fmt.Errorf("%w: %d tiles", ErrTileBatch, n)
	}
	if len(p) < tileBatchHead+4*n {
		return TileBatch{}, fmt.Errorf("%w: index of %d tiles cut short", ErrTileBatch, n)
	}
	b := TileBatch{RTP: binary.BigEndian.Uint32(p), Gen: binary.BigEndian.Uint32(p[4:]), Tiles: make([]int, n)}
	for i := range b.Tiles {
		b.Tiles[i] = int(binary.BigEndian.Uint32(p[tileBatchHead+4*i:]))
	}
	b.PNG = p[tileBatchHead+4*n:]
	return b, nil
}
