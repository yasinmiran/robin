//go:build linux

package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image/png"
	"io"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/robin/internal/agent/desktop"
	"github.com/yasinmiran/robin/internal/protocol"
)

// paintDesktop is a desktop whose picture the test paints.
type paintDesktop struct {
	fakeDesktop
	mu    sync.Mutex
	frame *desktop.Frame
	dirty bool
}

func (d *paintDesktop) Capture(force bool) (*desktop.Frame, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f := *d.frame
	f.Pix = slices.Clone(f.Pix)
	changed := d.dirty || force
	d.dirty = false
	return &f, changed, nil
}

func (d *paintDesktop) paint(x, y, w, h int, c byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.frame.Format == desktop.BGRX {
		paint(d.frame, x, y, w, h, c)
	}
	d.dirty = true
}

// tileBrowser is what a page sees of tiles: the manifests, the answers to
// ControlTiles, the tile batches, and the RTP timestamp of every video
// frame.
type tileBrowser struct {
	t  *testing.T
	dc *webrtc.DataChannel

	mu        sync.Mutex
	manifests []protocol.Frame
	answers   []protocol.Tiles
	frames    []uint32
	batches   chan protocol.TileBatch
}

// streamBrowser runs a stream of desk to a stand-in browser that takes
// tiles.
func streamBrowser(t *testing.T, desk desktop.Desktop) *tileBrowser {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var encodes atomic.Int64
	open := func(int, int, int, int) (Encoder, error) { return countingEncoder{&encodes}, nil }
	s := testSession(desk)
	s.stream = newStream(desk, 60, 1280, 720, open, log)
	s.stream.refresh = 50 * time.Millisecond
	fb := peerBrowser(t, s)
	b := &tileBrowser{t: t, dc: fb.dc, batches: make(chan protocol.TileBatch, 16)}
	fb.dc.OnMessage(func(m webrtc.DataChannelMessage) {
		var c protocol.Control
		if json.Unmarshal(m.Data, &c) != nil {
			return
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		switch c.T {
		case protocol.ControlFrame:
			b.manifests = append(b.manifests, *c.Frame)
		case protocol.ControlTiles:
			b.answers = append(b.answers, *c.Tiles)
		}
	})
	fb.pc.OnDataChannel(func(ch *webrtc.DataChannel) {
		if ch.Label() == protocol.BulkChannel {
			b.take(ch)
		}
	})
	fb.pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			for {
				pkt, _, err := tr.ReadRTP()
				if err != nil {
					return
				}
				if pkt.Marker {
					b.mu.Lock()
					b.frames = append(b.frames, pkt.Timestamp)
					b.mu.Unlock()
				}
			}
		}()
	})
	s.stream.setPeer(s.peer)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.stream.run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return b
}

func (b *tileBrowser) take(ch *webrtc.DataChannel) {
	var data []byte
	ch.OnMessage(func(m webrtc.DataChannelMessage) {
		if !m.IsString {
			data = append(data, m.Data...)
			return
		}
		f, _ := protocol.ParseBulk(m.Data)
		if f.T != protocol.BulkEnd {
			return
		}
		sum := sha256.Sum256(data)
		verdict := protocol.Bulk{T: protocol.BulkDone}
		batch, err := protocol.ParseTileBatch(data)
		if f.SHA256 != hex.EncodeToString(sum[:]) || err != nil {
			verdict = protocol.Bulk{T: protocol.BulkError, Error: "bad batch"}
		}
		v, _ := json.Marshal(verdict)
		ch.SendText(string(v))
		ch.Close()
		if err == nil {
			b.batches <- batch
		}
	})
}

func (b *tileBrowser) ask(on bool) {
	msg, _ := json.Marshal(protocol.Control{T: protocol.ControlTiles, On: on})
	if err := b.dc.SendText(string(msg)); err != nil {
		b.t.Fatal(err)
	}
}

