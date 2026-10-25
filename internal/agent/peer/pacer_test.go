package peer

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

type sentLog struct {
	seqs []uint16
	at   []time.Time
	now  *time.Time
}

func (l *sentLog) writer() interceptor.RTPWriter {
	return interceptor.RTPWriterFunc(func(h *rtp.Header, payload []byte, _ interceptor.Attributes) (int, error) {
		l.seqs = append(l.seqs, h.SequenceNumber)
		l.at = append(l.at, *l.now)
		return h.MarshalSize() + len(payload), nil
	})
}

// pacedRun writes n packets of size bytes at once through a pacer at bps
// and drains it every pacingTick, returning when each went out.
func pacedRun(t *testing.T, bps, n, size int) (*sentLog, time.Time) {
	t.Helper()
	start := time.Unix(0, 0)
	now := start
	log := &sentLog{now: &now}
	p := newPacerAt(bps, func() time.Time { return now })
	p.AddStream(1, log.writer())
	payload := make([]byte, size-12)
	for i := range n {
		if _, err := p.Write(&rtp.Header{SSRC: 1, SequenceNumber: uint16(i)}, payload, nil); err != nil {
			t.Fatal(err)
		}
	}
	for range 2000 {
		if len(log.seqs) == n {
			break
		}
		now = now.Add(pacingTick)
		p.drain()
	}
	if len(log.seqs) != n {
		t.Fatalf("%d of %d packets sent after 4 s", len(log.seqs), n)
	}
	for i, s := range log.seqs {
		if int(s) != i {
			t.Fatalf("sent out of order: %v", log.seqs)
		}
	}
	return log, start
}

func TestPacerSendsAFrameOfOrdinarySizeAtOnce(t *testing.T) {
	// 4 Mbit/s at 30 fps is 16 KB a frame; the allowance is 20 ms at
	// 6 Mbit/s, 15 KB.
	log, start := pacedRun(t, 4_000_000, 12, 1200)
	if last := log.at[len(log.at)-1].Sub(start); last != 0 {
		t.Fatalf("a 14 KB frame took %v to go out", last)
	}
}

func TestPacerSpreadsAKeyframe(t *testing.T) {
	const bps, n, size = 4_000_000, 125, 1200 // 150 KB
	log, start := pacedRun(t, bps, n, size)
	took := log.at[n-1].Sub(start)
	// At 1.5 times 4 Mbit/s, 150 KB less the burst takes 180 ms.
	want := time.Duration(float64(n*size)-pacingFactor*bps/8*pacingBurst.Seconds()) * time.Second / (pacingFactor * bps / 8)
	if took < want-10*time.Millisecond || took > want+10*time.Millisecond {
		t.Fatalf("150 KB went out over %v, want about %v", took, want)
	}
	// The first part goes at once.
	if log.at[0] != start {
		t.Fatalf("the first packet waited %v", log.at[0].Sub(start))
	}
}

func TestPacerNeverHoldsLongerThanItsBound(t *testing.T) {
	// 600 KB at 1.5 times 1 Mbit/s would take 3.2 s.
	const n = 500
	log, start := pacedRun(t, 1_000_000, n, 1200)
	if took := log.at[n-1].Sub(start); took > maxPacingDelay+20*time.Millisecond {
		t.Fatalf("600 KB took %v, more than the bound of %v", took, maxPacingDelay)
	}
}

func TestPacerKeepsOrderBehindAQueue(t *testing.T) {
	now := time.Unix(0, 0)
	log := &sentLog{now: &now}
	p := newPacerAt(1_000_000, func() time.Time { return now })
	p.AddStream(1, log.writer())
	p.AddStream(2, log.writer())
	big := make([]byte, 1188)
	for i := range 10 {
		p.Write(&rtp.Header{SSRC: 1, SequenceNumber: uint16(i)}, big, nil)
	}
	// A small packet of another stream waits its turn.
	p.Write(&rtp.Header{SSRC: 2, SequenceNumber: 10}, []byte{1}, nil)
	for range 200 {
		now = now.Add(pacingTick)
		p.drain()
	}
	if len(log.seqs) != 11 || log.seqs[10] != 10 {
		t.Fatalf("sent %v", log.seqs)
	}
	if _, err := p.Write(&rtp.Header{SSRC: 9}, nil, nil); err == nil || !strings.Contains(err.Error(), "unknown SSRC") {
		t.Fatalf("an unknown stream: %v", err)
	}
}

