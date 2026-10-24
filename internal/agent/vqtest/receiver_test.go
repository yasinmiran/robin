package vqtest

import (
	"slices"
	"testing"
	"time"
)

// stream builds RTP packets for a sequence of frames the way the agent's
// track sends them: a keyframe as its parameter sets in a STAP-A and the
// IDR in FU-A fragments, a delta frame as one slice.
type stream struct {
	seq uint16
	ts  uint32
}

func (s *stream) packet(payload []byte, marker bool) Packet {
	p := Packet{Seq: s.seq, Timestamp: s.ts, Marker: marker, Payload: payload}
	s.seq++
	return p
}

func (s *stream) keyframe() []Packet {
	defer func() { s.ts += 3000 }()
	return []Packet{
		s.packet([]byte{0x78, 0, 2, 0x67, 1, 0, 2, 0x68, 1}, false),
		s.packet([]byte{0x7c, 0x85, 1, 2, 3}, false),
		s.packet([]byte{0x7c, 0x45, 4, 5, 6}, true),
	}
}

func (s *stream) delta() []Packet {
	defer func() { s.ts += 3000 }()
	return []Packet{s.packet([]byte{0x41, 9, 9}, true)}
}

type clock struct{ now time.Time }

func (c *clock) tick(d time.Duration) time.Time {
	c.now = c.now.Add(d)
	return c.now
}

func push(r *Receiver, c *clock, pkts ...Packet) []Frame {
	var out []Frame
	for _, p := range pkts {
		out = append(out, r.Push(p, c.tick(time.Millisecond))...)
	}
	return out
}

func TestReceiverHandsOutFramesInOrder(t *testing.T) {
	c := &clock{now: time.Unix(0, 0)}
	r := NewReceiver(Chrome, c.now)
	var s stream
	frames := push(r, c, append(append(s.keyframe(), s.delta()...), s.delta()...)...)
	if len(frames) != 3 || !frames[0].Key || frames[1].Key || frames[2].Key {
		t.Fatalf("got %d frames: %+v", len(frames), frames)
	}
	if len(frames[0].Payloads) != 3 || frames[1].Timestamp != 3000 {
		t.Fatalf("keyframe has %d payloads, second frame timestamp %d", len(frames[0].Payloads), frames[1].Timestamp)
	}
	if nack, pli := r.Poll(c.tick(time.Millisecond)); nack != nil || pli != "" {
		t.Fatalf("Poll asked for %v and %q with nothing missing", nack, pli)
	}
	if r.Stats.Frames != 3 || r.Stats.Keyframes != 1 || r.Stats.Packets != 5 {
		t.Fatalf("stats %+v", r.Stats)
	}
}

func TestReceiverNacksAGapAndHoldsFramesBehindIt(t *testing.T) {
	c := &clock{now: time.Unix(0, 0)}
	r := NewReceiver(Chrome, c.now)
	var s stream
	push(r, c, s.keyframe()...)
	lost := s.delta()
	after := append(s.delta(), s.delta()...)
	if frames := push(r, c, after...); len(frames) != 0 {
		t.Fatalf("%d frames handed out across a gap", len(frames))
	}
	nack, pli := r.Poll(c.tick(time.Millisecond))
	if !slices.Equal(nack, []uint16{lost[0].Seq}) || pli != "" {
		t.Fatalf("Poll: nack %v, pli %q", nack, pli)
	}
	// Asked again only once NackInterval has passed.
	if nack, _ := r.Poll(c.tick(50 * time.Millisecond)); nack != nil {
		t.Fatalf("NACKed again after 50 ms: %v", nack)
	}
	if nack, _ := r.Poll(c.tick(50 * time.Millisecond)); len(nack) != 1 {
		t.Fatalf("not NACKed again after 100 ms: %v", nack)
	}
	frames := push(r, c, lost...)
	if len(frames) != 3 || frames[0].Recovered != 1 {
		t.Fatalf("after the retransmission: %+v", frames)
	}
	if r.Stats.Recovered != 1 || r.Stats.NACKed != 2 {
		t.Fatalf("stats %+v", r.Stats)
	}
	if frames := push(r, c, lost...); len(frames) != 0 || r.Stats.Duplicates != 1 {
		t.Fatalf("a duplicate made %d frames, %d duplicates counted", len(frames), r.Stats.Duplicates)
	}
}

func TestReceiverGivesUpAndStallsUntilAKeyframe(t *testing.T) {
	c := &clock{now: time.Unix(0, 0)}
	r := NewReceiver(Chrome, c.now)
	var s stream
	push(r, c, s.keyframe()...)
	s.delta() // lost for good
	push(r, c, s.delta()...)
	var asked int
	var pli PLIReason
	for range 30 {
		push(r, c, s.delta()...) // a frame a tick keeps the stream active
		var nack []uint16
		nack, pli = r.Poll(c.tick(100 * time.Millisecond))
		asked += len(nack)
		if pli != "" {
			break
		}
	}
	if asked != Chrome.MaxNackRetries || r.Stats.Abandoned != 1 {
		t.Fatalf("asked %d times, abandoned %d", asked, r.Stats.Abandoned)
	}
	if pli != PLIStalled {
		t.Fatalf("keyframe request %q, want %q", pli, PLIStalled)
	}
	frames := push(r, c, s.keyframe()...)
	if len(frames) != 1 || !frames[0].Key {
		t.Fatalf("a keyframe after the gap gave %+v", frames)
	}
	if frames := push(r, c, s.delta()...); len(frames) != 1 {
		t.Fatalf("the frame after the keyframe gave %d frames", len(frames))
	}
	if r.Stats.PLIs[PLIStalled] != 1 {
		t.Fatalf("PLIs %v", r.Stats.PLIs)
	}
}

