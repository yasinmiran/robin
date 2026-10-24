package peer

import (
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

type recorder struct {
	mu   sync.Mutex
	seqs []uint16
}

func (w *recorder) Write(h *rtp.Header, _ []byte, _ interceptor.Attributes) (int, error) {
	w.mu.Lock()
	w.seqs = append(w.seqs, h.SequenceNumber)
	w.mu.Unlock()
	return 0, nil
}

func (w *recorder) take() []uint16 {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.seqs
	w.seqs = nil
	return s
}

// newTestResponder binds one NACK-capable stream and sends seqs 0..n-1 of
// 1000-byte payloads through it.
func newTestResponder(t *testing.T, bps int, n int) (*nackResponder, *recorder) {
	t.Helper()
	f := &nackResponderFactory{budget: func() int { return bps }}
	i, _ := f.NewInterceptor("")
	r := i.(*nackResponder)
	out := &recorder{}
	w := r.BindLocalStream(&interceptor.StreamInfo{
		SSRC: 7, RTCPFeedback: []interceptor.RTCPFeedback{{Type: "nack"}},
	}, out)
	for seq := 0; seq < n; seq++ {
		w.Write(&rtp.Header{SSRC: 7, SequenceNumber: uint16(seq)}, make([]byte, 1000), nil)
	}
	out.take()
	return r, out
}

func nackFor(seqs ...uint16) *rtcp.TransportLayerNack {
	n := &rtcp.TransportLayerNack{MediaSSRC: 7}
	for _, s := range seqs {
		n.Nacks = append(n.Nacks, rtcp.NackPair{PacketID: s})
	}
	return n
}

func TestNackResendsRecentPackets(t *testing.T) {
	r, out := newTestResponder(t, 10_000_000, 10)
	now := time.Now()
	r.resend(nackFor(3, 5), now)
	r.resend(nackFor(3, 5), now.Add(nackBurst)) // tokens accrue from the first call
	got := out.take()
	if len(got) != 2 || got[0] != 3 || got[1] != 5 {
		t.Fatalf("resent %v, want [3 5]", got)
	}
}

func TestNackCapsResendsPerPacket(t *testing.T) {
	r, out := newTestResponder(t, 100_000_000, 4)
	now := time.Now()
	r.resend(nackFor(1), now)
	for i := 1; i <= 5; i++ {
		r.resend(nackFor(1), now.Add(time.Duration(i)*nackBurst/2))
	}
	if got := out.take(); len(got) != nackMaxResends {
		t.Fatalf("packet resent %d times, want %d", len(got), nackMaxResends)
	}
}

func TestNackIgnoresStalePackets(t *testing.T) {
	r, out := newTestResponder(t, 100_000_000, 4)
	r.resend(nackFor(2), time.Now())
	r.resend(nackFor(2), time.Now().Add(nackMaxAge+time.Millisecond))
	if got := out.take(); len(got) != 0 {
		t.Fatalf("resent stale packets %v", got)
	}
}

func TestNackStaysWithinBudget(t *testing.T) {
	// At 1 Mbit/s the share is 250 kbit/s, so a 100 ms burst allows about
	// 25 kbit, three 1000-byte packets, however many are asked for.
	r, out := newTestResponder(t, 1_000_000, 200)
	now := time.Now()
	r.resend(nackFor(0), now) // starts the bucket
	all := make([]uint16, 0, 200)
	for s := uint16(1); s < 200; s++ {
		all = append(all, s)
	}
	r.resend(nackFor(all...), now.Add(200*time.Millisecond))
	got := out.take()
	if len(got) == 0 || len(got) > 4 {
		t.Fatalf("resent %d packets in one burst, want 1 to 4", len(got))
	}
}

func TestNackReportsWhatItCannotResend(t *testing.T) {
	r, out := newTestResponder(t, 100_000_000, nackHistory+10)
	var lost int
	r.lost = func() { lost++ }
	now := time.Now()
	r.resend(nackFor(), now) // starts the bucket
	for range nackMaxResends + 1 {
		now = now.Add(nackBurst)
		r.resend(nackFor(nackHistory+5), now)
	}
	if lost != 0 || len(out.take()) != nackMaxResends {
		t.Fatalf("lost reported %d times for a packet it still holds", lost)
	}
	r.resend(nackFor(3), now) // overwritten by a newer packet
	if lost != 1 {
		t.Fatalf("lost reported %d times for a packet it no longer holds, want 1", lost)
	}
	r.resend(nackFor(nackHistory+6), now.Add(nackMaxAge+time.Millisecond))
	if lost != 2 {
		t.Fatalf("lost reported %d times after a packet aged out, want 2", lost)
	}
}

func TestNackSkipsStreamsWithoutNack(t *testing.T) {
	f := &nackResponderFactory{budget: func() int { return 1_000_000 }}
	i, _ := f.NewInterceptor("")
	out := &recorder{}
	w := i.BindLocalStream(&interceptor.StreamInfo{SSRC: 9}, out)
	if w != interceptor.RTPWriter(out) {
		t.Fatal("stream without nack feedback was wrapped")
	}
}
