//go:build linux

package agent

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/yasinmiran/robin/internal/agent/desktop"
	"github.com/yasinmiran/robin/internal/protocol"
)

func init() {
	protocol.RegisterBulkKind(protocol.BulkTile, protocol.TileBatchLimit)
	handleOwner(protocol.ControlTiles, func(s *session, m protocol.Control) { s.stream.wantTiles(m.On) })
}

// tileStill is how long a tile must stay unchanged before it is sent
// sharp. A tile that changes again within tileFlap of being sent, as one
// with a blinking caret does, waits twice as long the next time, up to
// tileStill<<maxFlaps: it stays blurred rather than flicker between sharp
// and blurred on every blink.
const (
	tileStill = 300 * time.Millisecond
	tileFlap  = 2 * time.Second
	maxFlaps  = 4
)

// Tile encoding may take tileShare of one core on average, running at most
// tileBurst ahead of that; and tileBatches transfers may be under way at
// once, so a still 1920x1080 screen goes in two and a 2560x1440 one in four.
const (
	tileShare   = 0.25
	tileBurst   = 100 * time.Millisecond
	tileBatches = 2
)

// noTilesReason is what the page shows for a desktop that gets no tiles:
// Wayland's portal hands the agent its frames already in 4:2:0, the very
// loss tiles undo.
const noTilesReason = "Not available on a Wayland desktop: its frames reach Robin with their colour detail already gone. An X11 desktop has it."

// tileSender is where tiles go: the peer, or a test's stand-in.
type tileSender interface {
	SendBulk(ctx context.Context, h protocol.Bulk, r io.Reader) error
}

// tileTracker finds the tiles of the screen that stay still and sends them
// losslessly, and tells the page which tiles each frame changes. It belongs
// to the stream's goroutine: only the batches it starts run elsewhere, and
// they report back on done.
type tileTracker struct {
	log    *slog.Logger
	budget cpuBudget

	// gen names the grid; any change of desktop size, peer or switch starts
	// a new one, and the page drops every tile of the old.
	gen        uint32
	w, h, cols int
	prev       []byte // the latest frame, 4*w bytes a row
	slots      []tileSlot
	gone       []int // live tiles changed since the last manifest

	// seq counts the frames update has seen; written is the seq of the
	// latest frame written to the page, and rtp its timestamp. A tile is
	// sent only with pixels some written frame shows.
	seq, written uint64
	rtp          uint32

	batches map[*tileBatch]struct{}
	done    chan tileResult
	stop    chan struct{}
	// running counts the batches' goroutines, so close outlives them all.
	running sync.WaitGroup
}

// tileSlot is the state of one tile of the grid.
type tileSlot struct {
	changedSeq uint64
	changedAt  time.Time
	// live says the page may hold this tile, so a change must be named in
	// the next manifest. It stays set after a failed transfer: the page may
	// have kept what the agent never heard it kept.
	live bool
	// sent says a batch carrying this tile arrived since its last change.
	sent  bool
	batch *tileBatch // the transfer under way that carries it
	// cutAt is when the tile was last sent, and flaps how many times in a
	// row it changed soon after.
	cutAt time.Time
	flaps int
}

// tileBatch is one transfer of tiles under way.
type tileBatch struct {
	gen    uint32
	tiles  []int
	left   int // tiles of it that are still current
	cancel context.CancelFunc
}

type tileResult struct {
	batch  *tileBatch
	encode time.Duration
	err    error
}

func newTileTracker(log *slog.Logger) *tileTracker {
	return &tileTracker{
		log:     log,
		budget:  cpuBudget{share: tileShare, burst: tileBurst},
		batches: map[*tileBatch]struct{}{},
		done:    make(chan tileResult, tileBatches),
		stop:    make(chan struct{}),
	}
}

// close cancels every transfer for good and returns once none is left
// running, which waits at most for the atlases being encoded: an encode
// cannot be cancelled.
func (t *tileTracker) close() {
	t.reset()
	close(t.stop)
	t.running.Wait()
}

// reset forgets the grid and cancels every transfer, so that whatever
// comes next is a new generation.
func (t *tileTracker) reset() {
	for b := range t.batches {
		b.cancel()
	}
	clear(t.batches)
	t.gen++
	t.w, t.h, t.cols = 0, 0, 0
	t.slots, t.gone = nil, nil
	t.written = 0
}

