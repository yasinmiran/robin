//go:build linux

package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agent/peer"
	"github.com/yasinmiran/robin/internal/protocol"
)

// richDesktop is a fakeDesktop with a rich clipboard.
type richDesktop struct {
	fakeDesktop
	setMu sync.Mutex
	set   []protocol.Clip
	clips chan protocol.Clip
}

func (r *richDesktop) SetClipboardContent(c protocol.Clip) error {
	r.setMu.Lock()
	defer r.setMu.Unlock()
	r.set = append(r.set, c)
	return nil
}

func (r *richDesktop) ClipboardContent() <-chan protocol.Clip { return r.clips }

func (r *richDesktop) setSoFar() []protocol.Clip {
	r.setMu.Lock()
	defer r.setMu.Unlock()
	return slices.Clone(r.set)
}

// The clipboard is set before the message that set it is done with, so a
// paste key after it finds it there.
func TestClipboardFromTheBrowserIsSetInOrder(t *testing.T) {
	desk := &richDesktop{}
	s := testSession(desk)
	s.control(protocol.Control{T: protocol.ControlClipboard, Text: "first"})
	s.control(protocol.Control{T: protocol.ControlClipboard, Text: strings.Repeat("x", clipboardSize+1)})
	s.control(protocol.Control{T: protocol.ControlClipboard, Text: "second"})
	got := desk.setSoFar()
	if len(got) != 2 || got[0].Text != "first" || got[1].Text != "second" {
		t.Errorf("set %+v, want first and then second", got)
	}
}

func TestClipboardBulkFromTheBrowser(t *testing.T) {
	desk := &richDesktop{}
	s := testSession(desk)

	want := protocol.Clip{Text: "a picture", PNG: []byte{0x89, 'P', 'N', 'G'}}
	payload, _ := protocol.EncodeClip(want)
	h := protocol.Bulk{T: protocol.BulkHeader, Kind: protocol.BulkClipboard, Size: int64(len(payload))}
	sink, err := s.bulk(h)
	if err != nil {
		t.Fatal(err)
	}
	sink.Write(payload[:3])
	sink.Write(payload[3:])
	if err := sink.Commit(); err != nil {
		t.Fatal(err)
	}
	if len(desk.setSoFar()) != 1 {
		t.Fatalf("the clipboard was set %d times, want once", len(desk.setSoFar()))
	}
	if got := desk.setSoFar()[0]; got.Text != want.Text || string(got.PNG) != string(want.PNG) {
		t.Errorf("set %+v, want %+v", got, want)
	}

	sink, _ = s.bulk(protocol.Bulk{T: protocol.BulkHeader, Kind: protocol.BulkClipboard, Size: 3})
	sink.Write([]byte{9, 'x', 'y'})
	if err := sink.Commit(); !errors.Is(err, protocol.ErrClip) {
		t.Errorf("Commit of a malformed payload: %v, want ErrClip", err)
	}

	// Only so many at once, each in memory; an end of either kind frees a
	// place.
	h = protocol.Bulk{T: protocol.BulkHeader, Kind: protocol.BulkClipboard, Size: 1}
	var open []peer.BulkSink
	for range maxClipsIn {
		sink, err := s.bulk(h)
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, sink)
	}
	if _, err := s.bulk(h); err == nil {
		t.Errorf("clipboard transfer %d at once was taken", maxClipsIn+1)
	}
	open[0].Abort(nil)
	open[1].Commit()
	for range maxClipsIn {
		if _, err := s.bulk(h); err != nil {
			t.Errorf("after both ended: %v", err)
		}
	}
}

// richTypist types text and has a rich clipboard, and records both among
// its input, in the order they reach it.
type richTypist struct{ fakeTypist }

func (r *richTypist) SetClipboardContent(c protocol.Clip) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inputs = append(r.inputs, protocol.Control{T: protocol.ControlClipboard, Text: c.Text})
	return nil
}

func (r *richTypist) ClipboardContent() <-chan protocol.Clip { return nil }

// A rich clipboard from the browser waits behind text being typed, as the
// paste key after it does, so that key finds it set.
func TestClipboardBulkWaitsBehindText(t *testing.T) {
	desk := &richTypist{fakeTypist{gate: make(chan struct{}), started: make(chan string, 1)}}
	s := testSession(desk)
	s.control(protocol.Control{T: protocol.ControlText, Text: "日本"})
	<-desk.started

	payload, _ := protocol.EncodeClip(protocol.Clip{Text: "pasted", PNG: []byte{0x89, 'P', 'N', 'G'}})
	sink, err := s.bulk(protocol.Bulk{T: protocol.BulkHeader, Kind: protocol.BulkClipboard, Size: int64(len(payload))})
	if err != nil {
		t.Fatal(err)
	}
	sink.Write(payload)
	if err := sink.Commit(); err != nil {
		t.Fatal(err)
	}
	paste := protocol.Control{T: protocol.ControlKey, Key: "v", Code: "KeyV", Down: true}
	s.control(paste)
	if got := desk.got(); len(got) != 0 {
		t.Fatalf("the clipboard or the paste key overtook the text being typed: %+v", got)
	}
	desk.gate <- struct{}{}
	waitIdle(t, &s.typing)
	want := []protocol.Control{{T: protocol.ControlText, Text: "日本"}, {T: protocol.ControlClipboard, Text: "pasted"}, paste}
	if got := desk.got(); !slices.Equal(got, want) {
		t.Errorf("desktop got %+v, want %+v", got, want)
	}
}

func TestClipboardOfATextOnlyDesktop(t *testing.T) {
	desk := &fakeDesktop{}
	s := testSession(desk)
	if err := s.setClip(protocol.Clip{HTML: "<p>only html</p>"}); err == nil {
		t.Error("an HTML-only clip went to a desktop that takes text")
	}
	s.setClip(protocol.Clip{Text: "text", PNG: []byte{1}})
	if !slices.Equal(desk.clipboard, []string{"text"}) {
		t.Errorf("desktop got %q, want the text of the clip", desk.clipboard)
	}
}

func TestForwardClipboardStopsWithTheDesktop(t *testing.T) {
	desk := &richDesktop{clips: make(chan protocol.Clip, 1)}
	s := testSession(desk)
	done := make(chan struct{})
	go func() { defer close(done); s.forwardClipboard(context.Background()) }()
	// No browser yet: the copy goes nowhere.
	desk.clips <- protocol.Clip{Text: "x", PNG: []byte{1}}
	close(desk.clips)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("forwardClipboard kept going after the desktop closed its channel")
	}

	ctx, stop := context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { defer close(done); testSession(&fakeDesktop{}).forwardClipboard(ctx) }()
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("forwardClipboard kept going after its context ended")
	}
}
