//go:build linux

package agent

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agent/desktop"
	"github.com/yasinmiran/robin/internal/agent/vqtest"
	"github.com/yasinmiran/robin/internal/protocol"
)

// tileFrame is a BGRX desktop whose rows are stride bytes apart, wider than
// the picture so the tracker cannot lean on Stride == 4*Width.
func tileFrame(w, h int) *desktop.Frame {
	stride := 4*w + 12
	f := &desktop.Frame{Width: w, Height: h, Stride: stride, Pix: make([]byte, stride*h), Format: desktop.BGRX}
	for y := range h {
		for x := range w {
			p := f.Pix[stride*y+4*x:]
			p[0], p[1], p[2] = byte(x), byte(y), byte(x^y)
		}
	}
	return f
}

// paint fills a rectangle of f with one colour.
func paint(f *desktop.Frame, x0, y0, w, h int, c byte) {
	for y := y0; y < y0+h; y++ {
		for x := x0; x < x0+w; x++ {
			p := f.Pix[f.Stride*y+4*x:]
			p[0], p[1], p[2] = c, c+1, c+2
		}
	}
}

// tileSink stands in for the peer. SendBulk waits on hold, when set,
// until it is closed or the transfer cancelled.
type tileSink struct {
	mu        sync.Mutex
	batches   []protocol.TileBatch
	cancelled int
	hold      chan struct{}
	started   chan struct{} // told of each transfer that reaches hold
	fail      error
}

// holding makes a sink that holds every transfer and says when one
// reaches it.
func holding() *tileSink {
	return &tileSink{hold: make(chan struct{}), started: make(chan struct{}, 16)}
}

func (s *tileSink) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-s.started:
	case <-time.After(10 * time.Second):
		t.Fatal("no transfer reached the sink")
	}
}

func (s *tileSink) SendBulk(ctx context.Context, h protocol.Bulk, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if h.Kind != protocol.BulkTile || h.Size != int64(len(data)) {
		return errors.New("bad header")
	}
	if lim, _ := protocol.BulkLimit(h.Kind); h.Size > lim {
		return errors.New("over the limit")
	}
	s.mu.Lock()
	hold := s.hold
	s.mu.Unlock()
	if hold != nil {
		if s.started != nil {
			s.started <- struct{}{}
		}
		select {
		case <-hold:
		case <-ctx.Done():
			s.mu.Lock()
			s.cancelled++
			s.mu.Unlock()
			return ctx.Err()
		}
	}
	b, err := protocol.ParseTileBatch(data)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.batches = append(s.batches, b)
	return nil
}

func (s *tileSink) sent() []protocol.TileBatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.batches)
}

func testTiles() *tiles {
	t := newTiles(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.budget = cpuBudget{share: 1, burst: time.Hour}
	return t
}

// settle waits for every transfer under way to report.
func settle(tb testing.TB, t *tiles, now time.Time) {
	tb.Helper()
	for deadline := time.Now().Add(10 * time.Second); len(t.batches) > 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			tb.Fatalf("%d tile transfers never ended", len(t.batches))
		}
		t.collect(now)
	}
}

// frame runs one frame through tr the way the stream does: update, the
// manifest, the write, then a cut.
func frame(tr *tiles, to tileSender, f *desktop.Frame, rtp uint32, now time.Time) protocol.Frame {
	tr.update(f, now)
	m := tr.manifest(rtp)
	tr.wrote(rtp)
	tr.cut(to, now)
	return *m.Frame
}

