//go:build linux

package agent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/protocol"
)

// fakeTypist is a fakeDesktop that types text, each Type waiting for a
// token on gate when gate is set.
type fakeTypist struct {
	fakeDesktop
	gate    chan struct{}
	started chan string
}

func (f *fakeTypist) Type(ctx context.Context, text string) error {
	if f.started != nil {
		f.started <- text
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, protocol.Control{T: protocol.ControlText, Text: text})
	return nil
}

func (f *fakeTypist) got() []protocol.Control {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.inputs)
}

// waitIdle waits for the lane's goroutine to finish what it was given.
func waitIdle(t *testing.T, l *lane) {
	t.Helper()
	l.mu.Lock()
	idle := l.idle
	l.mu.Unlock()
	if idle == nil {
		return
	}
	select {
	case <-idle:
	case <-time.After(5 * time.Second):
		t.Fatal("the lane never caught up")
	}
}

func TestTextKeepsInputInOrder(t *testing.T) {
	desk := &fakeTypist{gate: make(chan struct{}), started: make(chan string, 4)}
	s := testSession(desk)
	enter := protocol.Control{T: protocol.ControlKey, Key: "Enter", Code: "Enter", Down: true}
	s.control(protocol.Control{T: protocol.ControlText, Text: "日本"})
	<-desk.started
	s.control(enter)
	s.control(protocol.Control{T: protocol.ControlClipboard, Text: "copied"})
	s.control(protocol.Control{T: protocol.ControlResize, W: 1000, H: 700}) // not input: does not wait
	if got := desk.got(); len(got) != 0 {
		t.Fatalf("input overtook the text being typed: %+v", got)
	}
	if len(desk.sizes) != 1 {
		t.Errorf("resize waited behind the text")
	}
	desk.gate <- struct{}{}
	waitIdle(t, &s.typing)
	want := []protocol.Control{{T: protocol.ControlText, Text: "日本"}, enter}
	if got := desk.got(); !slices.Equal(got, want) {
		t.Errorf("desktop got %+v, want %+v", got, want)
	}
	if !slices.Equal(desk.clipboard, []string{"copied"}) {
		t.Errorf("clipboard %q, want it set after the text", desk.clipboard)
	}

	// Caught up, input applies at once again, on the caller's goroutine.
	s.control(enter)
	if got := desk.got(); len(got) != 3 {
		t.Errorf("input after the lane caught up did not apply at once: %+v", got)
	}
}

func TestTextRefused(t *testing.T) {
	desk := &fakeTypist{}
	s := testSession(desk)
	s.control(protocol.Control{T: protocol.ControlText, Text: strings.Repeat("x", protocol.MaxTextBytes+1)})
	s.control(protocol.Control{T: protocol.ControlText, Text: "bad \xff utf-8"})
	waitIdle(t, &s.typing)
	if got := desk.got(); len(got) != 0 {
		t.Errorf("typed %+v", got)
	}
	s.control(protocol.Control{T: protocol.ControlText, Text: strings.Repeat("x", protocol.MaxTextBytes)})
	waitIdle(t, &s.typing)
	if got := desk.got(); len(got) != 1 {
		t.Errorf("text at the limit not typed: %d messages", len(got))
	}

	// A desktop that cannot type text takes none.
	plain := &fakeDesktop{}
	s = testSession(plain)
	s.control(protocol.Control{T: protocol.ControlText, Text: "hi"})
	if s.typing.idle != nil || len(plain.inputs) != 0 {
		t.Error("text went to a desktop that cannot type it")
	}
}

// TestLaneDropsWhenFull fills the lane with input that cannot merge, and
// checks what it drops is followed by one release of everything.
func TestLaneDropsWhenFull(t *testing.T) {
	desk := &fakeTypist{gate: make(chan struct{}), started: make(chan string, 4)}
	s := testSession(desk)
	s.control(protocol.Control{T: protocol.ControlText, Text: "a"})
	<-desk.started
	for i := range maxLane + 10 {
		s.control(protocol.Control{T: protocol.ControlKey, Key: "a", Code: "KeyA", Down: i%2 == 0})
	}
	if s.typing.queue(func(context.Context) {}) {
		t.Error("a full lane took more text")
	}
	desk.gate <- struct{}{}
	waitIdle(t, &s.typing)
	// The text left the queue when its typing began.
	got := desk.got()
	if len(got) != 1+maxLane+1 {
		t.Fatalf("desktop got %d messages, want the text, %d keys and a release", len(got), maxLane)
	}
	if last := got[len(got)-1]; last.T != protocol.ControlRelease {
		t.Errorf("after dropping input the lane ended on %+v, want a release", last)
	}
}

func TestLaneDiscard(t *testing.T) {
	desk := &fakeTypist{gate: make(chan struct{}), started: make(chan string, 4)}
	s := testSession(desk)
	s.control(protocol.Control{T: protocol.ControlText, Text: "from the browser that left"})
	<-desk.started
	s.control(protocol.Control{T: protocol.ControlKey, Key: "Enter", Code: "Enter", Down: true})
	done := make(chan struct{})
	go func() {
		s.typing.discard()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("discard did not stop the text being typed")
	}
	if got := desk.got(); len(got) != 0 {
		t.Errorf("a discarded lane applied %+v", got)
	}
	s.typing.discard() // nothing to discard

	// The lane works again afterwards.
	desk.gate = nil
	s.control(protocol.Control{T: protocol.ControlText, Text: "next"})
	waitIdle(t, &s.typing)
	if got := desk.got(); !slices.Equal(got, []protocol.Control{{T: protocol.ControlText, Text: "next"}}) {
		t.Errorf("after discard the desktop got %+v", got)
	}
}

// TestLaneLetsGoWhenFull fills the lane behind text being typed, with a key
// held down, and checks nothing stays held once it has caught up.
func TestLaneLetsGoWhenFull(t *testing.T) {
	desk := &fakeTypist{gate: make(chan struct{}), started: make(chan string, 4)}
	s := testSession(desk)
	s.control(protocol.Control{T: protocol.ControlText, Text: "日本"})
	<-desk.started
	down := protocol.Control{T: protocol.ControlKey, Key: "a", Code: "KeyA", Down: true}
	up := protocol.Control{T: protocol.ControlKey, Key: "a", Code: "KeyA"}
	s.control(down)
	for i := range maxLane {
		s.control(protocol.Control{T: protocol.ControlMove, X: i, Y: i})
	}
	s.control(up)
	s.control(protocol.Control{T: protocol.ControlRelease})
	desk.gate <- struct{}{}
	waitIdle(t, &s.typing)

	got := desk.got()
	typed := slices.IndexFunc(got, func(m protocol.Control) bool { return m.T == protocol.ControlText })
	if typed < 0 {
		t.Fatalf("the text was not typed: %+v", got)
	}
	// The moves merged into the last two, the second arriving once the
	// first run had merged; nothing else was lost.
	after := got[typed+1:]
	want := []protocol.Control{
		down,
		{T: protocol.ControlMove, X: maxLane - 2, Y: maxLane - 2},
		{T: protocol.ControlMove, X: maxLane - 1, Y: maxLane - 1},
		up,
		{T: protocol.ControlRelease},
	}
	if !slices.Equal(after, want) {
		t.Errorf("after the text the desktop got %d messages, want the key, the last moves and its release:\n got %+v\nwant %+v", len(after), after, want)
	}
}
