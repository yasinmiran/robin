package peer

import (
	"strings"
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

func TestPacerFollowsTheTarget(t *testing.T) {
	now := time.Unix(0, 0)
	p := newPacerAt(1_000_000, func() time.Time { return now })
	p.SetTargetBitrate(8_000_000)
	if want := pacingFactor * 8_000_000 / 8; p.rate != want {
		t.Fatalf("rate %f bytes/s, want %f", p.rate, want)
	}
}

func TestPacerRunsUntilClosed(t *testing.T) {
	log := &sentLog{now: new(time.Time)}
	p := newPacer(1_000_000)
	p.AddStream(1, log.writer())
	for i := range 20 {
		p.Write(&rtp.Header{SSRC: 1, SequenceNumber: uint16(i)}, make([]byte, 1188), nil)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		left := len(p.queue)
		p.mu.Unlock()
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d packets still queued after 2 s", left)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}
