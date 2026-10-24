package peer

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/transport/v5/vnet"
	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/robin/internal/protocol"
)

// TestCongestionControlAdaptsToLink streams through a shaped virtual link with
// frames sized to whatever the congestion controller currently allows, as the
// agent's encoder does. The controller must settle near the link rate and the
// receiver must keep getting video.
func TestCongestionControlAdaptsToLink(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 30 s")
	}
	for _, tc := range []struct {
		name            string
		start, link     int
		dropTo, dropAtS int // optional mid-session change of the link rate
	}{
		// The start rate is a guess; a 4x overshoot used to trigger a
		// retransmission storm that pinned the target at its floor.
		{name: "overshooting start", start: 4_000_000, link: 1_000_000},
		{name: "link drops mid-session", start: 2_000_000, link: 3_000_000, dropTo: 1_000_000, dropAtS: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			final := tc.link
			if tc.dropTo > 0 {
				final = tc.dropTo
			}
			target, delivered := streamOverShapedLink(t, tc.start, tc.link, tc.dropTo, tc.dropAtS)
			t.Logf("link ends at %d kbit/s: target %d kbit/s, delivered %d kbit/s", final/1000, target/1000, delivered/1000)
			if target > final*16/10 {
				t.Errorf("target %d kbit/s did not come down to a %d kbit/s link", target/1000, final/1000)
			}
			if delivered < final*6/10 {
				t.Errorf("only %d kbit/s delivered on a %d kbit/s link", delivered/1000, final/1000)
			}
		})
	}
}

// TestBulkSharesTheLinkWithVideo sends a bulk transfer through a shaped link
// while video streams, and measures what each gets. Bulk is not paced:
// SCTP's own congestion control fills the queue, and the video's controller
// answers the delay by lowering its target until the transfer ends.
func TestBulkSharesTheLinkWithVideo(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 30 s")
	}
	const link = 3_000_000
	var disconnects atomic.Int32
	var failed atomic.Bool
	l := newShapedLink(t, Config{StartBitrate: 2_000_000}, link, Handlers{State: func(s webrtc.PeerConnectionState) {
		switch s {
		case webrtc.PeerConnectionStateDisconnected:
			disconnects.Add(1)
		case webrtc.PeerConnectionStateFailed:
			failed.Store(true)
		}
	}})
	videoRate := func(d time.Duration) int {
		before := l.received.Load()
		time.Sleep(d)
		return int(float64(l.received.Load()-before) * 8 / d.Seconds())
	}
	time.Sleep(6 * time.Second)
	videoBefore, targetBefore := videoRate(2*time.Second), l.target.Load()

	const size = 2 << 20
	done := make(chan error, 1)
	start, receivedAtStart := time.Now(), l.received.Load()
	go func() {
		done <- l.p.SendBulk(context.Background(), protocol.Bulk{Kind: testKind, Size: size}, bytes.NewReader(make([]byte, size)))
	}()
	lowest := targetBefore
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(60 * time.Second)
	var err error
wait:
	for {
		select {
		case err = <-done:
			break wait
		case <-tick.C:
			lowest = min(lowest, l.target.Load())
		case <-timeout:
			t.Fatal("transfer never finished")
		}
	}
	took := time.Since(start)
	videoDuring := int(float64(l.received.Load()-receivedAtStart) * 8 / took.Seconds())
	// The target climbs back at the controller's own pace.
	recovered := time.Now()
	for l.target.Load() < targetBefore*8/10 && time.Since(recovered) < 20*time.Second {
		time.Sleep(100 * time.Millisecond)
	}
	recovery := time.Since(recovered)

	t.Logf("%d kbit/s link: video %d kbit/s (target %d) before; %d KiB of bulk took %v, %d kbit/s, "+
		"while video got %d kbit/s with its target down to %d; target back to 80%% %v after; %d disconnects",
		link/1000, videoBefore/1000, targetBefore/1000, size>>10, took.Round(10*time.Millisecond),
		int(float64(size*8)/took.Seconds())/1000, videoDuring/1000, lowest/1000, recovery.Round(100*time.Millisecond), disconnects.Load())
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if failed.Load() {
		t.Error("the connection failed under the transfer")
	}
	if videoDuring < link/10 {
		t.Errorf("video starved to %d kbit/s during the transfer", videoDuring/1000)
	}
	if recovery >= 20*time.Second {
		t.Errorf("video target still %d kbit/s 20 s after the transfer", l.target.Load()/1000)
	}
}

// TestCongestionControlHoldsThroughAStillScreen streams until the target
// settles, sits still for 20 s with a refresh every 500 ms, and streams
// again. Over the still spell GCC used to raise its target 8% a second to
// the ceiling, which the first motion then overflowed, or to fall to a
// fraction of the link. The target must come out of the spell where it
// went in, and the first second of motion must deliver most of the link.
// ROBIN_FEED_WHILE_LIMITED=1 feeds GCC as Pion would, to see it fail.
func TestCongestionControlHoldsThroughAStillScreen(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 30 s")
	}
	const link = 3_000_000
	feed := os.Getenv("ROBIN_FEED_WHILE_LIMITED") != ""
	l := newShapedLink(t, Config{StartBitrate: 2_000_000, FeedWhileLimited: feed}, link, Handlers{})
	time.Sleep(8 * time.Second)
	before := int(l.target.Load())
	l.still.Store(true)
	time.Sleep(20 * time.Second)
	after := int(l.target.Load())
	l.still.Store(false)
	received := l.received.Load()
	time.Sleep(time.Second)
	delivered := int(l.received.Load()-received) * 8
	t.Logf("%d kbit/s link: target %d kbit/s before 20 s still, %d after; the first second of motion delivered %d kbit/s",
		link/1000, before/1000, after/1000, delivered/1000)
	if after < before*7/10 || after > before*13/10 {
		t.Errorf("the target went from %d kbit/s to %d over the still spell", before/1000, after/1000)
	}
	if delivered < link*6/10 {
		t.Errorf("the first second of motion delivered %d kbit/s on a %d kbit/s link", delivered/1000, link/1000)
	}
}

