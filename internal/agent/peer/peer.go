// Package peer is the agent's side of one WebRTC session with a browser: an
// H.264 video track out, the control data channel and bulk transfers both
// ways, the browser's microphone and camera tracks in with the feedback a
// receiver owes for them, and the congestion controller that tells the
// encoder how fast it may go.
package peer

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/rtcp"
	"github.com/pion/stun/v4"
	"github.com/pion/transport/v5"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/yasinmiran/robin/internal/protocol"
)

// H264Fmtp is the one video format the agent offers: Constrained Baseline in
// packetization mode 1, which every browser decodes and openh264 emits.
const H264Fmtp = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"

// MaxControlBytes bounds one control message. Browsers cap SCTP messages at
// 256 KiB, so larger clipboard text could not cross anyway.
const MaxControlBytes = 256 << 10

// sctpReceiveBuffer holds the credit of eight bulk transfers from the
// browser whose sinks have stalled, and room for control beside them.
// Pion's default holds two.
const sctpReceiveBuffer = 8*protocol.BulkWindow + 256<<10

// ErrNoControl means the browser has not opened the control channel yet, so
// nothing was sent.
var ErrNoControl = errors.New("peer: control channel not open")

// Config is fixed for the life of a Peer.
type Config struct {
	STUN []string
	// TCPMux accepts ICE-TCP from the SSH tunnel. It outlives individual peers
	// because the CLI's forward points at its listener. Nil disables ICE-TCP.
	TCPMux ice.TCPMux
	// IncludeLoopback gathers loopback candidates. The tunnel listener is
	// loopback-only, so ICE-TCP needs this.
	IncludeLoopback bool
	// Net replaces the real network; tests use it to run over a shaped virtual
	// link. Nil means the host network.
	Net transport.Net
	// Certificate is the DTLS identity. The agent keeps one for its whole
	// session so the fingerprint the CLI pinned survives a page reload. Nil
	// generates a fresh one.
	Certificate *webrtc.Certificate
	// Audio adds an Opus track for WriteAudio.
	Audio bool
	// Video and AudioTrack are the tracks this peer sends, when the session
	// shares them among its peers so each frame and packet is written once.
	// Nil makes the peer a track of its own; AudioTrack counts only with
	// Audio.
	Video      *VideoTrack
	AudioTrack *webrtc.TrackLocalStaticSample
	// Mic and Camera take the browser's microphone (Opus) and camera
	// (H.264) as tracks for Handlers.Track, with what a receiver owes the
	// sender: transport-wide congestion feedback for its rate, NACKs for
	// the camera's lost packets.
	Mic, Camera bool

	MinBitrate, StartBitrate, MaxBitrate int // bits per second
	// Unpaced sends every packet and every bulk chunk the moment it is
	// written, as a comparison for measurements; the agent paces.
	Unpaced bool
	// FeedWhileLimited hands GCC the browser's feedback while the agent
	// sends too little to test the link, as Pion does on its own, and
	// SoundBehindVideo queues sound with video in the pacer: comparisons
	// for measurements, which the agent does neither of.
	FeedWhileLimited, SoundBehindVideo bool
	Log                                *slog.Logger

	// headerWait replaces bulkHeaderWait, for tests that cannot wait it out.
	headerWait time.Duration
}

// Handlers receive the peer's events. Each may be nil. They are called from
// Pion's goroutines and must not block.
type Handlers struct {
	Candidate func(protocol.Candidate)
	Control   func(protocol.Control)
	// ControlOpen fires when the browser opens the control channel, so state
	// sent before then can be sent again.
	ControlOpen func()
	Keyframe    func()        // the browser lost a picture or just connected
	Bitrate     func(bps int) // new congestion-control target
	State       func(state webrtc.PeerConnectionState)
	// Bulk is asked about each transfer the browser opens; see BulkSink.
	// Without it every transfer is refused.
	Bulk func(protocol.Bulk) (BulkSink, error)
	// Track receives the microphone's and camera's tracks, on the lines
	// Config.Mic and Config.Camera take. The peer refuses or drops any
	// other media the browser sends, and all of it without Track.
	Track func(*webrtc.TrackRemote, *webrtc.RTPReceiver)
}

