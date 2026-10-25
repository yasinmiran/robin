package protocol

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func pipePair(interval, timeout time.Duration) (a, b *Conn, cleanup func()) {
	ar, bw := io.Pipe()
	br, aw := io.Pipe()
	a = NewConn(ar, aw, interval, timeout)
	b = NewConn(br, bw, interval, timeout)
	return a, b, func() {
		a.Close()
		b.Close()
		aw.Close()
		bw.Close()
	}
}

func TestConnRoundTrip(t *testing.T) {
	a, b, cleanup := pipePair(0, 0)
	defer cleanup()

	mid := "0"
	idx := uint16(0)
	want := Message{Type: TypeCandidate, Candidate: &Candidate{
		Candidate: "candidate:1 1 udp 2130706431 10.0.0.2 50000 typ host", SDPMid: &mid, SDPMLineIndex: &idx,
	}}
	go func() {
		if err := a.Send(want); err != nil {
			t.Errorf("send: %v", err)
		}
	}()
	got, err := b.Recv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != want.Type || got.Candidate == nil || got.Candidate.Candidate != want.Candidate.Candidate ||
		*got.Candidate.SDPMid != mid || *got.Candidate.SDPMLineIndex != idx {
		t.Fatalf("got %+v", got)
	}
}

func TestConnSkipsPingsAndKeepsAlive(t *testing.T) {
	a, b, cleanup := pipePair(20*time.Millisecond, 120*time.Millisecond)
	defer cleanup()

	// Both sides ping each other, so neither should go idle while we wait
	// several timeouts; drain a's side so its writes to b never block.
	go func() {
		for {
			if _, err := a.Recv(context.Background()); err != nil {
				return
			}
		}
	}()
	time.Sleep(400 * time.Millisecond)
	if err := b.Err(); err != nil {
		t.Fatalf("conn died despite pings: %v", err)
	}
	go a.Send(Message{Type: TypeBye})
	got, err := b.Recv(context.Background())
	if err != nil || got.Type != TypeBye {
		t.Fatalf("got %+v, %v; pings must not surface from Recv", got, err)
	}
}

func TestConnRecvStopsWithItsContext(t *testing.T) {
	a, b, cleanup := pipePair(0, 0)
	defer cleanup()

	late := errors.New("waited long enough")
	ctx, cancel := context.WithTimeoutCause(context.Background(), 50*time.Millisecond, late)
	defer cancel()
	if _, err := b.Recv(ctx); !errors.Is(err, late) {
		t.Fatalf("Recv = %v, want the context's cause", err)
	}
	if err := b.Err(); err != nil {
		t.Fatalf("an ended context closed the conn: %v", err)
	}
	go a.Send(Message{Type: TypeBye})
	if m, err := b.Recv(context.Background()); err != nil || m.Type != TypeBye {
		t.Fatalf("next Recv = %+v, %v; want the bye", m, err)
	}
}

func TestConnIdleTimeout(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	c := NewConn(r, io.Discard, 0, 80*time.Millisecond)
	start := time.Now()
	_, err := c.Recv(context.Background())
	if !errors.Is(err, ErrIdle) {
		t.Fatalf("want ErrIdle, got %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("idle detection took %v", d)
	}
}

func TestConnEOF(t *testing.T) {
	c := NewConn(strings.NewReader(`{"type":"bye"}`+"\n"), io.Discard, 0, 0)
	if m, err := c.Recv(context.Background()); err != nil || m.Type != TypeBye {
		t.Fatalf("got %+v, %v", m, err)
	}
	if _, err := c.Recv(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestConnRejectsStrayOutput(t *testing.T) {
	for name, in := range map[string]string{
		"not json":   "Welcome to Ubuntu 24.04 LTS\n",
		"no type":    `{"sdp":"v=0"}` + "\n",
		"json array": `["offer"]` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			c := NewConn(strings.NewReader(in), io.Discard, 0, 0)
			_, err := c.Recv(context.Background())
			if err == nil || errors.Is(err, io.EOF) {
				t.Fatalf("want framing error, got %v", err)
			}
		})
	}
}

func TestConnRejectsOversizedLine(t *testing.T) {
	line := `{"type":"offer","sdp":"` + strings.Repeat("a", MaxLineBytes) + `"}` + "\n"
	c := NewConn(strings.NewReader(line), io.Discard, 0, 0)
	_, err := c.Recv(context.Background())
	if err == nil || !strings.Contains(err.Error(), "longer") {
		t.Fatalf("want size error, got %v", err)
	}
}

// TestConnSendKeepsToThePeersLineLimit: Send writes a line the peer's
// reader takes, up to exactly MaxLineBytes with its newline, and refuses one
// byte more without writing it, so a message encoded larger than it arrived
// cannot cost the peer its connection.
func TestConnSendKeepsToThePeersLineLimit(t *testing.T) {
	a, b, cleanup := pipePair(0, 0)
	defer cleanup()
	envelope := len(`{"type":"offer","sdp":""}` + "\n")
	fits := Message{Type: TypeOffer, SDP: strings.Repeat("a", MaxLineBytes-envelope)}
	over := Message{Type: TypeOffer, SDP: strings.Repeat("<", MaxLineBytes/6)}
	sent := make(chan error, 2)
	go func() {
		sent <- a.Send(over)
		sent <- a.Send(fits)
	}()
	m, err := b.Recv(context.Background())
	if err != nil || len(m.SDP) != len(fits.SDP) {
		t.Fatalf("Recv = %d bytes of SDP, %v; want the message that fits", len(m.SDP), err)
	}
	if err := <-sent; !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("Send of a line over the limit: %v, want ErrLineTooLong", err)
	}
	if err := <-sent; err != nil {
		t.Fatalf("Send of a line at the limit: %v", err)
	}
}

func TestConnSendAfterClose(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	c := NewConn(r, io.Discard, 0, 0)
	c.Close()
	if err := c.Send(Message{Type: TypeBye}); err == nil {
		t.Fatal("send after close succeeded")
	}
}
