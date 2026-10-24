//go:build linux

package audio

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/pion/rtp"
)

// scriptDecoder stands in for libopus and records what it was asked: each
// packet's payload is one byte naming it and one giving its length in
// 2.5 ms steps.
type scriptDecoder struct {
	calls []string
}

func (d *scriptDecoder) samples(pkt []byte) int { return int(pkt[1]) * concealStep }

func (d *scriptDecoder) Decode(pkt []byte, pcm []int16) (int, error) {
	if pkt[0] == 'X' {
		d.calls = append(d.calls, "corrupt")
		return 0, errors.New("corrupt packet")
	}
	n := d.samples(pkt)
	d.calls = append(d.calls, fmt.Sprintf("decode %c %d", pkt[0], n))
	fill(pcm[:n], int16(pkt[0]))
	return n, nil
}

func (d *scriptDecoder) Recover(pkt []byte, pcm []int16, samples int) (int, error) {
	d.calls = append(d.calls, fmt.Sprintf("recover from %c %d", pkt[0], samples))
	fill(pcm[:samples], -int16(pkt[0]))
	return samples, nil
}

func (d *scriptDecoder) Conceal(pcm []int16, samples int) (int, error) {
	d.calls = append(d.calls, fmt.Sprintf("conceal %d", samples))
	fill(pcm[:samples], 0)
	return samples, nil
}

func fill(pcm []int16, v int16) {
	for i := range pcm {
		pcm[i] = v
	}
}

// packet is a 20 ms packet named name with sequence number seq, timed as the
// seq'th packet since timestamp 1000.
func packet(name byte, seq uint16) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{SequenceNumber: seq, Timestamp: 1000 + uint32(seq)*960},
		Payload: []byte{name, 8},
	}
}

type heard struct {
	stream *micStream
	dec    *scriptDecoder
	pieces []int // length of each piece of PCM out
}

func newHeard() *heard {
	h := &heard{dec: &scriptDecoder{}}
	h.stream = newMicStream(h.dec, func(pcm []int16) { h.pieces = append(h.pieces, len(pcm)) })
	return h
}

func (h *heard) push(ps ...*rtp.Packet) {
	for _, p := range ps {
		h.stream.push(p)
	}
}

func (h *heard) want(t *testing.T, calls ...string) {
	t.Helper()
	if !slices.Equal(h.dec.calls, calls) {
		t.Errorf("decoder calls:\n got  %q\n want %q", h.dec.calls, calls)
	}
}

func TestMicStreamInOrder(t *testing.T) {
	h := newHeard()
	h.push(packet('a', 7), packet('b', 8), packet('c', 9))
	h.want(t, "decode a 960", "decode b 960", "decode c 960")
	if !slices.Equal(h.pieces, []int{960, 960, 960}) {
		t.Errorf("pieces %v", h.pieces)
	}
}

func TestMicStreamRecoversOneLostPacket(t *testing.T) {
	h := newHeard()
	h.push(packet('a', 7), packet('c', 9))
	h.want(t, "decode a 960", "recover from c 960", "decode c 960")
	if h.stream.lost != 1 || h.stream.recovered != 960 {
		t.Errorf("lost %d, recovered %d", h.stream.lost, h.stream.recovered)
	}
}

func TestMicStreamConcealsBeforeRecovering(t *testing.T) {
	h := newHeard()
	h.push(packet('a', 7), packet('e', 11))
	h.want(t, "decode a 960", "conceal 1920", "recover from e 960", "decode e 960")
}

// TestMicStreamBoundsConcealment: a long gap gets maxConceal of made-up
// sound at most, so the latency does not grow by the length of the gap.
func TestMicStreamBoundsConcealment(t *testing.T) {
	h := newHeard()
	h.push(packet('a', 7), packet('z', 30))
	h.want(t, "decode a 960", fmt.Sprintf("conceal %d", maxConceal-960), "recover from z 960", "decode z 960")
	if h.stream.lost != 22 {
		t.Errorf("lost %d, want 22", h.stream.lost)
	}
}

