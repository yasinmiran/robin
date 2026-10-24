package peer

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

// Pacing limits. A keyframe of a 1080p desktop is 100 to 200 KB, which
// sent at once overflows a bottleneck that buffers 100 ms of 4 Mbit/s:
// the packets past the buffer are lost together, more than the bounded
// NACK responder repairs, and the browser stalls until it asks for another
// keyframe, which overflows the buffer again.
const (
	// pacingFactor times the congestion controller's target is the rate
	// a burst drains at.
	pacingFactor = 1.5
	// pacingBurst of that rate goes out at once, so a frame of ordinary
	// size is not delayed at all.
	pacingBurst = 20 * time.Millisecond
	// maxPacingDelay bounds how long a packet waits: when the queue holds
	// more than that at the pacing rate, it drains faster, so that its
	// last packet leaves in time. Pion's own pacer queues without bound,
	// and the delay it added pinned the target at its floor. Over the
	// tight link of the agent's quality matrix, 250 ms still overflowed
	// the buffer with keyframes and 1 s held scrolling back by most of a
	// second.
	maxPacingDelay = 500 * time.Millisecond
	// pacingTick is how often a non-empty queue is drained.
	pacingTick = 2 * time.Millisecond
)

// errPacerClosed is what Write returns once the pacer is closed.
var errPacerClosed = errors.New("peer: pacer: closed")

// pacer is the congestion controller's pacer: packets go out at once
// while they fit in a small burst allowance and queue behind each other at
// pacingFactor times the target beyond it, never for longer than
// maxPacingDelay. It keeps the order it was given packets in, which the
// transport-wide sequence numbers already stamped on them follow, except
// for the streams it was told to prioritize: their packets go out at once
// and are charged to the allowance, so the queue behind them waits that
// much longer. Packets then leave out of the order they were numbered in,
// so the pacer numbers them again as they leave; see transportSeqs.
type pacer struct {
	mu       sync.Mutex
	writers  map[uint32]interceptor.RTPWriter
	priority map[uint32]bool
	// transportCC is the ID of each stream's transport-wide sequence
	// number extension.
	transportCC map[uint32]uint8
	seqs        transportSeqs
	queue       []pacedPacket
	queued      int     // bytes in queue
	rate        float64 // bytes per second
	tokens      float64 // bytes that may go out now
	refill      time.Time
	wake        chan struct{}
	done        chan struct{}
	// stopped is closed when run returns; nil for a pacer without it.
	stopped chan struct{}
	now     func() time.Time
}

type pacedPacket struct {
	header  rtp.Header
	payload []byte
	attrs   interceptor.Attributes
	due     time.Time // when it must have gone out
}

func newPacer(initialBps int) *pacer {
	return newPacerAt(initialBps, time.Now).start()
}

// newPacerAt makes a pacer reading time from now, without its drain
// goroutine; tests drain it with drain, or start it.
func newPacerAt(initialBps int, now func() time.Time) *pacer {
	p := &pacer{
		writers: map[uint32]interceptor.RTPWriter{}, priority: map[uint32]bool{}, transportCC: map[uint32]uint8{},
		wake: make(chan struct{}, 1), done: make(chan struct{}),
		now: now, refill: now(),
	}
	p.SetTargetBitrate(initialBps)
	p.tokens = p.burst()
	return p
}

// SetTargetBitrate follows the congestion controller's target.
func (p *pacer) SetTargetBitrate(bps int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.top(p.now())
	p.rate = pacingFactor * float64(bps) / 8
}

// start runs the drain goroutine, which Close stops.
func (p *pacer) start() *pacer {
	p.stopped = make(chan struct{})
	go p.run()
	return p
}

func (p *pacer) burst() float64 { return p.rate * pacingBurst.Seconds() }

// top refills the burst allowance for the time since the last refill, at
// the pacing rate or, when that would keep the last queued packet past its
// due time, the rate that sends it in time.
func (p *pacer) top(now time.Time) {
	rate := p.rate
	if n := len(p.queue); n > 0 {
		rate = max(rate, float64(p.queued)/max(p.queue[n-1].due.Sub(now), pacingTick).Seconds())
	}
	p.tokens = min(p.tokens+rate*now.Sub(p.refill).Seconds(), max(p.burst(), p.tokens))
	p.refill = now
}

// AddStream registers where a stream's packets go.
func (p *pacer) AddStream(ssrc uint32, writer interceptor.RTPWriter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writers[ssrc] = writer
}

// Prioritize sends ssrc's packets ahead of any queue.
func (p *pacer) Prioritize(ssrc uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.priority[ssrc] = true
}

// SetTransportCC says which header extension of ssrc's packets carries the
// transport-wide sequence number.
func (p *pacer) SetTransportCC(ssrc uint32, id uint8) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.transportCC[ssrc] = id
}

