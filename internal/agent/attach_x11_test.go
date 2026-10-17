//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/robin/internal/agent/desktop"
	"github.com/yasinmiran/robin/internal/agent/display"
	"github.com/yasinmiran/robin/internal/protocol"
)

// liveDisplay starts an X session for the agent to attach to, as the user's
// own desktop would be, with its cookie where $XAUTHORITY points.
func liveDisplay(t *testing.T, w, h int) *display.Display {
	t.Helper()
	if _, err := exec.LookPath("Xvfb"); err != nil {
		t.Skip("Xvfb not installed; run via internal/agent/x11/testdata/xtest.sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := display.Spawn(ctx, display.Options{RunDir: t.TempDir(), MaxWidth: w, MaxHeight: h, Session: "sleep 1000"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	t.Setenv("XAUTHORITY", d.XAuthority)
	return d
}

// xconn is a client of the live display that observes the agent's effects.
func xconn(t *testing.T, d *display.Display) (*xgb.Conn, xproto.Window) {
	t.Helper()
	x, err := xgb.NewConnDisplay(d.Name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(x.Close)
	return x, xproto.Setup(x).DefaultScreen(x).Root
}

func rootSize(t *testing.T, x *xgb.Conn, root xproto.Window) (int, int) {
	t.Helper()
	g, err := xproto.GetGeometry(x, xproto.Drawable(root)).Reply()
	if err != nil {
		t.Fatal(err)
	}
	return int(g.Width), int(g.Height)
}

func TestAttachX11(t *testing.T) {
	live := liveDisplay(t, 1280, 800)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	d, err := openDesktop(context.Background(), protocol.Start{Attach: live.Name}, t.TempDir(), "", nil, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	if d.mode != protocol.ModeAttach || d.name != live.Name {
		t.Errorf("opened %q as %q, want %q as %q", d.name, d.mode, live.Name, protocol.ModeAttach)
	}
	if w, h, err := d.desk.Resize(800, 600); !errors.Is(err, desktop.ErrFixedSize) || w != 1280 || h != 800 {
		t.Errorf("Resize = %dx%d, %v; want 1280x800, ErrFixedSize", w, h, err)
	}
	d.close()
	select {
	case err := <-d.done:
		t.Errorf("closing our side reported the desktop gone: %v", err)
	case err := <-live.Done():
		t.Fatalf("closing the attached desktop ended the session: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	// The X server going away ends the attached desktop.
	d, err = openDesktop(context.Background(), protocol.Start{Attach: live.Name}, t.TempDir(), "", nil, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	live.Close()
	select {
	case err := <-d.done:
		if err == nil {
			t.Error("done fired without saying why")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("done did not fire when the X server went away")
	}
	d.close()

	if _, err := openDesktop(context.Background(), protocol.Start{Attach: live.Name}, t.TempDir(), "", nil, nil, log); err == nil {
		t.Error("attached to a display that is gone")
	}
}

// TestSessionAttachesAndScales runs a session against a desktop larger than
// the session allows: the video shrinks to fit, the pointer lands where the
// page points on the full-size desktop, and the page cannot resize it.
func TestSessionAttachesAndScales(t *testing.T) {
	live := liveDisplay(t, 2560, 1440)
	x, root := xconn(t, live)
	h := startAgent(t)
	h.cli.Send(protocol.Message{Type: protocol.TypeStart, Start: &protocol.Start{
		Version: protocol.Version, Width: 1000, Height: 700, MaxWidth: 1280, MaxHeight: 720, FPS: 30, Attach: live.Name,
	}})
	ready := h.recv(protocol.TypeReady).Ready
	if ready.Mode != protocol.ModeAttach || ready.Display != live.Name || ready.Width != 2560 || ready.Height != 1440 {
		t.Fatalf("ready %+v", ready)
	}

	pc, dc, tracks := connectBrowser(t, h)
	select {
	case tr := <-tracks:
		if _, _, err := tr.ReadRTP(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("no video track; ICE state %s", pc.ICEConnectionState())
	}
	waitSize(t, h.sizes, 1280, 720)
	for deadline := time.Now().Add(10 * time.Second); dc.ReadyState() != webrtc.DataChannelStateOpen; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("control channel never opened")
		}
	}
	send := func(c protocol.Control) {
		b, _ := json.Marshal(c)
		if err := dc.SendText(string(b)); err != nil {
			t.Fatal(err)
		}
	}

	// Video pixel 640,360 shows desktop pixels 1280..1281, 720..721.
	send(protocol.Control{T: protocol.ControlMove, X: 640, Y: 360})
	var px, py int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		p, err := xproto.QueryPointer(x, root).Reply()
		if err != nil {
			t.Fatal(err)
		}
		if px, py = int(p.RootX), int(p.RootY); px >= 1280 && px <= 1281 && py >= 720 && py <= 721 {
			break
		}
	}
	if px < 1280 || px > 1281 || py < 720 || py > 721 {
		t.Errorf("pointer at %d,%d after a move to 640,360 on the video, want 1280..1281,720..721", px, py)
	}

	send(protocol.Control{T: protocol.ControlResize, W: 1000, H: 700})
	time.Sleep(500 * time.Millisecond)
	if w, hgt := rootSize(t, x, root); w != 2560 || hgt != 1440 {
		t.Errorf("the page resized the attached desktop to %dx%d", w, hgt)
	}
	for len(h.sizes) > 0 {
		if s := <-h.sizes; s != [2]int{1280, 720} {
			t.Errorf("encoder saw %dx%d", s[0], s[1])
		}
	}

	h.stdin.Close()
	select {
	case <-h.exited:
		if h.runErr != nil {
			t.Fatalf("Run: %v", h.runErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("agent did not exit")
	}
	if entries, _ := os.ReadDir(h.root); len(entries) != 0 {
		t.Errorf("run dirs left behind: %v", entries)
	}
	select {
	case err := <-live.Done():
		t.Fatalf("the attached desktop did not outlive the session: %v", err)
	default:
	}
	if w, hgt := rootSize(t, x, root); w != 2560 || hgt != 1440 {
		t.Errorf("desktop is %dx%d after the session", w, hgt)
	}
}

// connectBrowser plays the page: it offers a receive-only video transceiver
// and the control channel, and trades candidates through the harness.
func connectBrowser(t *testing.T, h *harness) (*webrtc.PeerConnection, *webrtc.DataChannel, <-chan *webrtc.TrackRemote) {
	t.Helper()
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	h.mu.Lock()
	h.pc = pc
	h.mu.Unlock()
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	dc, err := pc.CreateDataChannel(protocol.ControlChannel, nil)
	if err != nil {
		t.Fatal(err)
	}
	tracks := make(chan *webrtc.TrackRemote, 1)
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { tracks <- tr })
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		h.cli.Send(protocol.Message{Type: protocol.TypeCandidate, Candidate: &protocol.Candidate{
			Candidate: init.Candidate, SDPMid: init.SDPMid, SDPMLineIndex: init.SDPMLineIndex}})
	})
	offer, _ := pc.CreateOffer(nil)
	pc.SetLocalDescription(offer)
	h.cli.Send(protocol.Message{Type: protocol.TypeOffer, SDP: offer.SDP})
	answer := h.recv(protocol.TypeAnswer)
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		t.Fatal(err)
	}
	return pc, dc, tracks
}