// streamOverShapedLink runs a peer over a virtual link of linkBps for 14 s and
// returns the controller's final target and the rate delivered over the last
// 4 s.
func streamOverShapedLink(t *testing.T, startBps, linkBps, dropTo, dropAtS int) (target, delivered int) {
	t.Helper()
	l := newShapedLink(t, Config{StartBitrate: startBps}, linkBps, Handlers{})
	for s := 1; s <= 10; s++ {
		time.Sleep(time.Second)
		if dropTo > 0 && s == dropAtS {
			l.tbf.SetRate(dropTo)
		}
	}
	before := l.received.Load()
	time.Sleep(4 * time.Second)
	return int(l.target.Load()), int((l.received.Load() - before) * 8 / 4)
}

// shapedLink is an agent peer streaming to a stand-in browser through a
// virtual link whose rate toward the browser the test sets.
type shapedLink struct {
	p   *Peer
	b   *browser
	tbf *vnet.TBFQueue
	// target is the congestion controller's target; received counts the
	// video bytes the browser got.
	target, received atomic.Int64
	// still sends a refresh every 500 ms instead of frames the size of
	// the target, as a still screen does.
	still atomic.Bool
}

// newShapedLink connects the peers over a link of linkBps with a 100 ms
// queue toward the browser, like a congested access link, and streams
// frames sized to whatever the congestion controller allows, as the
// agent's encoder does. cfg gives the start rate and any comparison; it
// returns once video arrives. h may add handlers; the browser accepts bulk
// transfers and discards them.
func newShapedLink(t *testing.T, cfg Config, linkBps int, h Handlers) *shapedLink {
	t.Helper()
	wan, err := vnet.NewRouter(&vnet.RouterConfig{
		CIDR: "10.0.0.0/24", MinDelay: 10 * time.Millisecond, LoggerFactory: logging.NewDefaultLoggerFactory(),
	})
	if err != nil {
		t.Fatal(err)
	}
	agentNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{"10.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	browserNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{"10.0.0.2"}})
	if err != nil {
		t.Fatal(err)
	}
	l := &shapedLink{tbf: vnet.NewTBFQueue(linkBps, 16_000, int64(linkBps/8/10))}
	shaped, err := vnet.NewQueue(browserNet, l.tbf)
	if err != nil {
		t.Fatal(err)
	}
	if err := wan.AddNet(agentNet); err != nil {
		t.Fatal(err)
	}
	if err := wan.AddNet(shaped); err != nil {
		t.Fatal(err)
	}
	if err := wan.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wan.Stop() })

	l.target.Store(int64(cfg.StartBitrate))
	cands := make(chan protocol.Candidate, 32)
	h.Candidate = func(c protocol.Candidate) { cands <- c }
	h.Bitrate = func(bps int) { l.target.Store(int64(bps)) }
	cfg.Net, cfg.MinBitrate, cfg.MaxBitrate = agentNet, 100_000, 8_000_000
	l.p, err = New(cfg, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.p.Close() })

	// The browser side: default codecs and interceptors, including the TWCC
	// feedback and NACKs that Chrome sends, and a control channel as the
	// page opens.
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	reg := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, reg); err != nil {
		t.Fatal(err)
	}
	se := webrtc.SettingEngine{}
	se.SetNet(browserNet)
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	b := &browser{got: make(chan protocol.Control, 8), track: make(chan *webrtc.TrackRemote, 1)}
	b.pc, err = webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(reg), webrtc.WithSettingEngine(se)).
		NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.pc.Close() })
	l.b = b
	b.pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	b.pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { b.track <- tr })
	if b.control, err = b.pc.CreateDataChannel(protocol.ControlChannel, nil); err != nil {
		t.Fatal(err)
	}
	b.pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		receiveBulk(dc, func(protocol.Bulk) (BulkSink, error) { return discard{}, nil }, slog.Default())
	})
	connect(t, b, l.p, cands)

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		const fps = 30
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(time.Second / fps):
			}
			size := max(int(l.target.Load())/8/fps, 64)
			if l.still.Load() {
				if i%(fps/2) != 0 {
					continue
				}
				size = 20_000
			}
			au := make([]byte, size)
			copy(au, []byte{0, 0, 0, 1, 0x65})
			l.p.WriteFrame(au, time.Second/fps)
		}
	}()

	var tr *webrtc.TrackRemote
	select {
	case tr = <-b.track:
	case <-time.After(15 * time.Second):
		t.Fatal("no video at the browser")
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err := tr.Read(buf)
			if err != nil {
				return
			}
			l.received.Add(int64(n))
		}
	}()
	return l
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
func (discard) Commit() error               { return nil }
func (discard) Abort(error)                 {}
