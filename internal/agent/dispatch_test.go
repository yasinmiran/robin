//go:build linux

package agent

import (
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yasinmiran/robin/internal/agent/desktop"
	"github.com/yasinmiran/robin/internal/agent/peer"
	"github.com/yasinmiran/robin/internal/protocol"
)

// fakeDesktop records what the session asks of it.
type fakeDesktop struct {
	mu        sync.Mutex
	inputs    []protocol.Control
	sizes     [][2]int
	clipboard []string
	resizeErr error
	inputErr  error
}

func (f *fakeDesktop) Size() (int, int) { return 800, 600 }
func (f *fakeDesktop) Resize(w, h int) (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sizes = append(f.sizes, [2]int{w, h})
	return w, h, f.resizeErr
}
func (f *fakeDesktop) Capture(bool) (*desktop.Frame, bool, error) { return nil, false, nil }
func (f *fakeDesktop) Input(m protocol.Control) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, m)
	return f.inputErr
}
func (f *fakeDesktop) SetClipboard(text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clipboard = append(f.clipboard, text)
	return errors.New("no clipboard owner")
}
func (f *fakeDesktop) Clipboard() <-chan string       { return nil }
func (f *fakeDesktop) Cursor() <-chan protocol.Cursor { return nil }
func (f *fakeDesktop) Close() error                   { return nil }

// fakeMonitors is a fakeDesktop of several monitors.
type fakeMonitors struct {
	fakeDesktop
	shown   []string
	showErr error
}

func (f *fakeMonitors) MonitorState() protocol.Monitors { return protocol.Monitors{} }
func (f *fakeMonitors) MonitorUpdates() <-chan struct{} { return nil }
func (f *fakeMonitors) ShowMonitor(name string) error {
	f.shown = append(f.shown, name)
	return f.showErr
}

func testSession(desk desktop.Desktop) *session {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &session{
		desk: desk, start: protocol.Start{MaxWidth: 1280, MaxHeight: 720}, log: log,
		stream: newStream(desk, 30, 1280, 720, nil, log),
	}
}

func TestControlDispatch(t *testing.T) {
	t.Run("input", func(t *testing.T) {
		desk := &fakeDesktop{}
		s := testSession(desk)
		input := []protocol.Control{
			{T: protocol.ControlKey, Key: "a", Code: "KeyA", Down: true},
			{T: protocol.ControlMove, X: 3, Y: 4},
			{T: protocol.ControlButton, Button: 2, Down: true},
			{T: protocol.ControlWheel, DY: 120},
			{T: protocol.ControlRelease},
		}
		for _, m := range input {
			s.control(m)
		}
		desk.inputErr = errors.New("display gone")
		s.control(protocol.Control{T: protocol.ControlRelease})
		if !slices.Equal(desk.inputs, append(input, protocol.Control{T: protocol.ControlRelease})) {
			t.Errorf("desktop got input %+v", desk.inputs)
		}
	})

	// Resize fits the request into the start limits and asks for a keyframe.
	t.Run("resize", func(t *testing.T) {
		desk := &fakeDesktop{}
		s := testSession(desk)
		s.control(protocol.Control{T: protocol.ControlResize, W: 2560, H: 1440})
		if !s.stream.keyframe.Load() {
			t.Error("a resize did not ask for a keyframe")
		}
		s.stream.keyframe.Store(false)
		desk.resizeErr = desktop.ErrFixedSize
		s.control(protocol.Control{T: protocol.ControlResize, W: 1000, H: 700})
		desk.resizeErr = errors.New("randr says no")
		s.control(protocol.Control{T: protocol.ControlResize, W: 900, H: 600})
		s.control(protocol.Control{T: protocol.ControlResize}) // nothing to fit
		if want := [][2]int{{1280, 720}, {1000, 700}, {900, 600}}; !slices.Equal(desk.sizes, want) {
			t.Errorf("desktop resized to %v, want %v", desk.sizes, want)
		}
		if s.stream.keyframe.Load() {
			t.Error("a resize that failed asked for a keyframe")
		}
	})

	t.Run("clipboard", func(t *testing.T) {
		desk := &fakeDesktop{}
		s := testSession(desk)
		s.control(protocol.Control{T: protocol.ControlClipboard, Text: "hi"})
		s.control(protocol.Control{T: protocol.ControlClipboard, Text: strings.Repeat("x", clipboardSize+1)})
		if !slices.Equal(desk.clipboard, []string{"hi"}) {
			t.Errorf("desktop clipboard set to %d texts, want only the one in bounds", len(desk.clipboard))
		}
	})

	t.Run("monitor", func(t *testing.T) {
		// A desktop of one picture ignores monitor choices, and so does the
		// session for message types nobody handles.
		s := testSession(&fakeDesktop{})
		s.control(protocol.Control{T: protocol.ControlMonitor, Monitor: "HDMI-1"})
		s.control(protocol.Control{T: "from-a-newer-page"})
		s.control(protocol.Control{T: protocol.ControlHeartbeat})

		mm := &fakeMonitors{}
		s = testSession(mm)
		s.control(protocol.Control{T: protocol.ControlMonitor, Monitor: "HDMI-1"})
		mm.showErr = errors.New("no such monitor")
		s.control(protocol.Control{T: protocol.ControlMonitor, Monitor: "VGA-9"})
		if !slices.Equal(mm.shown, []string{"HDMI-1", "VGA-9"}) {
			t.Errorf("monitors shown %v", mm.shown)
		}
	})
}

