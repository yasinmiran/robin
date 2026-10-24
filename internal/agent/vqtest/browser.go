package vqtest

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/transport/v5/vnet"
	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/robin/internal/agent/yuv"
	"github.com/yasinmiran/robin/internal/protocol"
)

// Agent is the far side of the connection, as the agent's peer offers it.
type Agent interface {
	Answer(offer string) (string, error)
	AddCandidate(protocol.Candidate) error
}

// Shown is one picture the browser decoded and showed.
type Shown struct {
	// Index is the clip frame it encodes, -1 when it carried no tag.
	Index int
	At    time.Time
	Key   bool
	Bytes int
	// Recovered counts its packets that came only after a NACK.
	Recovered int
	Score     Score
}

// Request is one keyframe request the browser sent.
type Request struct {
	At  time.Time
	Why PLIReason
}

// Result is what a browser saw.
type Result struct {
	Shown        []Shown
	Requests     []Request
	Stats        ReceiverStats
	DecodeErrors int
	// NoPicture counts frames the decoder took without showing anything;
	// Untagged, pictures shown that named no clip frame.
	NoPicture, Untagged int
	// Bytes counts RTP payload bytes received, retransmissions included.
	Bytes int
}

// Browser is a stand-in for a browser's video receiver: a Pion peer that
// sends transport-wide feedback and receiver reports as Chrome does, NACKs
// and asks for keyframes with Receiver's timings, decodes with openh264
// and scores every picture it shows against the clip.
type Browser struct {
	pc     *webrtc.PeerConnection
	clip   *Clip
	dec    *Decoder
	scores chan scoreJob
	frames chan decodeJob
	// done stops poll and the candidate forwarders.
	done chan struct{}
	// readers counts the track readers, which feed frames; wg counts
	// every other goroutine.
	readers, wg sync.WaitGroup

	closeOnce sync.Once
	closeErr  error

	mu     sync.Mutex
	closed bool
	rx     *Receiver
	result Result
	ssrc   uint32
}

type decodeJob struct {
	f  Frame
	at time.Time
}

type scoreJob struct {
	shown int // index into result.Shown
	clip  int
	img   *yuv.I420
}

// NewBrowser builds the browser on net, scoring against clip.
func NewBrowser(net *vnet.Net, clip *Clip, cfg ReceiverConfig) (*Browser, error) {
	dec, err := NewDecoder()
	if err != nil {
		return nil, err
	}
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			RTCPFeedback: []webrtc.RTCPFeedback{
				{Type: webrtc.TypeRTCPFBNACK}, {Type: webrtc.TypeRTCPFBNACK, Parameter: "pli"},
			},
		},
		PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		dec.Close()
		return nil, err
	}
	reg := &interceptor.Registry{}
	if err := webrtc.ConfigureTWCCSender(m, reg); err != nil {
		dec.Close()
		return nil, err
	}
	if err := webrtc.ConfigureRTCPReports(reg); err != nil {
		dec.Close()
		return nil, err
	}
	se := webrtc.SettingEngine{}
	se.SetNet(net)
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	// Pion drops SRTP packets more than 64 behind the newest as replays,
	// which throws most retransmissions away at a few Mbit/s; Chrome keeps
	// a window of 1024 for them.
	se.SetSRTPReplayProtectionWindow(srtpReplayWindow)
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(reg), webrtc.WithSettingEngine(se)).
		NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		dec.Close()
		return nil, err
	}
	b := &Browser{
		pc: pc, clip: clip, dec: dec,
		scores: make(chan scoreJob, 64), frames: make(chan decodeJob, 256), done: make(chan struct{}),
		rx: NewReceiver(cfg, time.Now()),
	}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		pc.Close()
		dec.Close()
		return nil, err
	}
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.closed {
			return
		}
		b.ssrc = uint32(tr.SSRC())
		b.rx = NewReceiver(cfg, time.Now())
		b.readers.Add(1)
		go b.read(tr)
	})
	workers := max(runtime.NumCPU()/2, 1)
	b.wg.Add(2 + workers)
	go b.poll()
	go b.decode()
	for range workers {
		go b.score()
	}
	return b, nil
}

// Connect negotiates with a: the browser offers, as the page does, and
// candidates flow both ways until Close.
func (b *Browser) Connect(a Agent, agentCandidates <-chan protocol.Candidate) error {
	cands := make(chan webrtc.ICECandidateInit, 64)
	b.pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		select {
		case cands <- c.ToJSON():
		case <-b.done:
		}
	})
	offer, err := b.pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err := b.pc.SetLocalDescription(offer); err != nil {
		return err
	}
	answer, err := a.Answer(offer.SDP)
	if err != nil {
		return err
	}
	if err := b.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		return err
	}
	b.wg.Add(2)
	go func() {
		defer b.wg.Done()
		for {
			select {
			case <-b.done:
				return
			case c := <-cands:
				a.AddCandidate(protocol.Candidate{Candidate: c.Candidate, SDPMid: c.SDPMid, SDPMLineIndex: c.SDPMLineIndex})
			}
		}
	}()
	go func() {
		defer b.wg.Done()
		for {
			select {
			case <-b.done:
				return
			case c := <-agentCandidates:
				if c.Candidate != "" {
					b.pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: c.Candidate, SDPMid: c.SDPMid, SDPMLineIndex: c.SDPMLineIndex})
				}
			}
		}
	}()
	return nil
}

