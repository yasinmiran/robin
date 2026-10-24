//go:build linux

package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agent/desktop"
	"github.com/yasinmiran/robin/internal/agent/h264"
	"github.com/yasinmiran/robin/internal/agent/peer"
	"github.com/yasinmiran/robin/internal/agent/vqtest"
	"github.com/yasinmiran/robin/internal/agent/yuv"
	"github.com/yasinmiran/robin/internal/protocol"
)

// clipDesktop plays a vqtest clip as a desktop: until begin it shows a
// blank screen, then frame i of the clip from i/fps after begin.
type clipDesktop struct {
	clip *vqtest.Clip
	fps  int

	mu    sync.Mutex
	start time.Time
	pix   []byte
	drawn int // clip frame in pix; -1 for the blank screen
	// index is the clip frame the latest capture returned, -1 before
	// begin; the tagging encoder reads it.
	index atomic.Int64
}

func newClipDesktop(clip *vqtest.Clip, fps int) *clipDesktop {
	d := &clipDesktop{clip: clip, fps: fps, pix: make([]byte, 4*clip.W*clip.H), drawn: -1}
	for i := range d.pix {
		d.pix[i] = 128
	}
	d.index.Store(-1)
	return d
}

// begin starts the clip and returns when its frame 0 is captured.
func (d *clipDesktop) begin() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.start = time.Now()
	return d.start
}

func (d *clipDesktop) Capture(force bool) (*desktop.Frame, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f := &desktop.Frame{Width: d.clip.W, Height: d.clip.H, Stride: 4 * d.clip.W, Pix: d.pix, Format: desktop.BGRX}
	if d.start.IsZero() {
		return f, force, nil
	}
	i := min(int(time.Since(d.start)*time.Duration(d.fps)/time.Second), d.clip.Len()-1)
	changed := false
	for j := d.drawn + 1; j <= i; j++ {
		changed = changed || j == 0 || d.clip.Changed(j)
	}
	if i != d.drawn {
		d.clip.Draw(d.pix, i)
		d.drawn = i
	}
	d.index.Store(int64(i))
	return f, changed || force, nil
}

func (d *clipDesktop) Size() (int, int)                  { return d.clip.W, d.clip.H }
func (d *clipDesktop) Resize(int, int) (int, int, error) { return 0, 0, desktop.ErrFixedSize }
func (d *clipDesktop) Input(protocol.Control) error      { return nil }
func (d *clipDesktop) SetClipboard(string) error         { return nil }
func (d *clipDesktop) Clipboard() <-chan string          { return nil }
func (d *clipDesktop) Cursor() <-chan protocol.Cursor    { return nil }
func (d *clipDesktop) Close() error                      { return nil }

// tagging wraps the encoder the stream opens: it records each access unit
// and tags it with the clip frame it encodes.
type tagging struct {
	desk *clipDesktop
	open NewEncoderFunc

	mu   sync.Mutex
	sent []vqtest.Sent
}

type taggedEncoder struct {
	Encoder
	t      *tagging
	target int
}

func (t *tagging) newEncoder(w, h, fps, bitrate int) (Encoder, error) {
	e, err := t.open(w, h, fps, bitrate)
	if err != nil {
		return nil, err
	}
	return &taggedEncoder{Encoder: e, t: t, target: bitrate}, nil
}

func (e *taggedEncoder) SetBitrate(bps int) error {
	e.target = bps
	return e.Encoder.SetBitrate(bps)
}

func (e *taggedEncoder) Encode(img *yuv.I420, ts time.Duration, keyframe bool) ([]byte, error) {
	au, err := e.Encoder.Encode(img, ts, keyframe)
	if err != nil || len(au) == 0 {
		return au, err
	}
	i := int(e.t.desk.index.Load())
	qp := 0
	if q, ok := e.Encoder.(interface{ QP() int }); ok {
		qp = q.QP()
	}
	e.t.mu.Lock()
	e.t.sent = append(e.t.sent, vqtest.Sent{Index: i, Bytes: len(au), Key: vqtest.IsKeyframe(au), QP: qp, Target: e.target})
	e.t.mu.Unlock()
	return vqtest.Tag(au, i), nil
}

// clipRun is one pass of a clip through the real stream loop, peer and
// encoder to a stand-in browser over a shaped virtual link.
type clipRun struct {
	clip       *vqtest.Clip
	fps        int
	maxW, maxH int
	maxKbps    int
	link       vqtest.Link
	open       NewEncoderFunc
	// preroll is how long the blank screen streams after the first
	// picture, letting congestion control settle before the clip.
	preroll time.Duration
	// tune changes the stream's policies before it starts.
	tune func(*stream)
	// unpaced sends without the peer's pacer.
	unpaced bool
}

