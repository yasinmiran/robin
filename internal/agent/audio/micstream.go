//go:build linux

package audio

import (
	"github.com/pion/rtp"

	"github.com/yasinmiran/robin/internal/agent/opus"
)

const (
	// maxConceal bounds the sound a stream makes up for a gap. Longer gaps
	// are mostly left out: the listener hears a jump rather than a smear,
	// and the latency stays where it was.
	maxConceal = 5 * opus.FrameSamples
	// concealStep is libopus's unit of concealment, 2.5 ms.
	concealStep = opus.SampleRate / 400
	// maxGap is the most packets a stream counts as lost. A bigger jump in
	// sequence numbers is a sender that started over, not loss.
	maxGap = 50
)

// micDecoder is what a micStream needs of opus.Decoder.
type micDecoder interface {
	Decode(pkt []byte, pcm []int16) (int, error)
	Recover(pkt []byte, pcm []int16, samples int) (int, error)
	Conceal(pcm []int16, samples int) (int, error)
}

// micStream turns the browser's microphone RTP into mono PCM in order. A
// packet lost just before one that arrives is recovered from that one's
// in-band FEC; what is lost before that is concealed, up to maxConceal; a
// packet that arrives after a later one is dropped, as its moment has
// passed. A pause, the track replaced with none and then a track again,
// moves the timestamps on without a gap in sequence numbers and is not
// loss. out receives each piece of PCM and must copy what it keeps.
type micStream struct {
	dec micDecoder
	out func([]int16)
	pcm []int16

	started bool
	next    uint16 // sequence number expected next
	nextTS  uint32 // the timestamp that packet should carry
	last    int    // samples per channel of the last packet decoded

	// What the stream has seen, for the log.
	packets, lost, late, recovered, concealed, failed int
}

func newMicStream(dec micDecoder, out func([]int16)) *micStream {
	return &micStream{dec: dec, out: out, pcm: make([]int16, opus.MaxFrameSamples), last: opus.FrameSamples}
}

// push takes the next packet off the track.
func (m *micStream) push(p *rtp.Packet) {
	if len(p.Payload) == 0 {
		return // padding, as a sender probes for bandwidth
	}
	m.packets++
	if m.started {
		switch gap := int16(p.SequenceNumber - m.next); {
		case gap < 0:
			m.late++
			return
		case gap > 0 && gap <= maxGap:
			m.lost += int(gap)
			missing := int(int32(p.Timestamp - m.nextTS))
			if missing <= 0 || missing > maxGap*opus.MaxFrameSamples {
				missing = int(gap) * m.last
			}
			m.fill(p, missing)
		}
	}
	m.started = true
	m.next = p.SequenceNumber + 1
	n, err := m.dec.Decode(p.Payload, m.pcm)
	if err != nil {
		m.failed++
		m.nextTS = p.Timestamp + uint32(m.last)
		return
	}
	m.last = n
	m.nextTS = p.Timestamp + uint32(n)
	m.out(m.pcm[:n])
}

// fill makes up the missing samples before p: concealment for all but the
// last frame of the gap, then that frame from p's in-band FEC.
func (m *micStream) fill(p *rtp.Packet, missing int) {
	fec := min(m.last, missing) / concealStep * concealStep
	if fec == 0 {
		return
	}
	conceal := min(missing-fec, maxConceal-fec) / concealStep * concealStep
	for conceal > 0 {
		n, err := m.dec.Conceal(m.pcm, min(conceal, opus.MaxFrameSamples))
		if err != nil || n == 0 {
			m.failed++
			break
		}
		m.concealed += n
		conceal -= n
		m.out(m.pcm[:n])
	}
	n, err := m.dec.Recover(p.Payload, m.pcm, fec)
	if err != nil {
		m.failed++
		return
	}
	m.recovered += n
	m.out(m.pcm[:n])
}