// Peer is one browser session.
type Peer struct {
	pc    *webrtc.PeerConnection
	track *VideoTrack
	audio *webrtc.TrackLocalStaticSample // nil without Config.Audio
	h     Handlers
	log   *slog.Logger
	// unpaced sends bulk as fast as the browser's credit allows.
	unpaced bool

	mu      sync.Mutex
	control *webrtc.DataChannel
	bwe     *estimator

	// taken are the receivers of the microphone and camera lines, as
	// takeMedia picked them; mu guards them.
	taken       [2]*webrtc.RTPReceiver
	mic, camera bool

	remote      remoteCandidates
	waitingBulk atomic.Int32 // bulk channels the browser opened, before their first frame
	headerWait  time.Duration
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
	if cfg.Audio || cfg.Mic {
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: opusCapability, PayloadType: 111,
		}, webrtc.RTPCodecTypeAudio); err != nil {
			return nil, err
		}
	}

	reg := &interceptor.Registry{}
	bweReady := make(chan *estimator, 1)
	ccf, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		// Not Pion's pacer: it queues without bound, so a burst sent while
		// the target falls drains for seconds and the delay that adds keeps
		// the target at its floor. The encoder holds the rate; the pacer
		// only spreads bursts, keyframes above all.
		var p gcc.Pacer = gcc.NewNoOpPacer()
		if !cfg.Unpaced {
			p = newPacer(cfg.StartBitrate)
		}
		e, err := newEstimator(cfg, p, log, time.Now)
		if err != nil {
			return nil, err
		}
		bweReady <- e
		return e, nil
	})
	if err != nil {
		return nil, err
	}
	reg.Add(ccf)
	if err := webrtc.ConfigureTWCCHeaderExtensionSender(m, reg); err != nil {
		return nil, err
	}
	// Registered after the TWCC and congestion-control interceptors so
	// retransmissions carry transport sequence numbers and count as sent.
	var bwe atomic.Pointer[estimator]
	reg.Add(&nackResponderFactory{
		budget: func() int {
			if e := bwe.Load(); e != nil {
				return e.GetTargetBitrate()
			}
			return cfg.StartBitrate
		},
		lost: func() {
			log.Info("keyframe requested", "by", "nack-unrecoverable")
			if h.Keyframe != nil {
				h.Keyframe()
			}
		},
	})
	if err := webrtc.ConfigureRTCPReports(reg); err != nil {
		return nil, err
	}
	if cfg.Mic || cfg.Camera {
		if err := addReceiverFeedback(reg); err != nil {
			return nil, err
		}
	}
	if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: playoutDelayURI}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}
	reg.Add(playoutDelayFactory{})

	se := webrtc.SettingEngine{}
	se.SetSCTPMaxMessageSize(MaxControlBytes)
	se.SetSCTPMaxReceiveBufferSize(sctpReceiveBuffer)
	// Disconnected after 2 s lets followNomination react quickly; failing late
	// gives the browser's ICE restart time to land.
	se.SetICETimeouts(2*time.Second, 30*time.Second, time.Second)
	var pcRef atomic.Pointer[webrtc.PeerConnection]
	se.SetICEBindingRequestHandler(func(msg *stun.Message, _, _ ice.Candidate, pair *ice.CandidatePair) bool {
		return followNomination(msg, pair, pcRef.Load())
	})
	if cfg.TCPMux != nil {
		se.SetICETCPMux(cfg.TCPMux)
		se.SetNetworkTypes([]webrtc.NetworkType{
			webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6, webrtc.NetworkTypeTCP4,
		})
	}
	se.SetIncludeLoopbackCandidate(cfg.IncludeLoopback)
	// No mDNS: whoever answers a query on the host's network would pick
	// where the agent sends checks for the browser's .local names, and the
	// browser reaches the agent as a peer-reflexive candidate anyway.
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	// ICE-TCP comes in through the tunnel listener only. Connecting out to
	// the passive TCP candidates the page names would aim the agent at any
	// port the host can reach.
	se.DisableActiveTCP(true)
	if cfg.Net != nil {
		se.SetNet(cfg.Net)
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(reg), webrtc.WithSettingEngine(se))
	var servers []webrtc.ICEServer
	if len(cfg.STUN) > 0 {
		servers = []webrtc.ICEServer{{URLs: cfg.STUN}}
	}
	pcConfig := webrtc.Configuration{ICEServers: servers}
	if cfg.Certificate != nil {
		pcConfig.Certificates = []webrtc.Certificate{*cfg.Certificate}
	}
	pc, err := api.NewPeerConnection(pcConfig)
	if err != nil {
		return nil, err
	}
	pcRef.Store(pc)
	p := &Peer{
		pc: pc, h: h, log: log, unpaced: cfg.Unpaced,
		mic: cfg.Mic, camera: cfg.Camera, headerWait: cmp.Or(cfg.headerWait, bulkHeaderWait),
	}

	select {
	case p.bwe = <-bweReady:
		bwe.Store(p.bwe)
	case <-time.After(5 * time.Second):
		pc.Close()
		return nil, errors.New("peer: congestion controller did not start")
	}
	p.bwe.OnTargetBitrateChange(func(bps int) {
		if h.Bitrate != nil {
			h.Bitrate(bps)
		}
	})

	if p.track = cfg.Video; p.track == nil {
		if p.track, err = NewVideoTrack(); err != nil {
			pc.Close()
			return nil, err
		}
	}
	sender, err := pc.AddTrack(p.track)
	if err != nil {
		pc.Close()
		return nil, err
	}
	go p.readRTCP(sender)
	if cfg.Audio {
		if p.audio = cfg.AudioTrack; p.audio == nil {
			if p.audio, err = NewAudioTrack(); err != nil {
				pc.Close()
				return nil, err
			}
		}
		audioSender, err := pc.AddTrack(p.audio)
		if err != nil {
			pc.Close()
			return nil, err
		}
		// Feedback the browser addresses to the audio stream reaches the
		// interceptors only if it is read.
		go p.readRTCP(audioSender)
	}

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
	pc.OnTrack(func(tr *webrtc.TrackRemote, r *webrtc.RTPReceiver) {
		log.Info("remote track", "kind", tr.Kind().String(), "codec", tr.Codec().MimeType)
		// The browser's sender reports reach the interceptors only if read.
		go drainRTCP(r)
		if h.Track == nil || !p.wanted(r) {
			log.Warn("dropping media the agent did not ask for", "kind", tr.Kind().String())
			go drop(tr)
			return
		}
		h.Track(tr, r)
	})
	pc.OnDataChannel(p.route)
	return p, nil
}

