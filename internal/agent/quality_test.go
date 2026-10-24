//go:build linux

package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agent/desktop"
	"github.com/yasinmiran/robin/internal/agent/h264"
	"github.com/yasinmiran/robin/internal/agent/hwenc"
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

// begin starts the clip and returns the time its frame 0 is shown from.
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

// QP passes on the wrapped encoder's, which the stream logs with keyframes.
func (e *taggedEncoder) QP() int {
	if q, ok := e.Encoder.(qpReporter); ok {
		return q.QP()
	}
	return -1
}

func (e *taggedEncoder) Encode(img *yuv.I420, ts time.Duration, keyframe bool) ([]byte, error) {
	au, err := e.Encoder.Encode(img, ts, keyframe)
	if err != nil || len(au) == 0 {
		return au, err
	}
	i := int(e.t.desk.index.Load())
	e.t.mu.Lock()
	e.t.sent = append(e.t.sent, vqtest.Sent{Index: i, Bytes: len(au), Key: vqtest.IsKeyframe(au), QP: e.QP(), Target: e.target})
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
	// sound streams 20 ms Opus-sized packets of 240 bytes, 96 kbit/s,
	// as a desktop playing sound does.
	sound bool
	// feedWhileLimited and soundBehindVideo put back what Pion does.
	feedWhileLimited, soundBehindVideo bool
}

func softwareEncoder() NewEncoderFunc {
	return paramsEncoder(func(*h264.Params) {})
}

// paramsEncoder opens openh264 with its defaults changed by edit.
func paramsEncoder(edit func(*h264.Params)) NewEncoderFunc {
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
	t.Cleanup(net.Close)

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
		Unpaced: c.unpaced, Audio: c.sound, FeedWhileLimited: c.feedWhileLimited, SoundBehindVideo: c.soundBehindVideo, Log: log,
	}, peer.Handlers{
		Candidate: func(pc protocol.Candidate) { cands <- pc },
		Keyframe:  s.requestKeyframe,
		Bitrate:   s.setBitrate,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	b, err := vqtest.NewBrowser(net.Browser, c.clip, vqtest.Chrome)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
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
	sounding := make(chan struct{})
	go func() {
		defer close(sounding)
		if !c.sound {
			return
		}
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				p.WriteAudio(vqtest.SoundPayload(240, now), 20*time.Millisecond)
			}
		}
	}()
	stop := func() {
		cancel()
		<-done
		<-sounding
	}
	t.Cleanup(stop)
	if err := b.WaitShown(20 * time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(c.preroll)
	start := desk.begin()
	time.Sleep(time.Duration(c.clip.Len())*time.Second/time.Duration(c.fps) + 500*time.Millisecond)
	stop()
	res, err := b.Close()
	if err != nil {
		t.Error(err)
	}
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
// The browser never has to ask for a keyframe, still or moving, and a still
// screen ends up sharper than its first picture.
func TestVideoQuality(t *testing.T) {
	if testing.Short() {
		t.Skip("streams for about 20 s")
	}
	needVideoHarness(t)
	clip := mustClip(t, vqtest.Captured, 1920, 1080,
		seconds(vqtest.Still, 5), seconds(vqtest.Typing, 2), seconds(vqtest.Scroll, 2), seconds(vqtest.Drag, 2), seconds(vqtest.Still, 4))
	r := clipRun{clip: clip, fps: 30, link: vqtest.Link{Delay: 10 * time.Millisecond}, open: softwareEncoder(), preroll: 2 * time.Second}.run(t)
	spans := r.Spans()
	t.Logf("\n%s%s", vqtest.Table(spans), r.Summary())

	if n := len(r.Result.Requests); n != 0 {
		t.Errorf("the browser asked for %d keyframes: %s", n, r.Summary())
	}
	if r.Result.DecodeErrors != 0 || r.Result.Untagged != 0 {
		t.Errorf("%d decode errors, %d pictures without their frame", r.Result.DecodeErrors, r.Result.Untagged)
	}
	for _, s := range spans {
		switch {
		case s.Name == "all":
		case s.Shown == 0:
			t.Errorf("%s: nothing shown", s.Name)
		case s.Name == "scroll" || s.Name == "drag":
			// Rate control skips frames to keep text legible, 18 to 27 fps
			// here; a picture stuck behind a loss shows a few a second.
			if s.FPS < 10 {
				t.Errorf("%s: %.1f fps", s.Name, s.FPS)
			}
		}
	}
	var first vqtest.Score
	for _, p := range r.Result.Shown {
		if p.Index >= 0 {
			first = p.Score
			break
		}
	}
	if last := spans[0].Last; last.PSNR < first.PSNR+1 {
		t.Errorf("the still screen ends at %.1f dB, first shown at %.1f: refreshing it did not sharpen it", last.PSNR, first.PSNR)
	}
}

// matrixSizes are the screens the matrix streams: a 1080p monitor, the
// 1440p one of the session that prompted it scaled to 1920x1080, and the
// same shown at its own size.
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
	edit func(*clipRun)
}