func (b *Browser) read(tr *webrtc.TrackRemote) {
	defer b.readers.Done()
	for {
		p, _, err := tr.ReadRTP()
		if err != nil {
			return
		}
		now := time.Now()
		b.mu.Lock()
		b.result.Bytes += len(p.Payload)
		frames := b.rx.Push(Packet{Seq: p.SequenceNumber, Timestamp: p.Timestamp, Marker: p.Marker, Payload: p.Payload}, now)
		b.mu.Unlock()
		for _, f := range frames {
			b.frames <- decodeJob{f, now}
		}
	}
}

// srtpReplayWindow is the SRTP replay window libwebrtc gives video.
const srtpReplayWindow = 1024

// pollEvery is how often the receiver's NACK and keyframe timers are run.
const pollEvery = 10 * time.Millisecond

func (b *Browser) poll() {
	defer b.wg.Done()
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		select {
		case <-b.done:
			return
		case now := <-t.C:
			b.mu.Lock()
			nack, pli := b.rx.Poll(now)
			ssrc := b.ssrc
			if pli != "" {
				b.result.Requests = append(b.result.Requests, Request{At: now, Why: pli})
			}
			b.mu.Unlock()
			var pkts []rtcp.Packet
			if len(nack) > 0 {
				pkts = append(pkts, &rtcp.TransportLayerNack{MediaSSRC: ssrc, Nacks: rtcp.NackPairsFromSequenceNumbers(nack)})
			}
			if pli != "" {
				pkts = append(pkts, &rtcp.PictureLossIndication{MediaSSRC: ssrc})
			}
			if len(pkts) > 0 && ssrc != 0 {
				b.pc.WriteRTCP(pkts)
			}
		}
	}
}

func (b *Browser) decode() {
	defer b.wg.Done()
	defer close(b.scores)
	for job := range b.frames {
		var au []byte
		var depay codecs.H264Packet
		bytes := 0
		for _, p := range job.f.Payloads {
			bytes += len(p)
			nal, err := depay.Unmarshal(p)
			if err == nil {
				au = append(au, nal...)
			}
		}
		img, err := b.dec.Decode(au)
		shownAt := time.Now()
		b.mu.Lock()
		if err != nil {
			b.result.DecodeErrors++
			b.rx.DecodeFailed()
			b.mu.Unlock()
			continue
		}
		if img == nil {
			b.result.NoPicture++
			b.mu.Unlock()
			continue
		}
		index, ok := FrameIndex(au)
		if !ok {
			index = -1
			b.result.Untagged++
		}
		b.result.Shown = append(b.result.Shown, Shown{
			Index: index, At: shownAt, Key: job.f.Key, Bytes: bytes, Recovered: job.f.Recovered,
		})
		n := len(b.result.Shown) - 1
		b.mu.Unlock()
		if ok && b.clip != nil && index >= 0 && index < b.clip.Len() {
			b.scores <- scoreJob{shown: n, clip: index, img: img}
		}
	}
}

func (b *Browser) score() {
	defer b.wg.Done()
	var pix []byte
	var src *yuv.I420
	for job := range b.scores {
		if pix == nil {
			pix = make([]byte, 4*b.clip.W*b.clip.H)
			src = yuv.NewI420(b.clip.W, b.clip.H)
		}
		b.clip.Draw(pix, job.clip)
		yuv.FromBGRX(src, pix, 4*b.clip.W)
		s := Compare(src.Y, src.Width, src.Height, job.img.Y, job.img.Width, job.img.Height)
		b.mu.Lock()
		b.result.Shown[job.shown].Score = s
		b.mu.Unlock()
	}
}

// Close disconnects, decodes and scores every frame already received and
// returns what the browser saw. Later calls return the same.
func (b *Browser) Close() (Result, error) {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.mu.Unlock()
		b.closeErr = b.pc.Close()
		// The readers end with the connection; the decoder then runs
		// out of frames and the scorers out of pictures.
		b.readers.Wait()
		close(b.frames)
		close(b.done)
		b.wg.Wait()
		b.dec.Close()
		b.mu.Lock()
		b.result.Stats = b.rx.Stats
		b.mu.Unlock()
	})
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.result, b.closeErr
}

// errNoVideo is returned when nothing was shown before the deadline.
var errNoVideo = errors.New("vqtest: no picture shown")

// WaitShown waits until the browser has shown a picture.
func (b *Browser) WaitShown(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		n := len(b.result.Shown)
		b.mu.Unlock()
		if n > 0 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("%w within %v", errNoVideo, timeout)
}