// wait polls cond, under the browser's lock, for up to 15 s.
func (b *tileBrowser) wait(what string, cond func() bool) {
	b.t.Helper()
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		b.mu.Lock()
		ok := cond()
		b.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func (b *tileBrowser) batch() protocol.TileBatch {
	b.t.Helper()
	select {
	case batch := <-b.batches:
		return batch
	case <-time.After(15 * time.Second):
		b.t.Fatal("no tiles arrived")
		return protocol.TileBatch{}
	}
}

// TestStreamSendsTiles runs tiles end to end over a real peer: the page
// asks, every frame it receives has a manifest, a still screen arrives
// sharp and exact, a change is named, and asking them off stops it all.
func TestStreamSendsTiles(t *testing.T) {
	desk := &paintDesktop{frame: tileFrame(128, 64)}
	b := streamBrowser(t, desk)
	b.ask(true)
	b.wait("the answer", func() bool { return len(b.answers) > 0 })
	if a := b.answers[0]; !a.On || a.Unsupported != "" {
		t.Fatalf("answer %+v", a)
	}

	batch := b.batch()
	b.mu.Lock()
	first := b.manifests[0]
	known := map[uint32]bool{}
	for _, m := range b.manifests {
		known[m.RTP] = true
		if m.Gen != first.Gen || m.W != 128 || m.H != 64 {
			t.Errorf("manifest %+v strays from the first, %+v", m, first)
		}
	}
	b.mu.Unlock()
	if batch.Gen != first.Gen || !known[batch.RTP] || !slices.Equal(batch.Tiles, []int{0, 1}) {
		t.Fatalf("batch gen %d RTP %d tiles %v; manifests of gen %d", batch.Gen, batch.RTP, batch.Tiles, first.Gen)
	}
	img, err := png.Decode(bytes.NewReader(batch.PNG))
	if err != nil {
		t.Fatal(err)
	}
	f := tileFrame(128, 64)
	for y := range 64 {
		for x := range 128 {
			p := f.Pix[f.Stride*y+4*x:]
			r, g, bl, _ := img.At(x, y).RGBA()
			if byte(r>>8) != p[2] || byte(g>>8) != p[1] || byte(bl>>8) != p[0] {
				t.Fatalf("tile pixel %d,%d is not the desktop's", x, y)
			}
		}
	}

	desk.paint(70, 10, 3, 3, 200)
	b.wait("the change's manifest", func() bool {
		return slices.ContainsFunc(b.manifests, func(m protocol.Frame) bool { return slices.Equal(m.Gone, []int{1, 2}) })
	})

	// Every frame from the first manifest on had one, and the manifests
	// came in the frames' order.
	b.mu.Lock()
	for _, m := range b.manifests {
		known[m.RTP] = true
	}
	from := slices.Index(b.frames, first.RTP)
	if from < 0 {
		t.Fatalf("the first manifest's frame %d never arrived", first.RTP)
	}
	for _, ts := range b.frames[from:] {
		if !known[ts] {
			t.Errorf("frame %d came without a manifest", ts)
		}
	}
	for i := 1; i < len(b.manifests); i++ {
		if int32(b.manifests[i].RTP-b.manifests[i-1].RTP) <= 0 {
			t.Errorf("manifest %d of RTP %d after %d", i, b.manifests[i].RTP, b.manifests[i-1].RTP)
		}
	}
	b.mu.Unlock()

	b.ask(false)
	time.Sleep(200 * time.Millisecond)
	b.mu.Lock()
	n := len(b.manifests)
	b.mu.Unlock()
	time.Sleep(300 * time.Millisecond)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.manifests) != n {
		t.Fatalf("%d manifests after tiles were turned off", len(b.manifests)-n)
	}
}

// A desktop whose frames come already in 4:2:0 says so, and sends nothing.
func TestStreamRefusesTilesOnI420(t *testing.T) {
	f := &desktop.Frame{Width: 64, Height: 64, Stride: 64, Pix: make([]byte, 64*64*3/2), Format: desktop.I420}
	b := streamBrowser(t, &paintDesktop{frame: f})
	b.ask(true)
	b.wait("the answer", func() bool { return len(b.answers) > 0 })
	if a := b.answers[0]; a.On || a.Unsupported != errNoTiles {
		t.Fatalf("answer %+v", a)
	}
	time.Sleep(200 * time.Millisecond)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.manifests) != 0 {
		t.Fatalf("%d manifests for a desktop without tiles", len(b.manifests))
	}
}