var variants = []variant{
	{"current", func(*clipRun) {}},
	{"no-refresh", func(c *clipRun) { c.tune = func(s *stream) { s.refresh = 0 } }},
	{"refresh-250ms", func(c *clipRun) { c.tune = func(s *stream) { s.refresh = 250 * time.Millisecond } }},
	{"refresh-1s", func(c *clipRun) { c.tune = func(s *stream) { s.refresh = time.Second } }},
	{"unpaced", func(c *clipRun) { c.unpaced = true }},
	{"unpaced-no-refresh", func(c *clipRun) {
		c.unpaced = true
		c.tune = func(s *stream) { s.refresh = 0 }
	}},
	{"maxqp-51", func(c *clipRun) { c.open = paramsEncoder(func(p *h264.Params) { p.MaxQP = 51 }) }},
	{"maxqp-44", func(c *clipRun) { c.open = paramsEncoder(func(p *h264.Params) { p.MaxQP = 44 }) }},
	{"maxqp-40", func(c *clipRun) { c.open = paramsEncoder(func(p *h264.Params) { p.MaxQP = 40 }) }},
	{"maxqp-36", func(c *clipRun) { c.open = paramsEncoder(func(p *h264.Params) { p.MaxQP = 36 }) }},
	{"max-20mbps", func(c *clipRun) { c.maxKbps = 20_000 }},
	{"vaapi", func(c *clipRun) { c.open = hardwareEncoder(hwenc.VAAPI) }},
}