func TestReceiverAsksForAKeyframeWhenIdle(t *testing.T) {
	c := &clock{now: time.Unix(0, 0)}
	r := NewReceiver(Chrome, c.now)
	var s stream
	push(r, c, s.keyframe()...)
	if _, pli := r.Poll(c.tick(2900 * time.Millisecond)); pli != "" {
		t.Fatalf("asked %q after 2.9 s", pli)
	}
	if _, pli := r.Poll(c.tick(100 * time.Millisecond)); pli != PLIIdle {
		t.Fatalf("after 3 s of nothing: %q, want %q", pli, PLIIdle)
	}
	// The sender answers, and 3 s later the receiver asks again.
	push(r, c, s.keyframe()...)
	if _, pli := r.Poll(c.tick(3 * time.Second)); pli != PLIIdle {
		t.Fatalf("second request %q", pli)
	}
	// Silence for longer than InactiveAfter stops the requests.
	if _, pli := r.Poll(c.tick(3 * time.Second)); pli != "" {
		t.Fatalf("asked %q of a sender silent for 6 s", pli)
	}
	if r.Stats.PLIs[PLIIdle] != 2 {
		t.Fatalf("PLIs %v", r.Stats.PLIs)
	}
}

func TestReceiverNeedsAKeyframeFirst(t *testing.T) {
	c := &clock{now: time.Unix(0, 0)}
	r := NewReceiver(Chrome, c.now)
	var s stream
	if frames := push(r, c, s.delta()...); len(frames) != 0 {
		t.Fatalf("a delta frame first gave %d frames", len(frames))
	}
	if _, pli := r.Poll(c.tick(200 * time.Millisecond)); pli != PLIKeyframeRequired {
		t.Fatalf("got %q, want %q", pli, PLIKeyframeRequired)
	}
	// A keyframe still arriving holds the next request back.
	key := s.keyframe()
	c.tick(100 * time.Millisecond)
	push(r, c, key[:2]...)
	if _, pli := r.Poll(c.tick(110 * time.Millisecond)); pli != "" {
		t.Fatalf("asked %q while a keyframe was arriving", pli)
	}
	if frames := push(r, c, key[2]); len(frames) != 1 || !frames[0].Key {
		t.Fatalf("got %+v", frames)
	}
	r.DecodeFailed()
	if _, pli := r.Poll(c.tick(200 * time.Millisecond)); pli != PLIKeyframeRequired {
		t.Fatalf("after a decode error: %q", pli)
	}
}

func TestReceiverOverflows(t *testing.T) {
	cfg := Chrome
	cfg.MaxNackList, cfg.MaxBuffered = 4, 6
	c := &clock{now: time.Unix(0, 0)}
	r := NewReceiver(cfg, c.now)
	var s stream
	push(r, c, s.keyframe()...)
	s.seq += 5
	push(r, c, s.delta()...)
	if nack, pli := r.Poll(c.tick(time.Millisecond)); pli != PLINackOverflow || nack != nil {
		t.Fatalf("five missing of four allowed: nack %v, pli %q", nack, pli)
	}
	for range 6 {
		push(r, c, s.delta()...)
	}
	if _, pli := r.Poll(c.tick(time.Millisecond)); pli != PLIBufferFull {
		t.Fatalf("seven held of six allowed: %q", pli)
	}
	if frames := push(r, c, s.keyframe()...); len(frames) != 1 {
		t.Fatalf("a keyframe after clearing gave %d frames", len(frames))
	}
}

func TestReceiverUnwrapsSequenceNumbers(t *testing.T) {
	c := &clock{now: time.Unix(0, 0)}
	r := NewReceiver(Chrome, c.now)
	s := stream{seq: 65534}
	frames := push(r, c, append(append(s.keyframe(), s.delta()...), s.delta()...)...)
	if len(frames) != 3 {
		t.Fatalf("got %d frames across the wrap", len(frames))
	}
	lost := s.delta()
	push(r, c, s.delta()...)
	if nack, _ := r.Poll(c.tick(time.Millisecond)); !slices.Equal(nack, []uint16{lost[0].Seq}) {
		t.Fatalf("NACKed %v, want [%d]", nack, lost[0].Seq)
	}
}

func TestNalTypes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload []byte
		want    []byte
		key     bool
	}{
		{"empty", nil, nil, false},
		{"P slice", []byte{0x41, 1}, []byte{1}, false},
		{"IDR slice", []byte{0x65, 1}, []byte{5}, true},
		{"STAP-A of SPS and PPS", []byte{0x78, 0, 2, 0x67, 1, 0, 2, 0x68, 1}, []byte{7, 8}, true},
		{"STAP-A with a size past the end", []byte{0x78, 0, 9, 0x67}, nil, false},
		{"FU-A of an IDR slice", []byte{0x7c, 0x85}, []byte{5}, true},
		{"FU-A without its header", []byte{0x7c}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nalTypes(tc.payload); !slices.Equal(got, tc.want) {
				t.Errorf("% x: types %v, want %v", tc.payload, got, tc.want)
			}
			if got := isKeyPacket(tc.payload); got != tc.key {
				t.Errorf("% x: key %v", tc.payload, got)
			}
		})
	}
}