func allTiles(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func sentTiles(bs []protocol.TileBatch) []int {
	var out []int
	for _, b := range bs {
		out = append(out, b.Tiles...)
	}
	slices.Sort(out)
	return out
}

// A tile goes out only once it has been still for tileStill, and then once.
func TestTilesWaitForStillness(t *testing.T) {
	tr, sink := testTiles(), &tileSink{}
	f := tileFrame(130, 70) // 3x2 tiles, the last column and row cut short
	t0 := time.Unix(1000, 0)
	m := frame(tr, sink, f, 10, t0)
	if m.W != 130 || m.H != 70 || m.Gen == 0 || m.RTP != 10 || m.Gone != nil {
		t.Fatalf("first manifest %+v", m)
	}
	tr.cut(sink, t0.Add(tileStill-time.Millisecond))
	settle(t, tr, t0)
	if got := sink.sent(); len(got) != 0 {
		t.Fatalf("tiles sent before they were still: %v", sentTiles(got))
	}
	tr.cut(sink, t0.Add(tileStill))
	settle(t, tr, t0.Add(tileStill))
	got := sink.sent()
	if len(got) != 1 || !slices.Equal(got[0].Tiles, allTiles(6)) || got[0].RTP != 10 || got[0].Gen != m.Gen {
		t.Fatalf("still screen sent as %+v, want one batch of all six tiles at RTP 10", got)
	}
	tr.cut(sink, t0.Add(time.Hour))
	settle(t, tr, t0.Add(time.Hour))
	if n := len(sink.sent()); n != 1 {
		t.Fatalf("%d batches for a screen that never changed", n)
	}
}

// The atlas holds each tile's own pixels, edge tiles at the top left of
// their square, decoded exactly as they were on the desktop.
func TestTileAtlasIsLossless(t *testing.T) {
	tr, sink := testTiles(), &tileSink{}
	f := tileFrame(64*17+5, 64+9) // 18 columns: the atlas wraps to a second row
	t0 := time.Unix(1000, 0)
	frame(tr, sink, f, 1, t0)
	tr.cut(sink, t0.Add(tileStill))
	settle(t, tr, t0)
	got := sink.sent()
	if len(got) != 1 || len(got[0].Tiles) != 36 {
		t.Fatalf("batches %v", got)
	}
	img, err := png.Decode(bytes.NewReader(got[0].PNG))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 16*64 || b.Dy() != 3*64 {
		t.Fatalf("atlas is %v, want 1024x192", b)
	}
	for k, i := range got[0].Tiles {
		x, y, w, h := tr.rect(i)
		ax, ay := k%16*64, k/16*64
		for r := range h {
			for c := range w {
				p := f.Pix[f.Stride*(y+r)+4*(x+c):]
				cr, cg, cb, ca := img.At(ax+c, ay+r).RGBA()
				if byte(cr>>8) != p[2] || byte(cg>>8) != p[1] || byte(cb>>8) != p[0] || ca != 0xffff {
					t.Fatalf("tile %d pixel %d,%d is %d,%d,%d,%d, want %d,%d,%d opaque", i, c, r, cr>>8, cg>>8, cb>>8, ca>>8, p[2], p[1], p[0])
				}
			}
		}
	}
}

// A change to a tile the page may hold is named in the very next manifest,
// whatever else that frame changed, and only then; tiles the page never had
// are not named at all.
func TestTilesManifestNamesWhatChanged(t *testing.T) {
	tr, sink := testTiles(), &tileSink{}
	f := tileFrame(64*4, 64*2)
	t0 := time.Unix(1000, 0)
	gen := frame(tr, sink, f, 100, t0).Gen
	tr.cut(sink, t0.Add(tileStill))
	settle(t, tr, t0)

	paint(f, 70, 3, 2, 2, 9)     // tile 1
	paint(f, 64*3, 64, 64, 1, 9) // tile 7, its first row only
	paint(f, 0, 127, 1, 1, 9)    // tile 4, its last pixel row
	m := frame(tr, sink, f, 200, t0.Add(time.Second))
	if m.Gen != gen || m.RTP != 200 || !slices.Equal(m.Gone, []int{1, 2, 4, 5, 7, 8}) {
		t.Fatalf("manifest %+v, want tiles 1, 4 and 7 gone in gen %d", m, gen)
	}
	paint(f, 70, 3, 2, 2, 10)
	if m := frame(tr, sink, f, 300, t0.Add(2*time.Second)); m.Gone != nil {
		t.Fatalf("a tile the page no longer has was named again: %+v", m)
	}
	if m := frame(tr, sink, f, 400, t0.Add(3*time.Second)); m.Gone != nil {
		t.Fatalf("an unchanged frame named %v", m.Gone)
	}
}

// A tile that changed in a frame the encoder dropped waits for a frame that
// shows it: tiles carry only pixels a written frame has.
func TestTilesWaitForAWrittenFrame(t *testing.T) {
	tr, sink := testTiles(), &tileSink{}
	f := tileFrame(64*2, 64)
	t0 := time.Unix(1000, 0)
	tr.update(f, t0)
	tr.cut(sink, t0.Add(time.Hour))
	settle(t, tr, t0)
	if len(sink.sent()) != 0 {
		t.Fatal("tiles sent before any frame was written")
	}
	frame(tr, sink, f, 5, t0)
	paint(f, 64, 0, 4, 4, 1)
	tr.update(f, t0.Add(time.Second)) // dropped by the encoder: no manifest, no write
	tr.cut(sink, t0.Add(time.Hour))
	settle(t, tr, t0)
	if got := sentTiles(sink.sent()); !slices.Equal(got, []int{0}) {
		t.Fatalf("sent %v, want only tile 0, which the written frame shows", got)
	}
	m := frame(tr, sink, f, 9, t0.Add(time.Hour))
	tr.cut(sink, t0.Add(2*time.Hour))
	settle(t, tr, t0)
	if m.Gone != nil {
		t.Fatalf("manifest %+v named a tile that was never sent", m)
	}
	got := sink.sent()
	if len(got) != 2 || !slices.Equal(got[1].Tiles, []int{1}) || got[1].RTP != 9 {
		t.Fatalf("second batch %+v, want tile 1 at RTP 9", got)
	}
}

// A still screen goes in a few transfers of at most MaxTileBatch tiles, no
// more than tileBatches at once.
func TestTilesBatch(t *testing.T) {
	tr, sink := testTiles(), &tileSink{hold: make(chan struct{})}
	f := tileFrame(1920, 1080) // 30x17 tiles
	t0 := time.Unix(1000, 0)
	frame(tr, sink, f, 1, t0)
	for i := range 5 {
		tr.cut(sink, t0.Add(tileStill+time.Duration(i)*time.Millisecond))
	}
	if n := len(tr.batches); n != tileBatches {
		t.Fatalf("%d transfers under way, want %d", n, tileBatches)
	}
	close(sink.hold)
	settle(t, tr, t0)
	tr.cut(sink, t0.Add(time.Second))
	settle(t, tr, t0)
	got := sink.sent()
	if len(got) != 2 || max(len(got[0].Tiles), len(got[1].Tiles)) != protocol.MaxTileBatch || !slices.Equal(sentTiles(got), allTiles(510)) {
		t.Fatalf("%d batches carrying %d tiles, want 2 carrying all 510", len(got), len(sentTiles(got)))
	}
}

// A transfer is cancelled once every tile in it changed, and goes on while
// some are still current: the page drops the others by the manifest.
func TestTilesCancelStaleTransfers(t *testing.T) {
	tr, sink := testTiles(), holding()
	f := tileFrame(64*3, 64)
	t0 := time.Unix(1000, 0)
	frame(tr, sink, f, 1, t0)
	tr.cut(sink, t0.Add(tileStill))
	sink.waitStarted(t)
	var b *tileBatch
	for b = range tr.batches {
	}
	if len(tr.batches) != 1 || b.left != 3 {
		t.Fatalf("%d batches, the first with %d current", len(tr.batches), b.left)
	}
	// Frames the way the stream runs them, without the cut that would send
	// the changed tiles again.
	step := func(rtp uint32, now time.Time) protocol.Frame {
		tr.update(f, now)
		m := tr.manifest(rtp)
		tr.wrote(rtp)
		return *m.Frame
	}
	paint(f, 0, 0, 1, 1, 7)
	paint(f, 64, 0, 1, 1, 7)
	if m := step(2, t0.Add(time.Second)); !slices.Equal(m.Gone, []int{0, 2}) || b.left != 1 {
		t.Fatalf("manifest %+v with %d current", m, b.left)
	}
	if sink.cancelled != 0 {
		t.Fatal("a transfer with a current tile was cancelled")
	}
	paint(f, 128, 0, 1, 1, 7)
	if m := step(3, t0.Add(2*time.Second)); !slices.Equal(m.Gone, []int{2, 3}) {
		t.Fatalf("manifest %+v", m)
	}
	settle(t, tr, t0.Add(2*time.Second))
	if sink.cancelled != 1 {
		t.Fatalf("%d transfers cancelled, want the one whose tiles all changed", sink.cancelled)
	}
	// Each tile is sent again once still, the page having none of them.
	close(sink.hold)
	tr.cut(sink, t0.Add(3*time.Second))
	settle(t, tr, t0.Add(3*time.Second))
	if got := sink.sent(); len(got) != 1 || !slices.Equal(got[0].Tiles, allTiles(3)) || got[0].RTP != 3 {
		t.Fatalf("resent as %+v", got)
	}
}

// A transfer that failed may still have reached the page, so a change to
// its tiles is named all the same; they are tried again after another
// still spell.
func TestTilesAfterAFailedTransfer(t *testing.T) {
	tr, sink := testTiles(), &tileSink{fail: errors.New("refused")}
	f := tileFrame(64, 64)
	t0 := time.Unix(1000, 0)
	frame(tr, sink, f, 1, t0)
	tr.cut(sink, t0.Add(tileStill))
	settle(t, tr, t0.Add(tileStill))
	sink.fail = nil
	tr.cut(sink, t0.Add(2*tileStill-time.Millisecond))
	settle(t, tr, t0)
	if len(sink.sent()) != 0 {
		t.Fatal("tried again before another still spell")
	}
	tr.cut(sink, t0.Add(2*tileStill))
	settle(t, tr, t0)
	if len(sink.sent()) != 1 {
		t.Fatal("never tried again")
	}

	tr2, sink2 := testTiles(), &tileSink{fail: errors.New("closed")}
	frame(tr2, sink2, f, 1, t0)
	tr2.cut(sink2, t0.Add(tileStill))
	settle(t, tr2, t0)
	paint(f, 0, 0, 1, 1, 3)
	if m := frame(tr2, sink2, f, 2, t0.Add(time.Second)); !slices.Equal(m.Gone, []int{0, 1}) {
		t.Fatalf("a change after a failed transfer gave %+v", m)
	}
}

// A tile that keeps changing soon after it was sent, like one with a
// blinking caret, waits longer each time and stops flickering between
// sharp and blurred; once it settles for good it is quick again.
func TestTilesBackOffFromABlinkingCaret(t *testing.T) {
	tr, sink := testTiles(), &tileSink{}
	f := tileFrame(64, 64)
	at := time.Unix(1000, 0)
	frame(tr, sink, f, 1, at)
	const blink = 530 * time.Millisecond
	cuts := 0
	for i := range 40 {
		on := byte(i % 2)
		for t10 := time.Duration(0); t10 < blink; t10 += 10 * time.Millisecond {
			tr.cut(sink, at.Add(t10))
			settle(t, tr, at.Add(t10))
		}
		at = at.Add(blink)
		paint(f, 3, 3, 1, 10, on)
		if m := frame(tr, sink, f, uint32(i+2), at); m.Gone != nil {
			cuts++
		}
	}
	if cuts > 3 {
		t.Fatalf("the caret's tile flickered %d times in %d blinks", cuts, 40)
	}
	quiet := at.Add(tileStill << maxFlaps)
	tr.cut(sink, quiet)
	settle(t, tr, quiet)
	n := len(sink.sent())
	quiet = quiet.Add(tileFlap)
	paint(f, 3, 3, 1, 10, 2)
	frame(tr, sink, f, 99, quiet)
	tr.cut(sink, quiet.Add(tileStill))
	settle(t, tr, quiet)
	if len(sink.sent()) != n+1 {
		t.Fatal("a tile that settled stayed slow")
	}
}

// A new desktop size is a new grid: every transfer is cancelled and the
// next manifest names a new generation.
func TestTilesNewGridOnResize(t *testing.T) {
	tr, sink := testTiles(), holding()
	t0 := time.Unix(1000, 0)
	gen := frame(tr, sink, tileFrame(128, 64), 1, t0).Gen
	tr.cut(sink, t0.Add(tileStill))
	sink.waitStarted(t)
	m := frame(tr, sink, tileFrame(64, 64), 2, t0.Add(time.Second))
	if m.Gen == gen || m.W != 64 || m.Gone != nil {
		t.Fatalf("manifest after a resize %+v, old gen %d", m, gen)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		sink.mu.Lock()
		n := sink.cancelled
		sink.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d transfers cancelled by the resize, want 1", n)
		}
	}
	tr.close()
}