// transportSeq is the transport-wide sequence number h carries, if its
// stream has one.
func (p *pacer) transportSeq(h *rtp.Header) (uint8, uint16, bool) {
	id := p.transportCC[h.SSRC]
	if id == 0 {
		return 0, 0, false
	}
	var ext rtp.TransportCCExtension
	if b := h.GetExtension(id); b == nil || ext.Unmarshal(b) != nil {
		return 0, 0, false
	}
	return id, ext.TransportSequence, true
}

// send writes a packet out, numbered next, with p.mu held.
func (p *pacer) send(w interceptor.RTPWriter, h *rtp.Header, payload []byte, attrs interceptor.Attributes) (int, error) {
	if id, _, ok := p.transportSeq(h); ok {
		b, err := rtp.TransportCCExtension{TransportSequence: p.seqs.next()}.Marshal()
		if err == nil {
			err = h.SetExtension(id, b)
		}
		if err != nil {
			return 0, err
		}
	}
	return w.Write(h, payload, attrs)
}

// Write sends a packet now if its stream has priority, or if nothing is
// queued and the allowance covers it, and queues a copy otherwise.
func (p *pacer) Write(header *rtp.Header, payload []byte, attrs interceptor.Attributes) (int, error) {
	size := header.MarshalSize() + len(payload)
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.done:
		return 0, errPacerClosed
	default:
	}
	w, ok := p.writers[header.SSRC]
	if !ok {
		return 0, fmt.Errorf("peer: pacer: unknown SSRC %d", header.SSRC)
	}
	if _, seq, ok := p.transportSeq(header); ok {
		p.seqs.add(seq)
	}
	now := p.now()
	p.top(now)
	if p.priority[header.SSRC] {
		p.tokens -= float64(size)
		return p.send(w, header, payload, attrs)
	}
	if len(p.queue) == 0 && p.tokens >= float64(size) {
		p.tokens -= float64(size)
		return p.send(w, header, payload, attrs)
	}
	p.queue = append(p.queue, pacedPacket{
		header: header.Clone(), payload: append([]byte(nil), payload...), attrs: attrs, due: now.Add(maxPacingDelay),
	})
	p.queued += size
	select {
	case p.wake <- struct{}{}:
	default:
	}
	return size, nil
}

// run drains the queue every pacingTick while it holds packets, and sleeps
// until Write queues one otherwise.
func (p *pacer) run() {
	defer close(p.stopped)
	t := time.NewTicker(pacingTick)
	t.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-p.wake:
		}
		t.Reset(pacingTick)
		for p.drain() {
			select {
			case <-p.done:
				t.Stop()
				return
			case <-t.C:
			}
		}
		t.Stop()
	}
}

// drain sends what the allowance covers from the head of the queue, and
// anything already due. It reports whether packets are left queued.
func (p *pacer) drain() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	p.top(now)
	for len(p.queue) > 0 {
		pp := &p.queue[0]
		size := pp.header.MarshalSize() + len(pp.payload)
		if p.tokens < float64(size) && now.Before(pp.due) {
			return true
		}
		p.tokens = max(p.tokens-float64(size), 0)
		p.queued -= size
		if w, ok := p.writers[pp.header.SSRC]; ok {
			p.send(w, &pp.header, pp.payload, pp.attrs)
		}
		p.queue[0] = pacedPacket{}
		p.queue = p.queue[1:]
	}
	return false
}

// Close stops the pacer and returns once nothing more will go out:
// whatever is queued is dropped, and a later Write fails.
func (p *pacer) Close() error {
	p.mu.Lock()
	select {
	case <-p.done:
	default:
		close(p.done)
		p.queue, p.queued, p.seqs = nil, 0, transportSeqs{}
	}
	p.mu.Unlock()
	if p.stopped != nil {
		<-p.stopped
	}
	return nil
}

// transportSeqs holds the transport-wide sequence numbers stamped on the
// packets the pacer has been given and not yet sent, and hands them out
// smallest first, so that packets leave in the order of their numbers
// whatever order the pacer sends them in. The browser's congestion
// feedback reports a packet that arrives after a higher number as lost:
// with sound sent ahead of a queued keyframe, GCC read half its packets as
// lost on a link that lost none, and its target fell to a tenth.
type transportSeqs struct{ pending []uint16 }

// seqBefore orders sequence numbers across their wrap.
func seqBefore(a, b uint16) bool { return int16(a-b) < 0 }

func (s *transportSeqs) add(seq uint16) {
	i := len(s.pending)
	for i > 0 && seqBefore(seq, s.pending[i-1]) {
		i--
	}
	s.pending = slices.Insert(s.pending, i, seq)
}

// next takes the smallest number, which add must have been given.
func (s *transportSeqs) next() uint16 {
	seq := s.pending[0]
	s.pending = s.pending[1:]
	return seq
}
