//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/robin/internal/agent/wayland/wltest"
	"github.com/yasinmiran/robin/internal/protocol"
)

// TestSessionAttachesToWayland runs a whole session against the GNOME
// Wayland session internal/agent/wayland/testdata/wltest.sh provides. The
// consent notice reaches the CLI before Ready, frames reach the encoder at
// the monitor's size, and the page is told to hide its own pointer, since
// the picture carries the remote one.
func TestSessionAttachesToWayland(t *testing.T) {
	if os.Getenv("ROBIN_WAYLAND_PROBE") == "" {
		t.Skip("needs a GNOME Wayland session; run internal/agent/wayland/testdata/wltest.sh")
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &harness{
		t: t, stdin: inW, root: t.TempDir(), sizes: make(chan [2]int, 64),
		exited: make(chan struct{}), msgs: make(chan protocol.Message, 256),
		cli: protocol.NewConn(outR, inW, protocol.DefaultPingInterval, 0),
	}
	state := t.TempDir()
	enc := &fakeEncoder{sizes: h.sizes}
	go func() {
		defer close(h.exited)
		h.runErr = Run(context.Background(), inR, outW, Options{
			RunRoot: h.root, StateDir: state, Build: "test",
			Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
			Encoders: map[string]NewEncoderFunc{protocol.EncoderSoftware: func(w, hgt, fps, bps int) (Encoder, error) { return enc, nil }},
		})
		outW.Close()
	}()
	go h.route()
	defer inW.Close()

	h.cli.Send(protocol.Message{Type: protocol.TypeStart, Start: &protocol.Start{
		Version: protocol.Version, Attach: protocol.AttachWayland, Width: 800, Height: 600,
		MaxWidth: 1920, MaxHeight: 1080, FPS: 30,
	}})
	notice := h.recv(protocol.TypeNotice).Notice
	if notice == nil || notice.Kind != protocol.NoticeConsent || notice.Wait <= 0 {
		t.Fatalf("notice %+v, want consent with a wait", notice)
	}
	answered := make(chan error, 1)
	go func() { answered <- wltest.AnswerDialog(true) }()
	ready := h.recv(protocol.TypeReady).Ready
	if err := <-answered; err != nil {
		t.Fatal(err)
	}
	if ready.Mode != protocol.ModeAttach || ready.Display != protocol.AttachWayland || ready.Width != 1280 || ready.Height != 720 {
		t.Fatalf("ready %+v", ready)
	}
	if _, err := os.Stat(filepath.Join(state, waylandTokenFile)); err != nil {
		t.Errorf("restore token not kept in the state dir: %v", err)
	}

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
	defer pc.Close()
	h.mu.Lock()
	h.pc = pc
	h.mu.Unlock()
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	dc, err := pc.CreateDataChannel(protocol.ControlChannel, nil)
	if err != nil {
		t.Fatal(err)
	}
	cursors := make(chan protocol.Cursor, 4)
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		var c protocol.Control
		if json.Unmarshal(msg.Data, &c) == nil && c.T == protocol.ControlCursor && c.Cursor != nil {
			cursors <- *c.Cursor
		}
	})
	tracks := make(chan *webrtc.TrackRemote, 1)
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { tracks <- tr })
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			init := c.ToJSON()
			h.cli.Send(protocol.Message{Type: protocol.TypeCandidate, Candidate: &protocol.Candidate{
				Candidate: init.Candidate, SDPMid: init.SDPMid, SDPMLineIndex: init.SDPMLineIndex}})
		}
	})
	offer, _ := pc.CreateOffer(nil)
	pc.SetLocalDescription(offer)
	h.cli.Send(protocol.Message{Type: protocol.TypeOffer, SDP: offer.SDP})
	answer := h.recv(protocol.TypeAnswer)
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		t.Fatal(err)
	}

	select {
	case tr := <-tracks:
		if _, _, err := tr.ReadRTP(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("no video track; ICE state %s", pc.ICEConnectionState())
	}
	waitSize(t, h.sizes, 1280, 720)
	select {
	case c := <-cursors:
		if !c.Hidden {
			t.Errorf("cursor %+v, want hidden", c)
		}
	case <-time.After(10 * time.Second):
		t.Error("the page was not told to hide its pointer")
	}

	inW.Close()
	select {
	case <-h.exited:
		if h.runErr != nil {
			t.Fatalf("Run: %v", h.runErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("agent did not exit")
	}
}