// Answer applies an offer and returns the answer SDP. A later offer on the
// same peer is a renegotiation, typically the browser's ICE restart.
func (p *Peer) Answer(offerSDP string) (string, error) {
	sdp, commit, err := p.remote.offer(offerSDP)
	if err != nil {
		return "", err
	}
	if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		return "", fmt.Errorf("peer: apply offer: %w", err)
	}
	commit()
	p.takeMedia()
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
// of the browser's gathering. Past maxRemoteCandidates since the last ICE
// restart it drops the candidate and returns ErrTooManyCandidates. One at
// an address the browser is never at counts, and is dropped.
func (p *Peer) AddCandidate(c protocol.Candidate) error {
	if c.Candidate != "" {
		if warn, err := p.remote.add(c.Candidate); err != nil {
			if warn {
				p.log.Warn("dropping the browser's candidates past the bound", "max", maxRemoteCandidates)
			}
			return err
		}
		if outOfReach(c.Candidate) {
			p.log.Debug("dropping a candidate at loopback, link-local or no unicast address")
			return nil
		}
	}
	return p.pc.AddICECandidate(webrtc.ICECandidateInit{
		Candidate: c.Candidate, SDPMid: c.SDPMid,
		SDPMLineIndex: c.SDPMLineIndex, UsernameFragment: c.UsernameFragment,
	})
}

