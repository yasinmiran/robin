package vqtest

import (
	"slices"
	"time"
)

// ReceiverConfig holds the timings of a browser's video receiver.
type ReceiverConfig struct {
	// NackInterval spaces repeated NACKs for one packet. Chrome waits one
	// round trip, and a receive-only peer, which gets no report blocks of
	// its own, assumes 100 ms.
	NackInterval time.Duration
	// MaxNackRetries is how often one packet is asked for before the
	// receiver gives up on it.
	MaxNackRetries int
	// MaxNackList is the most missing packets tracked; more asks for a
	// keyframe instead.
	MaxNackList int
	// MaxBuffered is the most packets held waiting for a gap to fill;
	// more clears the buffer and asks for a keyframe.
	MaxBuffered int
	// FrameTimeout is how long without a decodable frame before asking for
	// a keyframe; KeyframeTimeout replaces it while one is required.
	FrameTimeout, KeyframeTimeout time.Duration
	// InactiveAfter stops the timeouts asking once no packet has arrived
	// for that long.
	InactiveAfter time.Duration
}

// Chrome is how Chrome's WebRTC video receiver (libwebrtc's NackRequester,
// H26xPacketBuffer and VideoReceiveStream2) times NACKs and keyframe
// requests.
var Chrome = ReceiverConfig{
	NackInterval:    100 * time.Millisecond,
	MaxNackRetries:  10,
	MaxNackList:     1000,
	MaxBuffered:     2048,
	FrameTimeout:    3 * time.Second,
	KeyframeTimeout: 200 * time.Millisecond,
	InactiveAfter:   5 * time.Second,
}

// PLIReason is why the receiver asked for a keyframe; "" means it did
// not.
type PLIReason string

const (
	// PLIIdle means no frame came for FrameTimeout and none is waiting on
	// a gap: the sender sent nothing.
	PLIIdle PLIReason = "idle"
	// PLIStalled means no frame came for FrameTimeout because packets are
	// held behind one that never came.
	PLIStalled PLIReason = "stalled on loss"
	// PLIKeyframeRequired means no keyframe came within KeyframeTimeout
	// while one is required, at the start or after a decode error.
	PLIKeyframeRequired PLIReason = "keyframe required"
	// PLINackOverflow means more packets went missing than MaxNackList.
	PLINackOverflow PLIReason = "nack list overflow"
	// PLIBufferFull means more packets were held than MaxBuffered.
	PLIBufferFull PLIReason = "packet buffer full"
)

// Packet is the part of an RTP packet the receiver looks at.
type Packet struct {
	Seq       uint16
	Timestamp uint32
	Marker    bool
	Payload   []byte
}

// Frame is a complete access unit, handed out only once every packet
// before it has been handed out too or a keyframe started afresh.
type Frame struct {
	Timestamp uint32
	Key       bool
	// Payloads are the frame's RTP payloads in order.
	Payloads [][]byte
	// Recovered counts packets that arrived only after being NACKed.
	Recovered int
}

// ReceiverStats count what the receiver saw and did.
type ReceiverStats struct {
	Packets, Duplicates int
	// Recovered packets arrived after the receiver had noticed them
	// missing; Abandoned ones were asked for MaxNackRetries times and
	// never came.
	Recovered, Abandoned int
	// NACKed counts sequence numbers asked for, repeats included.
	NACKed            int
	Frames, Keyframes int
	PLIs              map[PLIReason]int
}

type stored struct {
	Packet
	recovered bool
}

type nackEntry struct {
	sentAt  time.Time
	retries int
}

// Receiver models a browser's video receiver: it NACKs gaps, holds frames
// behind a gap until it fills, restarts on a keyframe, and asks for one
// the way Chrome does. It is not safe for concurrent use.
type Receiver struct {
	cfg   ReceiverConfig
	Stats ReceiverStats

	ext      int64 // last unwrapped sequence number, for unwrapping
	started  bool
	highest  int64
	buf      map[int64]*stored
	keys     []int64 // sequence numbers of packets that start a keyframe
	next     int64   // where the next frame must start
	haveNext bool
	nacks    map[int64]*nackEntry

	keyRequired   bool
	pending       PLIReason // a keyframe request to send at the next Poll
	since         time.Time
	lastPacket    time.Time
	lastKeyPacket time.Time
}

