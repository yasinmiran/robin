//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jezek/xgb/xproto"
	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/robin/internal/agent/desktop"
	"github.com/yasinmiran/robin/internal/agent/display"
	"github.com/yasinmiran/robin/internal/agent/yuv"
	"github.com/yasinmiran/robin/internal/protocol"
)

// Two monitors side by side on a 1920x1080 live display, RIGHT primary.
var (
	leftMonitor  = protocol.Monitor{Name: "LEFT", X: 0, Y: 0, W: 800, H: 1080}
	rightMonitor = protocol.Monitor{Name: "RIGHT", X: 800, Y: 0, W: 1120, H: 1080, Primary: true}
)

// xrandr runs xrandr against the live display, as its user would.
func xrandr(t *testing.T, d *display.Display, args ...string) {
	t.Helper()
	cmd := exec.Command("xrandr", args...)
	cmd.Env = append(os.Environ(), "DISPLAY="+d.Name, "XAUTHORITY="+d.XAuthority)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("xrandr %v: %v: %s", args, err, out)
	}
}

func monitorGeometry(m protocol.Monitor) string {
	return fmt.Sprintf("%d/%dx%d/%d+%d+%d", m.W, m.W*254/960, m.H, m.H*254/960, m.X, m.Y)
}

// liveMonitors starts a live display split into leftMonitor and rightMonitor.
func liveMonitors(t *testing.T) *display.Display {
	t.Helper()
	live := liveDisplay(t, 1920, 1080)
	xrandr(t, live, "--setmonitor", "*RIGHT", monitorGeometry(rightMonitor), "screen")
	xrandr(t, live, "--setmonitor", "LEFT", monitorGeometry(leftMonitor), "none")
	return live
}

func TestAttachedX11Monitors(t *testing.T) {
	live := liveMonitors(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	open := func(monitor string) (*openedDesktop, desktop.MultiMonitor) {
		t.Helper()
		d, err := openDesktop(context.Background(), protocol.Start{Attach: live.Name, Monitor: monitor}, t.TempDir(), "", nil, nil, log)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(d.close)
		mm, ok := d.desk.(desktop.MultiMonitor)
		if !ok {
			t.Fatal("an attached X11 desktop has no monitors")
		}
		return d, mm
	}

	d, mm := open("")
	st := mm.MonitorState()
	if st.Current != "RIGHT" || len(st.List) != 2 {
		t.Fatalf("state %+v, want RIGHT shown of two", st)
	}
	if w, h := d.desk.Size(); w != rightMonitor.W || h != rightMonitor.H {
		t.Errorf("Size %dx%d, want the primary monitor's %dx%d", w, h, rightMonitor.W, rightMonitor.H)
	}
	second := st.List[1].Name

	// A choice by number counts in RandR's order.
	d2, mm2 := open("2")
	if got := mm2.MonitorState().Current; got != second {
		t.Errorf("--monitor 2 showed %s, want %s", got, second)
	}
	d2.close()
	d3, _ := open("all")
	if w, h := d3.desk.Size(); w != 1920 || h != 1080 {
		t.Errorf("--monitor all: Size %dx%d, want 1920x1080", w, h)
	}
	d3.close()
	if _, err := openDesktop(context.Background(), protocol.Start{Attach: live.Name, Monitor: "HDMI-9"}, t.TempDir(), "", nil, nil, log); err == nil ||
		!strings.Contains(err.Error(), "LEFT") || !strings.Contains(err.Error(), "RIGHT") {
		t.Errorf("--monitor HDMI-9: %v, want an error listing LEFT and RIGHT", err)
	}

	// Showing another monitor reports the change.
	if err := mm.ShowMonitor("LEFT"); err != nil {
		t.Fatal(err)
	}
	waitUpdate(t, mm, "LEFT")
	if w, h := d.desk.Size(); w != leftMonitor.W || h != leftMonitor.H {
		t.Errorf("Size %dx%d after showing LEFT, want %dx%d", w, h, leftMonitor.W, leftMonitor.H)
	}
	if err := mm.ShowMonitor("nowhere"); err == nil {
		t.Error("showed a monitor that does not exist")
	}

	// The monitor shown going away brings back the default one. Deleting a
	// monitor raises no RandR event, so this finds it by asking again.
	xrandr(t, live, "--delmonitor", "LEFT")
	waitUpdate(t, mm, "RIGHT")
	if st := mm.MonitorState(); len(st.List) != 1 {
		t.Errorf("monitors %+v after LEFT went away", st.List)
	}
}

// waitUpdate waits for an update that shows want.
func waitUpdate(t *testing.T, mm desktop.MultiMonitor, want string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-mm.MonitorUpdates():
			if mm.MonitorState().Current == want {
				return
			}
		case <-deadline:
			t.Fatalf("no update showing %s; state %+v", want, mm.MonitorState())
		}
	}
}