// update compares f, a BGRX frame at desktop size, with the frame before
// and marks the tiles it changes. A tile the page may hold is named in the
// next manifest; its transfer, if under way, is cancelled once none of the
// batch it rides in is current.
func (t *tileTracker) update(f *desktop.Frame, now time.Time) {
	t.seq++
	if f.Width != t.w || f.Height != t.h {
		t.reset()
		t.w, t.h, t.cols = f.Width, f.Height, protocol.TileColumns(f.Width)
		rows := (f.Height + protocol.TileSize - 1) / protocol.TileSize
		t.prev = make([]byte, 4*t.w*t.h)
		t.slots = make([]tileSlot, t.cols*rows)
		for y := range t.h {
			copy(t.prev[4*t.w*y:4*t.w*(y+1)], f.Pix[f.Stride*y:])
		}
		for i := range t.slots {
			t.slots[i] = tileSlot{changedSeq: t.seq, changedAt: now}
		}
		return
	}
	for i := range t.slots {
		x, y, w, h := t.rect(i)
		for r := y; r < y+h; r++ {
			cur := f.Pix[f.Stride*r+4*x : f.Stride*r+4*(x+w)]
			old := t.prev[4*(t.w*r+x) : 4*(t.w*r+x+w)]
			if bytes.Equal(cur, old) {
				continue
			}
			// Rows above r are the same; copy the rest.
			for ; r < y+h; r++ {
				copy(t.prev[4*(t.w*r+x):4*(t.w*r+x+w)], f.Pix[f.Stride*r+4*x:])
			}
			t.changed(i, now)
			break
		}
	}
}

func (t *tileTracker) changed(i int, now time.Time) {
	s := &t.slots[i]
	if s.live {
		t.gone = append(t.gone, i)
	}
	if b := s.batch; b != nil {
		if b.left--; b.left == 0 {
			b.cancel()
		}
	}
	flaps := s.flaps
	if s.live {
		flaps = 0
		if now.Sub(s.cutAt) < tileFlap {
			flaps = min(s.flaps+1, maxFlaps)
		}
	}
	*s = tileSlot{changedSeq: t.seq, changedAt: now, flaps: flaps}
}

// rect is tile i's place in the desktop.
func (t *tileTracker) rect(i int) (x, y, w, h int) {
	x, y = i%t.cols*protocol.TileSize, i/t.cols*protocol.TileSize
	return x, y, min(protocol.TileSize, t.w-x), min(protocol.TileSize, t.h-y)
}

// manifest is the ControlFrame of the frame about to be written with RTP
// timestamp rtp: the tiles changed since the previous one.
func (t *tileTracker) manifest(rtp uint32) protocol.Control {
	m := protocol.Control{T: protocol.ControlFrame, Frame: &protocol.Frame{RTP: rtp, Gen: t.gen, W: t.w, H: t.h, Gone: ranges(t.gone)}}
	t.gone = t.gone[:0]
	return m
}

// wrote records that the frame update saw last went out with timestamp rtp.
func (t *tileTracker) wrote(rtp uint32) {
	t.written, t.rtp = t.seq, rtp
}

// ranges turns tile indexes into the half-open ranges of Frame.Gone.
func ranges(idx []int) []int {
	if len(idx) == 0 {
		return nil
	}
	sorted := slices.Sorted(slices.Values(idx))
	var out []int
	for _, i := range sorted {
		if n := len(out); n > 0 && out[n-1] >= i {
			out[n-1] = max(out[n-1], i+1)
			continue
		}
		out = append(out, i, i+1)
	}
	return out
}