// NewReceiver returns a receiver that starts waiting at now, needing a
// keyframe.
func NewReceiver(cfg ReceiverConfig, now time.Time) *Receiver {
	return &Receiver{
		cfg: cfg, Stats: ReceiverStats{PLIs: map[PLIReason]int{}},
		buf: map[int64]*stored{}, nacks: map[int64]*nackEntry{},
		keyRequired: true, since: now,
	}
}

// unwrap extends a 16-bit sequence number to the one nearest the last.
func (r *Receiver) unwrap(seq uint16) int64 {
	if !r.started {
		r.ext = int64(seq)
		return r.ext
	}
	d := int64(int16(seq - uint16(r.ext)))
	r.ext += d
	return r.ext
}

// Push takes one packet arriving at now and returns the frames it made
// decodable, in order.
func (r *Receiver) Push(p Packet, now time.Time) []Frame {
	s := r.unwrap(p.Seq)
	r.Stats.Packets++
	r.lastPacket = now
	if isKeyPacket(p.Payload) {
		r.lastKeyPacket = now
	}
	if !r.started {
		r.started = true
		r.highest = s - 1
	}
	recovered := false
	switch {
	case s > r.highest:
		for q := r.highest + 1; q < s; q++ {
			r.nacks[q] = &nackEntry{}
		}
		r.highest = s
		if len(r.nacks) > r.cfg.MaxNackList {
			clear(r.nacks)
			r.request(PLINackOverflow)
		}
	case r.buf[s] != nil || (r.haveNext && s < r.next):
		r.Stats.Duplicates++
		return nil
	default:
		if _, ok := r.nacks[s]; ok {
			delete(r.nacks, s)
			r.Stats.Recovered++
			recovered = true
		}
	}
	r.buf[s] = &stored{Packet: p, recovered: recovered}
	if startsKeyframe(p.Payload) {
		i, _ := slices.BinarySearch(r.keys, s)
		r.keys = slices.Insert(r.keys, i, s)
	}
	if len(r.buf) > r.cfg.MaxBuffered {
		clear(r.buf)
		r.keys = r.keys[:0]
		r.haveNext = false
		r.request(PLIBufferFull)
		return nil
	}
	return r.assemble(now)
}

func (r *Receiver) request(why PLIReason) {
	if r.pending == "" {
		r.pending = why
	}
}

// assemble hands out every frame that is now complete and continuous,
// jumping to a complete keyframe when the stream is stuck behind a gap.
func (r *Receiver) assemble(now time.Time) []Frame {
	var out []Frame
	for {
		if r.haveNext {
			if f, last, ok := r.frameAt(r.next); ok {
				r.consume(r.next, last)
				out = append(out, f)
				r.Stats.Frames++
				if f.Key {
					r.Stats.Keyframes++
					r.keyRequired = false
				}
				r.since = now
				continue
			}
		}
		k, ok := r.completeKeyframe()
		if !ok {
			return out
		}
		r.drop(k)
		r.next, r.haveNext = k, true
	}
}

// frameAt returns the frame whose first packet is start, if all of its
// packets are here, and its last sequence number.
func (r *Receiver) frameAt(start int64) (Frame, int64, bool) {
	first := r.buf[start]
	if first == nil {
		return Frame{}, 0, false
	}
	f := Frame{Timestamp: first.Timestamp}
	for q := start; ; q++ {
		p := r.buf[q]
		if p == nil || p.Timestamp != first.Timestamp {
			return Frame{}, 0, false
		}
		f.Payloads = append(f.Payloads, p.Payload)
		f.Key = f.Key || isKeyPacket(p.Payload)
		if p.recovered {
			f.Recovered++
		}
		if p.Marker {
			return f, q, true
		}
	}
}

// completeKeyframe finds the earliest keyframe past the current position
// whose packets have all arrived.
func (r *Receiver) completeKeyframe() (int64, bool) {
	for _, k := range r.keys {
		if r.haveNext && k <= r.next {
			continue
		}
		if _, _, ok := r.frameAt(k); ok {
			return k, true
		}
	}
	return 0, false
}

