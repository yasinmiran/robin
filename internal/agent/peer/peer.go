// Package peer is the agent's side of one WebRTC session with a browser: an
// H.264 video track out, the control data channel both ways, and the
// congestion controller that tells the encoder how fast it may go.
package peer

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/yasinmiran/remotejump/internal/protocol"
)

// H264Fmtp is the one video format the agent offers: Constrained Baseline in
// packetization mode 1, which every browser decodes and openh264 emits.
const H264Fmtp = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"

// MaxControlBytes bounds one control message. Browsers cap SCTP messages at
// 256 KiB, so larger clipboard text could not cross anyway.
const MaxControlBytes = 256 << 10

// Config is fixed for the life of a Peer.
type Config struct {
	STUN []string
	// TCPMux accepts ICE-TCP from the SSH tunnel. It outlives individual peers
	// because the CLI's forward points at its listener. Nil disables ICE-TCP.
	TCPMux ice.TCPMux
	// IncludeLoopback gathers loopback candidates. The tunnel listener is
	// loopback-only, so ICE-TCP needs this.
	IncludeLoopback bool

	MinBitrate, StartBitrate, MaxBitrate int // bits per second
	Log                                  *slog.Logger
}

// Handlers receive the peer's events. Each may be nil. They are called from
// Pion's goroutines and must not block.
type Handlers struct {
	Candidate func(protocol.Candidate)
	Control   func(protocol.Control)
	Keyframe  func()        // the browser lost a picture or just connected
	Bitrate   func(bps int) // new congestion-control target
	State     func(state webrtc.PeerConnectionState)
}

// Peer is one browser session.
type Peer struct {
	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticSample
	h     Handlers
	log   *slog.Logger

	mu      sync.Mutex
	control *webrtc.DataChannel
	bwe     cc.BandwidthEstimator
}

// New builds the peer connection and its video track. Nothing is sent until
// an offer has been answered and ICE connects.
func New(cfg Config, h Handlers) (*Peer, error) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	m := &webrtc.MediaEngine{}
	feedback := []webrtc.RTCPFeedback{
		{Type: webrtc.TypeRTCPFBNACK},
		{Type: webrtc.TypeRTCPFBNACK, Parameter: "pli"},
		{Type: webrtc.TypeRTCPFBCCM, Parameter: "fir"},
		{Type: webrtc.TypeRTCPFBTransportCC},
	}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: H264Fmtp, RTCPFeedback: feedback,
		},
		PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}

	reg := &interceptor.Registry{}
	bweReady := make(chan cc.BandwidthEstimator, 1)
	ccf, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		return gcc.NewSendSideBWE(
			gcc.SendSideBWEInitialBitrate(cfg.StartBitrate),
			gcc.SendSideBWEMinBitrate(cfg.MinBitrate),
			gcc.SendSideBWEMaxBitrate(cfg.MaxBitrate),
		)
	})
	if err != nil {
		return nil, err
	}
	ccf.OnNewPeerConnection(func(_ string, e cc.BandwidthEstimator) { bweReady <- e })
	reg.Add(ccf)
	if err := webrtc.ConfigureTWCCHeaderExtensionSender(m, reg); err != nil {
		return nil, err
	}
	if err := webrtc.ConfigureNack(m, reg); err != nil {
		return nil, err
	}
	if err := webrtc.ConfigureRTCPReports(reg); err != nil {
		return nil, err
	}

	se := webrtc.SettingEngine{}
	se.SetSCTPMaxMessageSize(MaxControlBytes)
	// The browser drives recovery with an ICE restart after ~2 s of
	// disconnection; failing late gives that restart time to land.
	se.SetICETimeouts(5*time.Second, 30*time.Second, 2*time.Second)
	if cfg.TCPMux != nil {
		se.SetICETCPMux(cfg.TCPMux)
		se.SetNetworkTypes([]webrtc.NetworkType{
			webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6, webrtc.NetworkTypeTCP4,
		})
	}
	se.SetIncludeLoopbackCandidate(cfg.IncludeLoopback)

	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(reg), webrtc.WithSettingEngine(se))
	var ice []webrtc.ICEServer
	if len(cfg.STUN) > 0 {
		ice = []webrtc.ICEServer{{URLs: cfg.STUN}}
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: ice})
	if err != nil {
		return nil, err
	}
	p := &Peer{pc: pc, h: h, log: log}

	select {
	case p.bwe = <-bweReady:
	case <-time.After(5 * time.Second):
		pc.Close()
		return nil, errors.New("peer: congestion controller did not start")
	}
	p.bwe.OnTargetBitrateChange(func(bps int) {
		if h.Bitrate != nil {
			h.Bitrate(bps)
		}
	})

	p.track, err = webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: H264Fmtp},
		"video", "remotejump",
	)
	if err != nil {
		pc.Close()
		return nil, err
	}
	sender, err := pc.AddTrack(p.track)
	if err != nil {
		pc.Close()
		return nil, err
	}
	go p.readRTCP(sender)

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if h.Candidate == nil {
			return
		}
		if c == nil {
			h.Candidate(protocol.Candidate{})
			return
		}
		init := c.ToJSON()
		h.Candidate(protocol.Candidate{
			Candidate: init.Candidate, SDPMid: init.SDPMid,
			SDPMLineIndex: init.SDPMLineIndex, UsernameFragment: init.UsernameFragment,
		})
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Info("peer connection", "state", s.String())
		if s == webrtc.PeerConnectionStateConnected && h.Keyframe != nil {
			h.Keyframe()
		}
		if h.State != nil {
			h.State(s)
		}
	})
	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		log.Info("ice connection", "state", s.String())
	})
	pc.OnDataChannel(p.attachControl)
	return p, nil
}