func softwareEncoder(t *testing.T) NewEncoderFunc {
	return paramsEncoder(t, func(*h264.Params) {})
}

// paramsEncoder opens openh264 with its defaults changed by edit.
func paramsEncoder(t *testing.T, edit func(*h264.Params)) NewEncoderFunc {
	return func(w, h, fps, bps int) (Encoder, error) {
		p := h264.Defaults(w, h, fps, bps)
		edit(&p)
		e, err := h264.Open(p)
		if err != nil {
			return nil, err
		}
		return e, nil
	}
}

// needVideoHarness skips unless this build has openh264 to encode and to
// decode with.
func needVideoHarness(t *testing.T) {
	t.Helper()
	if e, err := h264.New(64, 64, 30, 1_000_000); err != nil {
		t.Skip(err)
	} else {
		e.Close()
	}
	d, err := vqtest.NewDecoder()
	if err != nil {
		t.Skip(err)
	}
	d.Close()
}

func (c clipRun) run(t *testing.T) *vqtest.Run {
	t.Helper()
	net, err := c.link.Build()
	if err != nil {
		t.Fatal(err)
	}
	defer net.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("ROBIN_VQ_LOG") != "" {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	desk := newClipDesktop(c.clip, c.fps)
	tag := &tagging{desk: desk, open: c.open}
	maxW, maxH := c.maxW, c.maxH
	if maxW == 0 {
		maxW, maxH = c.clip.W, c.clip.H
	}
	s := newStream(desk, c.fps, maxW, maxH, tag.newEncoder, log)
	if c.tune != nil {
		c.tune(s)
	}

	maxBitrate := max(c.maxKbps*1000, startBitrate)
	if c.maxKbps == 0 {
		maxBitrate = protocol.DefaultMaxKbps * 1000
	}
	cands := make(chan protocol.Candidate, 64)
	p, err := peer.New(peer.Config{
		Net: net.Agent, MinBitrate: minBitrate, StartBitrate: min(startBitrate, maxBitrate), MaxBitrate: maxBitrate,
		Unpaced: c.unpaced, Log: log,
	}, peer.Handlers{
		Candidate: func(pc protocol.Candidate) { cands <- pc },
		Keyframe:  s.requestKeyframe,
		Bitrate:   s.setBitrate,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	b, err := vqtest.NewBrowser(net.Browser, c.clip, vqtest.Chrome)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Connect(p, cands); err != nil {
		t.Fatal(err)
	}
	s.setPeer(p)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.run(ctx)
		close(done)
	}()
	if err := b.WaitShown(20 * time.Second); err != nil {
		cancel()
		t.Fatal(err)
	}
	time.Sleep(c.preroll)
	start := desk.begin()
	time.Sleep(time.Duration(c.clip.Len())*time.Second/time.Duration(c.fps) + 500*time.Millisecond)
	cancel()
	<-done
	res, _ := b.Close()
	tag.mu.Lock()
	defer tag.mu.Unlock()
	return &vqtest.Run{Clip: c.clip, FPS: c.fps, Start: start, Sent: tag.sent, Result: res}
}

func mustClip(t *testing.T, bg vqtest.Background, w, h int, segs ...vqtest.Segment) *vqtest.Clip {
	t.Helper()
	c, err := vqtest.NewClip(bg, w, h, segs...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func seconds(k vqtest.Kind, s float64) vqtest.Segment {
	return vqtest.Segment{Kind: k, Frames: int(s * 30)}
}

// TestVideoQuality streams a short clip of real small text, still and
// moving, through the stream loop, the agent's peer and openh264 to a
// stand-in for Chrome over a 20 ms round trip, and scores what it shows.
func TestVideoQuality(t *testing.T) {
	if testing.Short() {
		t.Skip("streams for about 20 s")
	}
	needVideoHarness(t)
	clip := mustClip(t, vqtest.Captured, 960, 540,
		seconds(vqtest.Still, 7), seconds(vqtest.Typing, 2), seconds(vqtest.Scroll, 2), seconds(vqtest.Drag, 2), seconds(vqtest.Still, 4))
	r := clipRun{clip: clip, fps: 30, link: vqtest.Link{Delay: 10 * time.Millisecond}, open: softwareEncoder(t), preroll: 2 * time.Second}.run(t)
	t.Logf("\n%s%s", vqtest.Table(r.Spans()), r.Summary())
}

// matrixSizes are the screens the matrix streams: a 1080p monitor, the
// 1440p one of the session that prompted it, scaled to the default
// --max-size as it was, and the same shown at its own size.
var matrixSizes = []struct {
	name       string
	w, h       int
	maxW, maxH int
}{
	{"1080p", 1920, 1080, 1920, 1080},
	{"1440p-scaled", 2560, 1440, 1920, 1080},
	{"1440p", 2560, 1440, 2560, 1440},
}

// matrixLinks are the paths the matrix streams over.
var matrixLinks = []vqtest.Link{
	{Name: "fast", Rate: 50_000_000, Queue: 50 * time.Millisecond, Delay: 15 * time.Millisecond, Jitter: time.Millisecond, Loss: 0.001},
	{Name: "lossy", Rate: 20_000_000, Queue: 50 * time.Millisecond, Delay: 20 * time.Millisecond, Jitter: 2 * time.Millisecond, Loss: 0.01},
	{Name: "tight", Rate: 4_000_000, Queue: 100 * time.Millisecond, Delay: 30 * time.Millisecond},
}

// variant is a way of streaming the matrix compares: what the agent does
// now, or what it did before a change, so the ADR's before and after
// numbers can be measured again.
type variant struct {
	name string
	edit func(t *testing.T, c *clipRun)
}

var variants = []variant{
	{"current", func(*testing.T, *clipRun) {}},
	{"no-refresh", func(_ *testing.T, c *clipRun) { c.tune = func(s *stream) { s.refresh = 0 } }},
	{"refresh-250ms", func(_ *testing.T, c *clipRun) { c.tune = func(s *stream) { s.refresh = 250 * time.Millisecond } }},
	{"refresh-1s", func(_ *testing.T, c *clipRun) { c.tune = func(s *stream) { s.refresh = time.Second } }},
	{"unpaced", func(_ *testing.T, c *clipRun) { c.unpaced = true }},
	{"unpaced-no-refresh", func(_ *testing.T, c *clipRun) {
		c.unpaced = true
		c.tune = func(s *stream) { s.refresh = 0 }
	}},
	{"maxqp-51", func(t *testing.T, c *clipRun) { c.open = paramsEncoder(t, func(p *h264.Params) { p.MaxQP = 51 }) }},
	{"maxqp-44", func(t *testing.T, c *clipRun) { c.open = paramsEncoder(t, func(p *h264.Params) { p.MaxQP = 44 }) }},
	{"maxqp-40", func(t *testing.T, c *clipRun) { c.open = paramsEncoder(t, func(p *h264.Params) { p.MaxQP = 40 }) }},
	{"maxqp-36", func(t *testing.T, c *clipRun) { c.open = paramsEncoder(t, func(p *h264.Params) { p.MaxQP = 36 }) }},
	{"max-20mbps", func(_ *testing.T, c *clipRun) { c.maxKbps = 20_000 }},
}

// TestVideoQualityMatrix streams a longer clip at each matrix size over
// each matrix link and prints the numbers the ADR records. It is a
// measuring aid: set ROBIN_VQ=1, and ROBIN_VQ_SIZE, ROBIN_VQ_LINK or
// ROBIN_VQ_VARIANT to a comma-separated list of names to pick sizes, links
// and variants; the default variant is "current".
func TestVideoQualityMatrix(t *testing.T) {
	if os.Getenv("ROBIN_VQ") == "" {
		t.Skip("set ROBIN_VQ=1")
	}
	needVideoHarness(t)
	picked := func(env, name, fallback string) bool {
		list := os.Getenv(env)
		if list == "" {
			list = fallback
		}
		return list == "" || slices.Contains(strings.Split(list, ","), name)
	}
	for _, size := range matrixSizes {
		if !picked("ROBIN_VQ_SIZE", size.name, "") {
			continue
		}
		clip := mustClip(t, vqtest.Captured, size.w, size.h,
			seconds(vqtest.Still, 6), seconds(vqtest.Typing, 3), seconds(vqtest.Scroll, 3),
			seconds(vqtest.Drag, 3), seconds(vqtest.Video, 3), seconds(vqtest.Still, 6))
		for _, link := range matrixLinks {
			if !picked("ROBIN_VQ_LINK", link.Name, "") {
				continue
			}
			for _, v := range variants {
				if !picked("ROBIN_VQ_VARIANT", v.name, "current") {
					continue
				}
				c := clipRun{
					clip: clip, fps: 30, maxW: size.maxW, maxH: size.maxH, link: link,
					open: softwareEncoder(t), preroll: 3 * time.Second,
				}
				v.edit(t, &c)
				r := c.run(t)
				t.Logf("%s over %s (%v), %s:\n%s%s", size.name, link.Name, link, v.name, vqtest.Table(r.Spans()), r.Summary())
			}
		}
	}
}