func mustBenchClip(b testing.TB) *vqtest.Clip {
	clip, err := vqtest.NewClip(vqtest.Captured, 2560, 1440, vqtest.Segment{Kind: vqtest.Still, Frames: 1})
	if err != nil {
		b.Fatal(err)
	}
	return clip
}

func TestRanges(t *testing.T) {
	for _, c := range []struct{ in, want []int }{
		{nil, nil},
		{[]int{3}, []int{3, 4}},
		{[]int{5, 1, 2, 3, 9, 8}, []int{1, 4, 5, 6, 8, 10}},
		{[]int{2, 2}, []int{2, 3}},
	} {
		if got := ranges(c.in); !slices.Equal(got, c.want) {
			t.Errorf("ranges(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCPUBudget(t *testing.T) {
	b := cpuBudget{share: 0.25, burst: 100 * time.Millisecond}
	t0 := time.Unix(1000, 0)
	if !b.ok(t0) {
		t.Fatal("a fresh budget refuses")
	}
	b.spend(200*time.Millisecond, t0)
	if b.ok(t0.Add(799 * time.Millisecond)) {
		t.Fatal("200 ms of work at a quarter of a core was paid off in under 800 ms")
	}
	if !b.ok(t0.Add(800 * time.Millisecond)) {
		t.Fatal("200 ms of work at a quarter of a core still owed after 800 ms")
	}
	// An idle hour banks only the burst.
	later := t0.Add(time.Hour)
	b.ok(later)
	b.spend(150*time.Millisecond, later)
	if b.ok(later.Add(199 * time.Millisecond)) {
		t.Fatal("more than the burst was banked")
	}
	if !b.ok(later.Add(200 * time.Millisecond)) {
		t.Fatal("the burst was not banked")
	}
}

// The budget holds back the next batch until the last one's encode is paid
// for.
func TestTilesKeepToTheBudget(t *testing.T) {
	tr, sink := testTiles(), &tileSink{}
	tr.budget = cpuBudget{share: tileShare}
	f := tileFrame(64*2, 64)
	t0 := time.Unix(1000, 0)
	frame(tr, sink, f, 1, t0)
	tr.budget.spend(time.Second, t0)
	tr.cut(sink, t0.Add(tileStill))
	settle(t, tr, t0)
	if len(sink.sent()) != 0 {
		t.Fatal("a batch started on an overdrawn budget")
	}
	tr.cut(sink, t0.Add(4*time.Second))
	settle(t, tr, t0)
	if len(sink.sent()) != 1 {
		t.Fatal("no batch once the budget was paid off")
	}
}

// BenchmarkTiles measures the two costs of sharp text on a real screen of
// text: comparing a frame on the capture path, and encoding a still
// 2560x1440 screen's tiles. It reports the bytes that screen takes.
func BenchmarkTiles(b *testing.B) {
	clip := mustBenchClip(b)
	f := &desktop.Frame{Width: clip.W, Height: clip.H, Stride: 4 * clip.W, Pix: make([]byte, 4*clip.W*clip.H), Format: desktop.BGRX}
	clip.Draw(f.Pix, 0)
	b.Run("compare", func(b *testing.B) {
		tr := testTiles()
		tr.update(f, time.Now())
		b.SetBytes(int64(len(f.Pix)))
		for b.Loop() {
			tr.update(f, time.Now())
		}
	})
	b.Run("encode", func(b *testing.B) {
		tr := testTiles()
		tr.update(f, time.Now())
		all := allTiles(len(tr.slots))
		var size int
		for b.Loop() {
			size = 0
			for k := 0; k < len(all); k += protocol.MaxTileBatch {
				p, err := encodeAtlas(tr.atlas(all[k:min(k+protocol.MaxTileBatch, len(all))]))
				if err != nil {
					b.Fatal(err)
				}
				size += len(p)
			}
		}
		b.ReportMetric(float64(size), "bytes/screen")
		b.ReportMetric(float64(b.Elapsed().Microseconds())/float64(b.N)/float64(len(all)), "µs/tile")
	})
}
