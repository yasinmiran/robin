package peer

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/transport/v5/vnet"
	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/remotejump/internal/protocol"
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

// streamOverShapedLink runs a peer over a virtual link of linkBps for 14 s and
// returns the controller's final target and the rate delivered over the last
// 4 s.
func streamOverShapedLink(t *testing.T, startBps, linkBps, dropTo, dropAtS int) (target, delivered int) {
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
	// Everything bound for the browser drains through a token bucket with a
	// 100 ms queue, like a congested access link.
	tbf := vnet.NewTBFQueue(linkBps, 16_000, int64(linkBps/8/10))
	shaped, err := vnet.NewQueue(browserNet, tbf)
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
	defer wan.Stop()

	var want atomic.Int64
	want.Store(int64(startBps))
	cands := make(chan protocol.Candidate, 32)
	p, err := New(Config{
		Net: agentNet, MinBitrate: 100_000, StartBitrate: startBps, MaxBitrate: 8_000_000,
	}, Handlers{
		Candidate: func(c protocol.Candidate) { cands <- c },
		Bitrate:   func(bps int) { want.Store(int64(bps)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// The browser side: default codecs and interceptors, including the TWCC
	// feedback and NACKs that Chrome sends.
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
	defer b.pc.Close()
	b.pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	b.pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { b.track <- tr })
	connect(t, b, p, cands)

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		const fps = 30
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Second / fps):
			}
			au := make([]byte, max(int(want.Load())/8/fps, 64))
			copy(au, []byte{0, 0, 0, 1, 0x65})
			p.WriteFrame(au, time.Second/fps)
		}
	}()

	var tr *webrtc.TrackRemote
	select {
	case tr = <-b.track:
	case <-time.After(15 * time.Second):
		t.Fatal("no video at the browser")
	}
	var received atomic.Int64
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err := tr.Read(buf)
			if err != nil {
				return
			}
			received.Add(int64(n))
		}
	}()

	for s := 1; s <= 10; s++ {
		time.Sleep(time.Second)
		if dropTo > 0 && s == dropAtS {
			tbf.SetRate(dropTo)
		}
	}
	before := received.Load()
	time.Sleep(4 * time.Second)
	return int(want.Load()), int((received.Load() - before) * 8 / 4)
}