// TestPacerSendsSoundAhead: a prioritized stream's packet goes out at once
// while a keyframe drains, and the keyframe's last packet goes out that
// much later.
func TestPacerSendsSoundAhead(t *testing.T) {
	now := time.Unix(0, 0)
	log := &sentLog{now: &now}
	p := newPacerAt(1_000_000, func() time.Time { return now })
	p.AddStream(1, log.writer())
	p.AddStream(2, log.writer())
	p.Prioritize(2)
	big := make([]byte, 1188)
	for i := range 50 {
		p.Write(&rtp.Header{SSRC: 1, SequenceNumber: uint16(i)}, big, nil)
	}
	queued := len(log.seqs)
	sound := make([]byte, 288)
	var soundAt []time.Time
	for i := range 200 {
		now = now.Add(pacingTick)
		if i%10 == 0 {
			p.Write(&rtp.Header{SSRC: 2, SequenceNumber: uint16(1000 + i)}, sound, nil)
			if s := log.seqs[len(log.seqs)-1]; s != uint16(1000+i) {
				t.Fatalf("sound packet %d did not go out at once: last sent %d", 1000+i, s)
			}
			soundAt = append(soundAt, now)
		}
		p.drain()
	}
	if queued == 50 {
		t.Fatal("the keyframe was not queued")
	}
	var last time.Time
	for i, s := range log.seqs {
		if s == 49 {
			last = log.at[i]
		}
	}
	// 60 KB at 1.5 Mbit/s less the burst is 280 ms; the sound sent
	// meanwhile, 15 packets of 300 bytes, adds 24 ms.
	if took := last.Sub(time.Unix(0, 0)); took < 290*time.Millisecond || took > 330*time.Millisecond {
		t.Fatalf("the keyframe's last packet went out after %v", took)
	}
}

// TestPacerNumbersPacketsAsTheyLeave: with sound sent ahead of a queued
// keyframe, the transport-wide sequence numbers on the wire still count up
// one by one, as the browser's feedback needs them to.
func TestPacerNumbersPacketsAsTheyLeave(t *testing.T) {
	const ext = 5
	now := time.Unix(0, 0)
	var seqs []uint16
	w := interceptor.RTPWriterFunc(func(h *rtp.Header, _ []byte, _ interceptor.Attributes) (int, error) {
		if h.SSRC == 0 {
			if h.GetExtension(ext) != nil {
				t.Error("a stream without the extension left with it")
			}
			return 0, nil
		}
		var tcc rtp.TransportCCExtension
		if err := tcc.Unmarshal(h.GetExtension(ext)); err != nil {
			t.Fatalf("packet %d left without its number: %v", h.SequenceNumber, err)
		}
		seqs = append(seqs, tcc.TransportSequence)
		return 0, nil
	})
	p := newPacerAt(1_000_000, func() time.Time { return now })
	for ssrc := range uint32(3) {
		p.AddStream(ssrc, w)
	}
	p.SetTransportCC(1, ext)
	p.SetTransportCC(2, ext)
	p.Prioritize(2)
	transport := uint16(65500) // wraps on the way
	write := func(ssrc uint32, size int) {
		h := &rtp.Header{SSRC: ssrc, SequenceNumber: transport}
		if ssrc != 0 {
			b, _ := rtp.TransportCCExtension{TransportSequence: transport}.Marshal()
			h.SetExtension(ext, b)
			transport++
		}
		if _, err := p.Write(h, make([]byte, size), nil); err != nil {
			t.Fatal(err)
		}
	}
	for range 60 {
		write(1, 1200)
	}
	for i := range 300 {
		now = now.Add(pacingTick)
		if i%10 == 0 {
			write(2, 300)
		}
		if i == 50 {
			write(0, 100) // a stream without the extension is left alone
		}
		p.drain()
	}
	if len(seqs) != 90 {
		t.Fatalf("%d numbered packets left, want 90", len(seqs))
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("numbers on the wire skip: ... %d, %d ...", seqs[i-1], seqs[i])
		}
	}
	if seqs[0] != 65500 {
		t.Fatalf("first number %d", seqs[0])
	}
}