// cut sends the tiles that have been still long enough, as far as the
// transfers under way and the CPU budget allow, and takes the reports of
// the transfers that ended.
func (t *tileTracker) cut(to tileSender, now time.Time) {
	t.collect(now)
	if t.written == 0 || len(t.batches) >= tileBatches || !t.budget.ok(now) {
		return
	}
	var pick []int
	for i := range t.slots {
		s := &t.slots[i]
		if s.sent || s.batch != nil || s.changedSeq > t.written || now.Sub(s.changedAt) < tileStill<<s.flaps {
			continue
		}
		if pick = append(pick, i); len(pick) == protocol.MaxTileBatch {
			break
		}
	}
	if len(pick) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &tileBatch{gen: t.gen, tiles: pick, left: len(pick), cancel: cancel}
	for _, i := range pick {
		s := &t.slots[i]
		s.live, s.batch, s.cutAt = true, b, now
	}
	t.batches[b] = struct{}{}
	atlas := t.atlas(pick)
	batch := protocol.TileBatch{RTP: t.rtp, Gen: t.gen, Tiles: pick}
	t.running.Go(func() {
		r := tileResult{batch: b, err: ctx.Err()}
		if r.err == nil {
			began := time.Now()
			batch.PNG, r.err = encodeAtlas(atlas)
			r.encode = time.Since(began)
		}
		if r.err == nil {
			payload := batch.Marshal()
			r.err = to.SendBulk(ctx, protocol.Bulk{Kind: protocol.BulkTile, Size: int64(len(payload))}, bytes.NewReader(payload))
		}
		cancel()
		select {
		case t.done <- r:
		case <-t.stop:
		}
	})
}

// collect takes the reports of transfers that ended.
func (t *tileTracker) collect(now time.Time) {
	for {
		select {
		case r := <-t.done:
			t.ended(r, now)
		default:
			return
		}
	}
}

func (t *tileTracker) ended(r tileResult, now time.Time) {
	t.budget.spend(r.encode, now)
	b := r.batch
	if _, ok := t.batches[b]; !ok || b.gen != t.gen {
		return
	}
	delete(t.batches, b)
	if r.err != nil && !errors.Is(r.err, context.Canceled) {
		t.log.Debug("tiles not sent", "tiles", len(b.tiles), "err", r.err)
	}
	for _, i := range b.tiles {
		s := &t.slots[i]
		if s.batch != b {
			continue
		}
		s.batch = nil
		if r.err == nil {
			s.sent = true
		} else {
			// Wait another still spell before trying again.
			s.changedAt = now
		}
	}
}

// atlas copies the tiles of pick out of the latest frame into an atlas of
// TileAtlasColumns tiles a row, still BGRX.
func (t *tileTracker) atlas(pick []int) *image.RGBA {
	const ts = protocol.TileSize
	w, h := protocol.TileAtlasSize(len(pick))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for k, i := range pick {
		x, y, w, h := t.rect(i)
		ax, ay := k%protocol.TileAtlasColumns*ts, k/protocol.TileAtlasColumns*ts
		for r := range h {
			copy(img.Pix[img.Stride*(ay+r)+4*ax:], t.prev[4*(t.w*(y+r)+x):4*(t.w*(y+r)+x+w)])
		}
	}
	return img
}

// tilePNG is shared by every batch: fastest compression, since a tile
// costs far more to encode well than the bytes it saves on the way.
var tilePNG = png.Encoder{CompressionLevel: png.BestSpeed}

// encodeAtlas turns the BGRX atlas into RGBA, opaque, and compresses it.
func encodeAtlas(img *image.RGBA) ([]byte, error) {
	p := img.Pix
	for i := 0; i < len(p); i += 4 {
		p[i], p[i+2], p[i+3] = p[i+2], p[i], 0xff
	}
	var buf bytes.Buffer
	if err := tilePNG.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// cpuBudget lets work run at share of one core on average and at most
// burst ahead of that.
type cpuBudget struct {
	share float64
	burst time.Duration
	debt  time.Duration
	at    time.Time
}

func (b *cpuBudget) settle(now time.Time) {
	if !now.After(b.at) {
		return
	}
	if !b.at.IsZero() {
		b.debt -= time.Duration(float64(now.Sub(b.at)) * b.share)
	}
	b.debt = max(b.debt, -b.burst)
	b.at = now
}

// ok reports whether more work may start.
func (b *cpuBudget) ok(now time.Time) bool {
	b.settle(now)
	return b.debt <= 0
}

// spend charges work that took d.
func (b *cpuBudget) spend(d time.Duration, now time.Time) {
	b.settle(now)
	b.debt += d
}