// WriteFrame sends one encoded access unit in Annex-B form, on the peer's
// video track: to every peer that shares it.
func (p *Peer) WriteFrame(annexB []byte, d time.Duration) error {
	return p.track.Write(annexB, d)
}

// NewAudioTrack makes an Opus track for Config.AudioTrack.
func NewAudioTrack() (*webrtc.TrackLocalStaticSample, error) {
	return webrtc.NewTrackLocalStaticSample(opusCapability, "audio", "robin")
}

// WriteAudio sends one Opus packet. It is a no-op without Config.Audio.
func (p *Peer) WriteAudio(opus []byte, d time.Duration) error {
	if p.audio == nil {
		return nil
	}
	return p.audio.WriteSample(media.Sample{Data: opus, Duration: d})
}

// SendControl sends a message to the browser over the control channel, or
// returns ErrNoControl before the browser has opened it.
func (p *Peer) SendControl(m protocol.Control) error {
	p.mu.Lock()
	dc := p.control
	p.mu.Unlock()
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return ErrNoControl
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

// RequestKeyframe asks the browser for a keyframe on the track it sends
// with ssrc.
func (p *Peer) RequestKeyframe(ssrc uint32) error {
	return p.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}})
}

// Bitrate is the congestion controller's current target in bits per second.
func (p *Peer) Bitrate() int { return p.bwe.GetTargetBitrate() }

// Close tears the peer connection down.
func (p *Peer) Close() error { return p.pc.Close() }

// route hands each data channel the browser opens to what its label is for.
func (p *Peer) route(dc *webrtc.DataChannel) {
	switch dc.Label() {
	case protocol.ControlChannel:
		p.attachControl(dc)
	case protocol.BulkChannel:
		p.takeBulk(dc)
	default:
		p.log.Warn("ignoring unexpected data channel", "label", dc.Label())
		dc.Close()
	}
}

// attachControl makes dc the control channel. The page opens one; a second
// replaces the first, which is closed, so one channel at a time dispatches.
func (p *Peer) attachControl(dc *webrtc.DataChannel) {
	p.mu.Lock()
	old := p.control
	p.control = dc
	p.mu.Unlock()
	if old != nil {
		p.log.Warn("the browser opened a second control channel; closing the first")
		old.Close()
	}
	dc.OnOpen(func() {
		if p.h.ControlOpen != nil {
			p.h.ControlOpen()
		}
	})
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
				p.log.Info("keyframe requested", "by", fmt.Sprintf("%T", pkt))
				if p.h.Keyframe != nil {
					p.h.Keyframe()
				}
			}
		}
	}
}

// followNomination decides whether to switch to a pair the browser nominated.
// Pion on its own only follows nominations of higher priority, but when UDP
// dies the browser fails over to the lower-priority SSH tunnel; without this
// the agent keeps sending into the dead path until ICE fails 30 s later. It
// follows only once its own path has gone quiet and the new pair has proven
// itself, so the normal startup churn of nominations is ignored.
func followNomination(m *stun.Message, pair *ice.CandidatePair, pc *webrtc.PeerConnection) bool {
	return pc != nil && m.Contains(stun.AttrUseCandidate) && pair.ResponsesReceived() > 0 &&
		pc.ICEConnectionState() == webrtc.ICEConnectionStateDisconnected
}

// opusCapability is the audio format: stereo Opus with in-band FEC, which
// every browser decodes.
var opusCapability = webrtc.RTPCodecCapability{
	MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1",
}