func TestTransportSeqsHandOutTheSmallestFirst(t *testing.T) {
	var s transportSeqs
	for _, seq := range []uint16{65534, 0, 65535, 2, 1} {
		s.add(seq)
	}
	for _, want := range []uint16{65534, 65535, 0, 1, 2} {
		if got := s.next(); got != want {
			t.Fatalf("next %d, want %d", got, want)
		}
	}
}

func TestPacerFollowsTheTarget(t *testing.T) {
	now := time.Unix(0, 0)
	p := newPacerAt(1_000_000, func() time.Time { return now })
	p.SetTargetBitrate(8_000_000)
	if want := pacingFactor * 8_000_000 / 8; p.rate != want {
		t.Fatalf("rate %f bytes/s, want %f", p.rate, want)
	}
}

// countingWriter counts the packets written through it, from any goroutine.
func countingWriter(n *atomic.Int64) interceptor.RTPWriter {
	return interceptor.RTPWriterFunc(func(h *rtp.Header, payload []byte, _ interceptor.Attributes) (int, error) {
		n.Add(1)
		return h.MarshalSize() + len(payload), nil
	})
}

// waitDrained waits until the drain goroutine has emptied p's queue.
func waitDrained(t *testing.T, p *pacer) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		left := len(p.queue)
		p.mu.Unlock()
		if left == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d packets still queued after 2 s", left)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPacerRunsUntilClosed(t *testing.T) {
	var sent atomic.Int64
	p := newPacer(1_000_000)
	p.AddStream(1, countingWriter(&sent))
	for i := range 20 {
		p.Write(&rtp.Header{SSRC: 1, SequenceNumber: uint16(i)}, make([]byte, 1188), nil)
	}
	waitDrained(t, p)
	if n := sent.Load(); n != 20 {
		t.Fatalf("%d of 20 packets sent", n)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.stopped:
	default:
		t.Fatal("Close returned before the drain goroutine did")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestPacerSleepsWhileIdle checks that the drain goroutine stops waking
// once the queue is empty, and wakes again for the next burst.
func TestPacerSleepsWhileIdle(t *testing.T) {
	var reads, sent atomic.Int64
	p := newPacerAt(1_000_000, func() time.Time {
		reads.Add(1)
		return time.Now()
	}).start()
	defer p.Close()
	p.AddStream(1, countingWriter(&sent))
	for burst := range 2 {
		for i := range 20 {
			p.Write(&rtp.Header{SSRC: 1, SequenceNumber: uint16(20*burst + i)}, make([]byte, 1188), nil)
		}
		waitDrained(t, p)
		before := reads.Load()
		time.Sleep(100 * time.Millisecond)
		// A wake left over from the burst may cost one more drain; ticking
		// on would read the clock about 50 times.
		if n := reads.Load() - before; n > 2 {
			t.Fatalf("burst %d: the clock was read %d times in 100 ms with nothing queued", burst, n)
		}
	}
	if n := sent.Load(); n != 40 {
		t.Fatalf("%d of 40 packets sent", n)
	}
}

func TestPacerDropsItsQueueOnClose(t *testing.T) {
	now := time.Unix(0, 0)
	var sent atomic.Int64
	p := newPacerAt(1_000_000, func() time.Time { return now })
	p.AddStream(1, countingWriter(&sent))
	for i := range 100 {
		p.Write(&rtp.Header{SSRC: 1, SequenceNumber: uint16(i)}, make([]byte, 1188), nil)
	}
	early := sent.Load()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if p.drain() || sent.Load() != early {
		t.Fatalf("%d packets sent after Close", sent.Load()-early)
	}
	if _, err := p.Write(&rtp.Header{SSRC: 1}, make([]byte, 10), nil); !errors.Is(err, errPacerClosed) {
		t.Fatalf("Write after Close: %v", err)
	}
}