func TestMicStreamDropsLatePackets(t *testing.T) {
	h := newHeard()
	h.push(packet('a', 7), packet('c', 9), packet('b', 8), packet('c', 9), packet('d', 10))
	h.want(t, "decode a 960", "recover from c 960", "decode c 960", "decode d 960")
	if h.stream.late != 2 {
		t.Errorf("late %d, want 2", h.stream.late)
	}
}

func TestMicStreamAcrossTheSequenceWrap(t *testing.T) {
	h := newHeard()
	h.push(packet('a', 65534), packet('b', 65535), packet('c', 0), packet('e', 2))
	h.want(t, "decode a 960", "decode b 960", "decode c 960", "recover from e 960", "decode e 960")
}

// TestMicStreamStartsOver: a sender that jumps far ahead in sequence
// numbers started a new stream; nothing is made up for the jump.
func TestMicStreamStartsOver(t *testing.T) {
	h := newHeard()
	h.push(packet('a', 7), packet('b', 8+maxGap+1))
	h.want(t, "decode a 960", "decode b 960")
}

// TestMicStreamPauseIsNotLoss: the page turns the microphone off and on
// again; the sender's clock moved on, its sequence numbers did not.
func TestMicStreamPauseIsNotLoss(t *testing.T) {
	h := newHeard()
	b := packet('b', 8)
	b.Timestamp += 48000 * 30
	h.push(packet('a', 7), b)
	h.want(t, "decode a 960", "decode b 960")
}

// TestMicStreamMeasuresGapsInTime: with 10 ms packets, a lost one is 10 ms
// of sound, which the timestamps say even before a packet of that length
// has been decoded.
func TestMicStreamMeasuresGapsInTime(t *testing.T) {
	h := newHeard()
	short := func(name byte, seq uint16) *rtp.Packet {
		return &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Timestamp: uint32(seq) * 480}, Payload: []byte{name, 4}}
	}
	h.push(short('a', 1), short('c', 3), short('f', 6))
	h.want(t, "decode a 480", "recover from c 480", "decode c 480", "conceal 480", "recover from f 480", "decode f 480")
}

// TestMicStreamUnreliableTimestamps: a gap whose timestamps make no sense
// is measured in packets of the last length instead.
func TestMicStreamUnreliableTimestamps(t *testing.T) {
	h := newHeard()
	c := packet('c', 9)
	c.Timestamp = 5
	h.push(packet('a', 7), c)
	h.want(t, "decode a 960", "recover from c 960", "decode c 960")
}

func TestMicStreamSkipsWhatItCannotDecode(t *testing.T) {
	h := newHeard()
	h.push(packet('a', 7), packet('X', 8), packet('c', 9), &rtp.Packet{Header: rtp.Header{SequenceNumber: 10}})
	h.want(t, "decode a 960", "corrupt", "decode c 960")
	if h.stream.failed != 1 || h.stream.packets != 3 {
		t.Errorf("failed %d, packets %d", h.stream.failed, h.stream.packets)
	}
}

// failingDecoder fails every concealment and recovery, as libopus would on
// a frame size it does not take.
type failingDecoder struct{ scriptDecoder }

func (d *failingDecoder) Recover([]byte, []int16, int) (int, error) { return 0, errors.New("no") }
func (d *failingDecoder) Conceal([]int16, int) (int, error)         { return 0, errors.New("no") }

func TestMicStreamCarriesOnWhenFillingFails(t *testing.T) {
	d := &failingDecoder{}
	var pieces int
	m := newMicStream(d, func([]int16) { pieces++ })
	for _, p := range []*rtp.Packet{packet('a', 7), packet('e', 11), packet('f', 12)} {
		m.push(p)
	}
	if pieces != 3 || m.failed != 2 {
		t.Errorf("%d pieces out, %d failures; want the three packets and two failures", pieces, m.failed)
	}
}
