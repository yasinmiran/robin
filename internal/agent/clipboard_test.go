//go:build linux

package agent

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/protocol"
)

// richDesktop is a fakeDesktop with a rich clipboard.
type richDesktop struct {
	fakeDesktop
	setMu sync.Mutex
	set   []protocol.Clip
	block chan struct{} // when set, SetClipboardContent waits until it closes
	clips chan protocol.Clip
}

func (r *richDesktop) SetClipboardContent(c protocol.Clip) error {
	if r.block != nil {
		<-r.block
	}
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

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestClipboardFromTheBrowserKeepsOnlyTheLatest(t *testing.T) {
	desk := &richDesktop{block: make(chan struct{})}
	s := testSession(desk)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go s.setClips(ctx)

	// The first set waits on the desktop; of those that come meanwhile only
	// the newest is set after it.
	s.control(protocol.Control{T: protocol.ControlClipboard, Text: "first"})
	waitFor(t, "the first set to start", func() bool { return len(s.clips) == 0 })
	s.control(protocol.Control{T: protocol.ControlClipboard, Text: "second"})
	s.putClip(protocol.Clip{Text: "third", HTML: "<b>third</b>"})
	close(desk.block)
	waitFor(t, "two sets", func() bool { return len(desk.setSoFar()) == 2 })
	got := desk.setSoFar()
	if got[0].Text != "first" || got[1].HTML != "<b>third</b>" {
		t.Errorf("set %+v, want first and then third", got)
	}
}

func TestClipboardBulkFromTheBrowser(t *testing.T) {
	desk := &richDesktop{}
	s := testSession(desk)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go s.setClips(ctx)

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
	waitFor(t, "the clipboard set", func() bool { return len(desk.setSoFar()) == 1 })
	if got := desk.setSoFar()[0]; got.Text != want.Text || string(got.PNG) != string(want.PNG) {
		t.Errorf("set %+v, want %+v", got, want)
	}

	sink, _ = s.bulk(protocol.Bulk{T: protocol.BulkHeader, Kind: protocol.BulkClipboard, Size: 3})
	sink.Write([]byte{9, 'x', 'y'})
	if err := sink.Commit(); !errors.Is(err, protocol.ErrClip) {
		t.Errorf("Commit of a malformed payload: %v, want ErrClip", err)
	}
	sink.Abort(nil)
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