type discardSink struct{}

func (discardSink) Write(p []byte) (int, error) { return len(p), nil }
func (discardSink) Commit() error               { return nil }
func (discardSink) Abort(error)                 {}

// dispatchKind is the bulk kind these tests hand a handler for. It has a
// limit like any other, so TestBulkKindsHaveLimits holds while they run.
const dispatchKind = "dispatch-test"

func init() { protocol.RegisterBulkKind(dispatchKind, 1<<10) }

func discardBulk(*session, protocol.Bulk) (peer.BulkSink, error) { return discardSink{}, nil }

func TestBulkDispatch(t *testing.T) {
	var got []protocol.Bulk
	bulkHandlers[dispatchKind] = func(s *session, h protocol.Bulk) (peer.BulkSink, error) {
		got = append(got, h)
		return discardSink{}, nil
	}
	defer delete(bulkHandlers, dispatchKind)
	s := testSession(&fakeDesktop{})

	h := protocol.Bulk{T: protocol.BulkHeader, Kind: dispatchKind, Size: 3}
	if sink, err := s.bulk(h); err != nil || sink == nil {
		t.Fatalf("registered kind: %v, %v", sink, err)
	}
	if len(got) != 1 || got[0] != h {
		t.Errorf("handler got %+v", got)
	}
	if _, err := s.bulk(protocol.Bulk{T: protocol.BulkHeader, Kind: "nobody"}); !errors.Is(err, protocol.ErrBulkKind) {
		t.Errorf("unhandled kind: %v, want ErrBulkKind", err)
	}
}

// TestBulkKindsHaveLimits catches a kind with a handler but no limit, which
// would refuse every transfer of it at runtime.
func TestBulkKindsHaveLimits(t *testing.T) {
	for kind := range bulkHandlers {
		if _, ok := protocol.BulkLimit(kind); !ok {
			t.Errorf("bulk kind %q has a handler but no limit from protocol.RegisterBulkKind", kind)
		}
	}
}

func TestDispatchRefusesBadHandlers(t *testing.T) {
	defer delete(bulkHandlers, dispatchKind)
	for name, register := range map[string]func(){
		"control twice": func() { handleControl(protocol.ControlKey, (*session).applyInput) },
		"bulk twice": func() {
			handleBulk(dispatchKind, discardBulk)
			handleBulk(dispatchKind, discardBulk)
		},
		"nil control": func() { handleControl("dispatch-nil", nil) },
		"nil bulk":    func() { handleBulk("dispatch-nil", nil) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("registered without a panic")
				}
			}()
			register()
		})
	}
	if _, ok := controlHandlers["dispatch-nil"]; ok {
		t.Error("a nil control handler was kept")
	}
	if _, ok := bulkHandlers["dispatch-nil"]; ok {
		t.Error("a nil bulk handler was kept")
	}
}