// encode is one Encode call: the frame size and whether it was a keyframe.
type encode struct {
	w, h int
	key  bool
}

type recordingEncoder struct{ calls chan encode }

func (r *recordingEncoder) Encode(img *yuv.I420, _ time.Duration, key bool) ([]byte, error) {
	select {
	case r.calls <- encode{img.Width, img.Height, key}:
	default:
	}
	return []byte{0, 0, 0, 1, 0x65, 0x88, 0x84, 0x21}, nil
}
func (r *recordingEncoder) SetBitrate(int) error { return nil }
func (r *recordingEncoder) Close() error         { return nil }

// TestSessionSwitchesMonitors plays a page that switches monitors: the
// agent tells it which monitors there are, the video takes the size of the
// monitor shown and starts with a keyframe, and the pointer lands on that
// monitor.
func TestSessionSwitchesMonitors(t *testing.T) {
	live := liveMonitors(t)
	x, root := xconn(t, live)
	rec := &recordingEncoder{calls: make(chan encode, 256)}
	h := startAgent(t, func(o *Options) {
		o.Encoders = map[string]NewEncoderFunc{protocol.EncoderSoftware: func(int, int, int, int) (Encoder, error) { return rec, nil }}
	})
	h.cli.Send(protocol.Message{Type: protocol.TypeStart, Start: &protocol.Start{
		Version: protocol.Version, MaxWidth: 1920, MaxHeight: 1080, FPS: 30, Attach: live.Name,
	}})
	ready := h.recv(protocol.TypeReady).Ready
	if ready.Monitor != "RIGHT" || len(ready.Monitors) != 2 || ready.Width != rightMonitor.W {
		t.Fatalf("ready %+v", ready)
	}

	pc, dc, tracks := connectBrowser(t, h)
	monitors := make(chan protocol.Monitors, 16)
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		var c protocol.Control
		if json.Unmarshal(msg.Data, &c) == nil && c.T == protocol.ControlMonitors && c.Monitors != nil {
			monitors <- *c.Monitors
		}
	})
	select {
	case tr := <-tracks:
		if _, _, err := tr.ReadRTP(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("no video track; ICE state %s", pc.ICEConnectionState())
	}
	waitMonitors := func(want string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case m := <-monitors:
				if m.Current == want && len(m.List) == 2 {
					return
				}
			case <-deadline:
				t.Fatalf("the page was never told %s is shown", want)
			}
		}
	}
	waitKeyframe := func(m protocol.Monitor) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case e := <-rec.calls:
				if e.w == m.W && e.h == m.H {
					if !e.key {
						t.Fatalf("the first %dx%d frame was not a keyframe", m.W, m.H)
					}
					return
				}
			case <-deadline:
				t.Fatalf("no frame of %s's %dx%d", m.Name, m.W, m.H)
			}
		}
	}
	send := func(c protocol.Control) {
		b, _ := json.Marshal(c)
		if err := dc.SendText(string(b)); err != nil {
			t.Fatal(err)
		}
	}
	pointerAt := func(wantX, wantY int) {
		t.Helper()
		var px, py int
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(30 * time.Millisecond) {
			p, err := xproto.QueryPointer(x, root).Reply()
			if err != nil {
				t.Fatal(err)
			}
			if px, py = int(p.RootX), int(p.RootY); px == wantX && py == wantY {
				return
			}
		}
		t.Errorf("pointer at %d,%d, want %d,%d", px, py, wantX, wantY)
	}

	waitMonitors("RIGHT")
	waitKeyframe(rightMonitor)
	send(protocol.Control{T: protocol.ControlMove, X: 10, Y: 20})
	pointerAt(rightMonitor.X+10, 20)

	send(protocol.Control{T: protocol.ControlMonitor, Monitor: "LEFT"})
	waitMonitors("LEFT")
	waitKeyframe(leftMonitor)
	send(protocol.Control{T: protocol.ControlMove, X: 10, Y: 20})
	pointerAt(10, 20)

	send(protocol.Control{T: protocol.ControlMonitor, Monitor: protocol.MonitorAll})
	waitMonitors(protocol.MonitorAll)
	waitKeyframe(protocol.Monitor{Name: "all", W: 1920, H: 1080})
}
