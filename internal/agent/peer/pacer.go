package peer

import (
	"fmt"
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
	// and the delay it added pinned the target at its floor.
	maxPacingDelay = 250 * time.Millisecond
	// pacingTick is how often a non-empty queue is drained.
	pacingTick = 2 * time.Millisecond
)

// pacer is the congestion controller's pacer: packets go out at once
// while they fit in a small burst allowance and queue behind each other at
// pacingFactor times the target beyond it, never for longer than
// maxPacingDelay. It keeps the order it was given packets in, which the
// transport-wide sequence numbers already stamped on them follow.
type pacer struct {
	mu      sync.Mutex
	writers map[uint32]interceptor.RTPWriter
	queue   []pacedPacket
	queued  int     // bytes in queue
	rate    float64 // bytes per second
	tokens  float64 // bytes that may go out now
	refill  time.Time
	wake    chan struct{}
	done    chan struct{}
	now     func() time.Time
}

type pacedPacket struct {
	header  rtp.Header
	payload []byte
	attrs   interceptor.Attributes
	due     time.Time // when it must have gone out
}

func newPacer(initialBps int) *pacer {
	p := newPacerAt(initialBps, time.Now)
	go p.run()
	return p
}

// newPacerAt makes a pacer reading time from now, without its drain
// goroutine; tests drain it with drain.
func newPacerAt(initialBps int, now func() time.Time) *pacer {
	p := &pacer{
		writers: map[uint32]interceptor.RTPWriter{},
		wake:    make(chan struct{}, 1), done: make(chan struct{}),
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

// Write sends a packet now if nothing is queued and the allowance covers
// it, and queues a copy otherwise.
func (p *pacer) Write(header *rtp.Header, payload []byte, attrs interceptor.Attributes) (int, error) {
	size := header.MarshalSize() + len(payload)
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.writers[header.SSRC]
	if !ok {
		return 0, fmt.Errorf("peer: pacer: unknown SSRC %d", header.SSRC)
	}
	now := p.now()
	p.top(now)
	if len(p.queue) == 0 && p.tokens >= float64(size) {
		p.tokens -= float64(size)
		return w.Write(header, payload, attrs)
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

func (p *pacer) run() {
	t := time.NewTicker(pacingTick)
	defer t.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-p.wake:
		case <-t.C:
		}
		p.drain()
	}
}

// drain sends what the allowance covers from the head of the queue, and
// anything already due.
func (p *pacer) drain() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	p.top(now)
	for len(p.queue) > 0 {
		pp := &p.queue[0]
		size := pp.header.MarshalSize() + len(pp.payload)
		if p.tokens < float64(size) && now.Before(pp.due) {
			return
		}
		p.tokens = max(p.tokens-float64(size), 0)
		p.queued -= size
		if w, ok := p.writers[pp.header.SSRC]; ok {
			w.Write(&pp.header, pp.payload, pp.attrs)
		}
		p.queue[0] = pacedPacket{}
		p.queue = p.queue[1:]
	}
}

// Close stops draining; whatever is queued is dropped.
func (p *pacer) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.done:
	default:
		close(p.done)
	}
	return nil
}
