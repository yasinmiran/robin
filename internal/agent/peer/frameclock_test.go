package peer

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/robin/internal/protocol"
)

// TestNextFrameRTPIsTheFramesTimestamp writes frames of uneven durations,
// fractions of a tick included, and checks that each one the browser gets
// carries the timestamp NextFrameRTP named before it was written.
func TestNextFrameRTPIsTheFramesTimestamp(t *testing.T) {
	cands := make(chan protocol.Candidate, 32)
	connected := make(chan struct{})
	var once sync.Once
	p, err := New(Config{MinBitrate: 100_000, StartBitrate: 1_000_000, MaxBitrate: 5_000_000, IncludeLoopback: true},
		Handlers{
			Candidate: func(c protocol.Candidate) { cands <- c },
			State: func(s webrtc.PeerConnectionState) {
				if s == webrtc.PeerConnectionStateConnected {
					once.Do(func() { close(connected) })
				}
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, ok := p.NextFrameRTP(); ok {
		t.Fatal("NextFrameRTP is known before the browser was answered")
	}
	// Written before the track is bound, a frame goes nowhere and moves no
	// clock.
	frame := func(i int) []byte {
		au := []byte{0, 0, 0, 1, 0x65, 0, 0, 0x84, 0x21}
		binary.BigEndian.PutUint16(au[5:], uint16(i))
		return au
	}
	before, _ := p.NextFrameRTP()
	if err := p.WriteFrame(frame(0), time.Second); err != nil {
		t.Fatal(err)
	}
	if after, _ := p.NextFrameRTP(); after != before {
		t.Fatalf("an unbound write moved the clock from %d to %d", before, after)
	}

	b := newBrowser(t)
	connect(t, b, p, cands)
	if _, ok := p.NextFrameRTP(); !ok {
		t.Fatal("NextFrameRTP is unknown once the browser was answered")
	}
	select {
	case <-connected:
	case <-time.After(15 * time.Second):
		t.Fatal("peer never connected")
	}

	durations := []time.Duration{
		33 * time.Millisecond, 16666667, 11111, 11112, time.Millisecond / 3, 500 * time.Millisecond, 33333333, 7,
	}
	const n = 400
	want := make([]atomic.Uint32, n)
	stop, wrote := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(wrote)
		pace := time.NewTicker(2 * time.Millisecond)
		defer pace.Stop()
		for i := range n {
			ts, _ := p.NextFrameRTP()
			want[i].Store(ts)
			if err := p.WriteFrame(frame(i), durations[i%len(durations)]); err != nil {
				t.Errorf("write frame %d: %v", i, err)
				return
			}
			select {
			case <-pace.C:
			case <-stop:
				return
			}
		}
	}()
	// The writer stops before the deferred Close.
	defer func() {
		close(stop)
		<-wrote
	}()
	var tr *webrtc.TrackRemote
	select {
	case tr = <-b.track:
	case <-time.After(15 * time.Second):
		t.Fatal("no video track arrived")
	}
	checked := 0
	deadline := time.Now().Add(20 * time.Second)
	for checked < 100 && time.Now().Before(deadline) {
		tr.SetReadDeadline(deadline)
		pkt, _, err := tr.ReadRTP()
		if err != nil {
			t.Fatalf("read rtp after %d frames: %v", checked, err)
		}
		if len(pkt.Payload) < 3 || pkt.Payload[0]&0x1f != 5 {
			continue
		}
		i := int(binary.BigEndian.Uint16(pkt.Payload[1:]))
		if i >= n || pkt.Timestamp != want[i].Load() {
			t.Fatalf("frame %d carries RTP %d, NextFrameRTP said %d", i, pkt.Timestamp, want[min(i, n-1)].Load())
		}
		checked++
	}
	if checked < 100 {
		t.Fatalf("only %d frames arrived", checked)
	}
}