// Answer applies an offer and returns the answer SDP. A later offer on the
// same peer is a renegotiation, typically the browser's ICE restart.
func (p *Peer) Answer(offerSDP string) (string, error) {
	if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
		return "", fmt.Errorf("peer: apply offer: %w", err)
	}
	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("peer: create answer: %w", err)
	}
	if err := p.pc.SetLocalDescription(answer); err != nil {
		return "", fmt.Errorf("peer: apply answer: %w", err)
	}
	return p.pc.LocalDescription().SDP, nil
}

// AddCandidate applies a browser candidate. An empty candidate marks the end
// of the browser's gathering.
func (p *Peer) AddCandidate(c protocol.Candidate) error {
	return p.pc.AddICECandidate(webrtc.ICECandidateInit{
		Candidate: c.Candidate, SDPMid: c.SDPMid,
		SDPMLineIndex: c.SDPMLineIndex, UsernameFragment: c.UsernameFragment,
	})
}

// WriteFrame sends one encoded access unit in Annex-B form.
func (p *Peer) WriteFrame(annexB []byte, d time.Duration) error {
	return p.track.WriteSample(media.Sample{Data: annexB, Duration: d})
}

// SendControl sends a message to the browser over the control channel. It is
// a no-op until the browser has opened the channel.
func (p *Peer) SendControl(m protocol.Control) error {
	p.mu.Lock()
	dc := p.control
	p.mu.Unlock()
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b) > MaxControlBytes {
		return fmt.Errorf("peer: control message of %d bytes exceeds %d", len(b), MaxControlBytes)
	}
	return dc.SendText(string(b))
}

// Bitrate is the congestion controller's current target in bits per second.
func (p *Peer) Bitrate() int { return p.bwe.GetTargetBitrate() }

// Close tears the peer connection down.
func (p *Peer) Close() error { return p.pc.Close() }

func (p *Peer) attachControl(dc *webrtc.DataChannel) {
	if dc.Label() != protocol.ControlChannel {
		p.log.Warn("ignoring unexpected data channel", "label", dc.Label())
		dc.Close()
		return
	}
	p.mu.Lock()
	p.control = dc
	p.mu.Unlock()
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if !msg.IsString || len(msg.Data) > MaxControlBytes || p.h.Control == nil {
			return
		}
		var m protocol.Control
		if err := json.Unmarshal(msg.Data, &m); err != nil {
			p.log.Warn("bad control message", "err", err)
			return
		}
		p.h.Control(m)
	})
}

// readRTCP drains RTCP so the interceptors (NACK, congestion control) see it,
// and turns picture-loss requests into keyframes.
func (p *Peer) readRTCP(sender *webrtc.RTPSender) {
	for {
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		for _, pkt := range pkts {
			switch pkt.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				if p.h.Keyframe != nil {
					p.h.Keyframe()
				}
			}
		}
	}
}