// hardwareEncoder opens a host tool's encoder, as the agent does for
// --encoder vaapi or nvenc.
func hardwareEncoder(kind hwenc.Kind) NewEncoderFunc {
	o := hwenc.NewOpener(kind, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return func(w, h, fps, bps int) (Encoder, error) {
		e, err := o.Open(w, h, fps, bps)
		if err != nil {
			return nil, err
		}
		return e, nil
	}
}

// needHardware skips unless this host has a working route for kind.
func needHardware(t *testing.T, kind hwenc.Kind) {
	t.Helper()
	e, err := hardwareEncoder(kind)(64, 64, 30, 1_000_000)
	if err != nil {
		t.Skip(err)
	}
	e.Close()
}

// TestVideoQualityMatrix streams a longer clip at each matrix size over
// each matrix link and prints the numbers the ADR records. It is a
// measuring aid: ROBIN_VQ=1 runs the current variant, ROBIN_VQ=all every
// variant, and -run picks cells by size, link and variant, as in
// -run 'TestVideoQualityMatrix/^1440p$/tight/unpaced$'.
func TestVideoQualityMatrix(t *testing.T) {
	mode := os.Getenv("ROBIN_VQ")
	if mode == "" {
		t.Skip("set ROBIN_VQ=1, or ROBIN_VQ=all for every variant")
	}
	needVideoHarness(t)
	for _, size := range matrixSizes {
		t.Run(size.name, func(t *testing.T) {
			clip := mustClip(t, vqtest.Captured, size.w, size.h,
				seconds(vqtest.Still, 6), seconds(vqtest.Typing, 3), seconds(vqtest.Scroll, 3),
				seconds(vqtest.Drag, 3), seconds(vqtest.Video, 3), seconds(vqtest.Still, 6))
			for _, link := range matrixLinks {
				t.Run(link.Name, func(t *testing.T) {
					for _, v := range variants {
						t.Run(v.name, func(t *testing.T) {
							if mode != "all" && v.name != "current" {
								t.Skip("set ROBIN_VQ=all")
							}
							if v.name == "vaapi" {
								needHardware(t, hwenc.VAAPI)
							}
							c := clipRun{
								clip: clip, fps: 30, maxW: size.maxW, maxH: size.maxH, link: link,
								open: softwareEncoder(), preroll: 3 * time.Second,
							}
							v.edit(&c)
							r := c.run(t)
							t.Logf("%s over %v, %s:\n%s%s", size.name, link, v.name, vqtest.Table(r.Spans()), r.Summary())
						})
					}
				})
			}
		})
	}
}

// afterStill is a clip that scrolls until congestion control has settled,
// sits still and scrolls again, with sound throughout: the first motion
// after a still screen, which goes out at whatever target the spell left.
type afterStill struct {
	link  vqtest.Link
	still float64 // seconds
	edit  func(*clipRun)
}

// afterStillScroll and afterStillSettled are how long the first scroll
// runs and how much of its end counts as the steady state.
const (
	afterStillScroll  = 8.0
	afterStillSettled = 4.0
)

// run returns the run, the settled end of the first scroll and the first
// second of the second.
func (a afterStill) run(t *testing.T) (r *vqtest.Run, steady, first vqtest.Span) {
	t.Helper()
	clip := mustClip(t, vqtest.Captured, 1920, 1080,
		seconds(vqtest.Scroll, afterStillScroll), seconds(vqtest.Still, a.still), seconds(vqtest.Scroll, 4))
	c := clipRun{clip: clip, fps: 30, link: a.link, open: softwareEncoder(), preroll: 3 * time.Second, sound: true}
	if a.edit != nil {
		a.edit(&c)
	}
	r = c.run(t)
	second := int((afterStillScroll + a.still) * 30)
	steady = r.Window("steady", int((afterStillScroll-afterStillSettled)*30), int(afterStillScroll*30))
	first = r.Window("first", second, second+30)
	var targets strings.Builder
	for i, f := range r.Sent {
		if i == 0 || f.Target != r.Sent[i-1].Target {
			fmt.Fprintf(&targets, " %.1fs:%d", float64(f.Index)/30, f.Target/1000)
		}
	}
	t.Logf("%v, %g s still:\n%s%s\ntarget in kbit/s by clip time:%s",
		a.link, a.still, vqtest.Table(append(r.Spans(), steady, first)), r.Summary(), targets.String())
	return r, steady, first
}

// TestVideoQualityAfterStill streams the after-still clip over the
// matrix's tight link, where a target that rose over the still spell
// overflows the link's buffer: with feed-while-limited, the first second
// of the second scroll shows 0 to 3 pictures. That second must show at
// least a third as many as the settled scroll, encoded against at least
// half its target, which a target that fell over the spell would not be.
// The bounds leave room for what is not the link's: after some spells
// openh264's rate control skips every other frame for a second, at QP 35
// where the settled scroll had QP 18, and GCC's target saws by 15% at a
// time on a link it fills, so the spell can begin at a low of the saw.
func TestVideoQualityAfterStill(t *testing.T) {
	if testing.Short() {
		t.Skip("streams for about 40 s")
	}
	needVideoHarness(t)
	_, steady, first := afterStill{link: matrixLinks[2], still: 20}.run(t)
	if first.FPS < steady.FPS/3 {
		t.Errorf("first second after the still: %.1f fps, settled %.1f", first.FPS, steady.FPS)
	}
	if first.KbpsTarget < steady.KbpsTarget/2 {
		t.Errorf("first second after the still encoded at %.0f kbit/s, settled at %.0f", first.KbpsTarget, steady.KbpsTarget)
	}
}

// TestVideoQualityAfterStillMatrix streams the after-still clip with a
// minute still over each matrix link three ways: as the agent does, with
// GCC fed while limited, and with sound queued behind video. Like the
// matrix, it is a measuring aid for ROBIN_VQ.
func TestVideoQualityAfterStillMatrix(t *testing.T) {
	if os.Getenv("ROBIN_VQ") == "" {
		t.Skip("set ROBIN_VQ=1")
	}
	needVideoHarness(t)
	for _, link := range matrixLinks {
		t.Run(link.Name, func(t *testing.T) {
			for _, v := range []variant{
				{"current", func(*clipRun) {}},
				{"feed-while-limited", func(c *clipRun) { c.feedWhileLimited = true }},
				{"sound-behind-video", func(c *clipRun) { c.soundBehindVideo = true }},
			} {
				t.Run(v.name, func(t *testing.T) { afterStill{link: link, still: 60, edit: v.edit}.run(t) })
			}
		})
	}
}