// consume removes a handed-out frame's packets and moves past them.
func (r *Receiver) consume(from, to int64) {
	for q := from; q <= to; q++ {
		delete(r.buf, q)
	}
	r.next = to + 1
	r.drop(r.next)
}

// drop forgets every packet, keyframe start and NACK before seq: whatever
// was held behind a gap is lost once a keyframe moves past it.
func (r *Receiver) drop(seq int64) {
	for q := range r.buf {
		if q < seq {
			delete(r.buf, q)
		}
	}
	for q := range r.nacks {
		if q < seq {
			delete(r.nacks, q)
		}
	}
	i, _ := slices.BinarySearch(r.keys, seq)
	r.keys = r.keys[i:]
}

// Poll returns the sequence numbers to NACK now and, when the receiver
// asks for a keyframe now, why.
func (r *Receiver) Poll(now time.Time) (nack []uint16, pli PLIReason) {
	for _, q := range sortedKeys(r.nacks) {
		e := r.nacks[q]
		if e.retries >= r.cfg.MaxNackRetries {
			delete(r.nacks, q)
			r.Stats.Abandoned++
			continue
		}
		if e.sentAt.IsZero() || now.Sub(e.sentAt) >= r.cfg.NackInterval {
			e.sentAt = now
			e.retries++
			r.Stats.NACKed++
			nack = append(nack, uint16(q))
		}
	}
	if pli, r.pending = r.pending, ""; pli != "" {
		r.Stats.PLIs[pli]++
		return nack, pli
	}
	timeout := r.cfg.FrameTimeout
	if r.keyRequired {
		timeout = r.cfg.KeyframeTimeout
	}
	if now.Sub(r.since) < timeout {
		return nack, ""
	}
	r.since = now
	active := !r.lastPacket.IsZero() && now.Sub(r.lastPacket) < r.cfg.InactiveAfter
	receivingKey := !r.lastKeyPacket.IsZero() && now.Sub(r.lastKeyPacket) < r.cfg.KeyframeTimeout
	if !active || receivingKey {
		return nack, ""
	}
	switch {
	case r.keyRequired:
		pli = PLIKeyframeRequired
	case len(r.buf) > 0:
		pli = PLIStalled
	default:
		pli = PLIIdle
	}
	r.Stats.PLIs[pli]++
	return nack, pli
}

// DecodeFailed tells the receiver a frame it handed out did not decode,
// so it needs a keyframe.
func (r *Receiver) DecodeFailed() { r.keyRequired = true }

func sortedKeys[V any](m map[int64]V) []int64 {
	ks := make([]int64, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// NAL unit types the receiver looks for.
const (
	nalIDR   = 5
	nalSEI   = 6
	nalSPS   = 7
	nalPPS   = 8
	nalSTAPA = 24
	nalFUA   = 28
)

// nalTypes lists the NAL unit types an H.264 RTP payload carries.
func nalTypes(payload []byte) (types []byte) {
	if len(payload) == 0 {
		return nil
	}
	switch t := payload[0] & 0x1f; t {
	case nalSTAPA:
		for b := payload[1:]; len(b) >= 3; {
			n := int(b[0])<<8 | int(b[1])
			if n == 0 || 2+n > len(b) {
				break
			}
			types = append(types, b[2]&0x1f)
			b = b[2+n:]
		}
		return types
	case nalFUA:
		if len(payload) < 2 {
			return nil
		}
		return []byte{payload[1] & 0x1f}
	default:
		return []byte{t}
	}
}

// isKeyPacket reports whether a payload carries part of a keyframe.
func isKeyPacket(payload []byte) bool {
	for _, t := range nalTypes(payload) {
		if t == nalIDR || t == nalSPS || t == nalPPS {
			return true
		}
	}
	return false
}

// startsKeyframe reports whether a payload is where a keyframe can start:
// its parameter sets, which every IDR here is sent behind.
func startsKeyframe(payload []byte) bool {
	return slices.Contains(nalTypes(payload), nalSPS)
}
