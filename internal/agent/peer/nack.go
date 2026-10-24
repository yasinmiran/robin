package peer

import (
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// Retransmission limits. Packets older than nackMaxAge would arrive too late
// to help a live desktop, and an unbounded responder turns a burst of loss
// into a retransmission storm: everything lost gets resent, mostly lost
// again, and congestion control reads the storm as permanent heavy loss.
//
// The history holds packets by count and is read by age, so it has to
// hold nackMaxAge at the highest rate, about 1000 packets a second at
// 10 Mbit/s: Chrome asks for a packet again every 100 ms for about a
// second.
const (
	nackHistory     = 4096 // packets kept per stream; a power of two
	nackMaxAge      = time.Second
	nackMaxResends  = 2
	nackBudgetShare = 0.25 // of the congestion controller's target
	nackBurst       = 100 * time.Millisecond
)

// nackResponderFactory builds the responder for each peer connection.
// budget reports the current target bitrate in bits per second. lost, if
// set, is called when a NACK names a packet too old to send again: the
// browser cannot decode past it until a keyframe, and asks for one only
// after 3 s without a frame.
type nackResponderFactory struct {
	budget func() int
	lost   func()
}

func (f *nackResponderFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	return &nackResponder{budget: f.budget, lost: f.lost, streams: map[uint32]*sendHistory{}}, nil
}

// nackResponder answers RTCP NACKs from a short history of sent packets,
// within a share of the current bitrate.
type nackResponder struct {
	interceptor.NoOp
	budget func() int
	lost   func()

	mu      sync.Mutex
	streams map[uint32]*sendHistory
	tokens  float64 // bits
	refill  time.Time
}

type sentPacket struct {
	header  rtp.Header
	payload []byte
	sentAt  time.Time
	resends int
}

type sendHistory struct {
	writer  interceptor.RTPWriter
	packets [nackHistory]*sentPacket
}

func (r *nackResponder) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	if !wantsNack(info) {
		return writer
	}
	h := &sendHistory{writer: writer}
	r.mu.Lock()
	r.streams[info.SSRC] = h
	r.mu.Unlock()
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, a interceptor.Attributes) (int, error) {
		p := &sentPacket{header: header.Clone(), payload: append([]byte(nil), payload...), sentAt: time.Now()}
		r.mu.Lock()
		h.packets[header.SequenceNumber%nackHistory] = p
		r.mu.Unlock()
		return writer.Write(header, payload, a)
	})
}

func (r *nackResponder) UnbindLocalStream(info *interceptor.StreamInfo) {
	r.mu.Lock()
	delete(r.streams, info.SSRC)
	r.mu.Unlock()
}

func (r *nackResponder) BindRTCPReader(reader interceptor.RTCPReader) interceptor.RTCPReader {
	return interceptor.RTCPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
		n, a, err := reader.Read(b, a)
		if err != nil {
			return 0, nil, err
		}
		if a == nil {
			a = interceptor.Attributes{}
		}
		pkts, err := a.GetRTCPPackets(b[:n])
		if err != nil {
			return 0, nil, err
		}
		for _, p := range pkts {
			if nack, ok := p.(*rtcp.TransportLayerNack); ok {
				r.resend(nack, time.Now())
			}
		}
		return n, a, nil
	})
}

func (r *nackResponder) resend(nack *rtcp.TransportLayerNack, now time.Time) {
	r.mu.Lock()
	h := r.streams[nack.MediaSSRC]
	if h == nil {
		r.mu.Unlock()
		return
	}
	rate := nackBudgetShare * float64(r.budget())
	if !r.refill.IsZero() {
		r.tokens = min(r.tokens+rate*now.Sub(r.refill).Seconds(), rate*nackBurst.Seconds())
	}
	r.refill = now
	var due []*sentPacket
	gone := false
	for _, pair := range nack.Nacks {
		for _, seq := range pair.PacketList() {
			p := h.packets[seq%nackHistory]
			if p == nil || p.header.SequenceNumber != seq || now.Sub(p.sentAt) > nackMaxAge {
				gone = true
				continue
			}
			if p.resends >= nackMaxResends {
				continue
			}
			cost := float64(8 * (p.header.MarshalSize() + len(p.payload)))
			if r.tokens < cost {
				continue
			}
			r.tokens -= cost
			p.resends++
			due = append(due, p)
		}
	}
	r.mu.Unlock()
	for _, p := range due {
		hdr := p.header
		h.writer.Write(&hdr, p.payload, nil)
	}
	if gone && r.lost != nil {
		r.lost()
	}
}

func wantsNack(info *interceptor.StreamInfo) bool {
	for _, fb := range info.RTCPFeedback {
		if fb.Type == "nack" && fb.Parameter == "" {
			return true
		}
	}
	return false
}
