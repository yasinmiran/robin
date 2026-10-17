//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/robin/internal/agent/yuv"
	"github.com/yasinmiran/robin/internal/protocol"
)

// fakeEncoder records the frame sizes it is asked to encode and emits a
// fixed access unit, so the pipeline runs without openh264.
type fakeEncoder struct {
	sizes chan [2]int
}

func (f *fakeEncoder) Encode(img *yuv.I420, _ time.Duration, _ bool) ([]byte, error) {
	select {
	case f.sizes <- [2]int{img.Width, img.Height}:
	default:
	}
	return []byte{0, 0, 0, 1, 0x65, 0x88, 0x84, 0x21}, nil
}
func (f *fakeEncoder) SetBitrate(int) error { return nil }
func (f *fakeEncoder) Close() error         { return nil }

type harness struct {
	t     *testing.T
	cli   *protocol.Conn
	stdin io.WriteCloser
	root  string
	sizes chan [2]int

	// exited is closed when Run returns; runErr is its result.
	exited chan struct{}
	runErr error

	// One reader routes agent messages: candidates to the current browser
	// peer, everything else to msgs.
	msgs chan protocol.Message
	mu   sync.Mutex
	pc   *webrtc.PeerConnection
}

// startAgent runs Run against pipes, as the CLI would over SSH. configure
// may adjust the Options first.
func startAgent(t *testing.T, configure ...func(*Options)) *harness {
	t.Helper()
	if _, err := exec.LookPath("Xvfb"); err != nil {
		t.Skip("Xvfb not installed; run via internal/agent/x11/testdata/xtest.sh")
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &harness{
		t: t, stdin: inW, root: t.TempDir(), sizes: make(chan [2]int, 64),
		exited: make(chan struct{}), msgs: make(chan protocol.Message, 256),
		// Ping like the real CLI, or the agent's idle watchdog ends the session.
		cli: protocol.NewConn(outR, inW, protocol.DefaultPingInterval, 0),
	}
	enc := &fakeEncoder{sizes: h.sizes}
	o := Options{
		RunRoot: h.root, Build: "test", Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		Encoders: map[string]NewEncoderFunc{protocol.EncoderSoftware: func(w, hgt, fps, bps int) (Encoder, error) { return enc, nil }},
	}
	for _, c := range configure {
		c(&o)
	}
	go func() {
		defer close(h.exited)
		h.runErr = Run(context.Background(), inR, outW, o)
		outW.Close()
	}()
	go h.route()
	t.Cleanup(func() {
		inW.Close()
		select {
		case <-h.exited:
		case <-time.After(10 * time.Second):
		}
	})
	return h
}

func (h *harness) route() {
	defer close(h.msgs)
	for {
		m, err := h.cli.Recv(context.Background())
		if err != nil {
			return
		}
		if m.Type == protocol.TypeCandidate {
			h.mu.Lock()
			pc := h.pc
			h.mu.Unlock()
			if pc != nil && m.Candidate != nil && m.Candidate.Candidate != "" {
				pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: m.Candidate.Candidate, SDPMid: m.Candidate.SDPMid, SDPMLineIndex: m.Candidate.SDPMLineIndex})
			}
			continue
		}
		h.msgs <- m
	}
}

func (h *harness) recv(want protocol.Type) protocol.Message {
	h.t.Helper()
	for {
		var m protocol.Message
		var ok bool
		select {
		case m, ok = <-h.msgs:
		case <-time.After(20 * time.Second):
			h.t.Fatalf("timed out waiting for %s", want)
		}
		if !ok {
			h.t.Fatalf("agent closed the channel while waiting for %s (Run: %v)", want, h.runErr)
		}
		if m.Type == want {
			return m
		}
		if m.Type == protocol.TypeError {
			h.t.Fatalf("agent error while waiting for %s: %s", want, m.Error)
		}
	}
}

func (h *harness) start(configure ...func(*protocol.Start)) *protocol.Ready {
	h.t.Helper()
	start := protocol.Start{Version: protocol.Version, Width: 800, Height: 600, MaxWidth: 1280, MaxHeight: 720, FPS: 30}
	for _, c := range configure {
		c(&start)
	}
	if err := h.cli.Send(protocol.Message{Type: protocol.TypeStart, Start: &start}); err != nil {
		h.t.Fatal(err)
	}
	return h.recv(protocol.TypeReady).Ready
}

// waitGone fails unless Run returned and the session left nothing behind.
func (h *harness) waitGone(display string) {
	h.t.Helper()
	select {
	case <-h.exited:
		if h.runErr != nil {
			h.t.Fatalf("Run: %v", h.runErr)
		}
	case <-time.After(10 * time.Second):
		h.t.Fatal("agent did not exit")
	}
	entries, _ := os.ReadDir(h.root)
	if len(entries) != 0 {
		h.t.Fatalf("run dirs left behind: %v", entries)
	}
	num := strings.TrimPrefix(display, ":")
	if _, err := os.Stat("/tmp/.X" + num + "-lock"); !errors.Is(err, os.ErrNotExist) {
		h.t.Fatalf("Xvfb for %s still holds its lock (%v)", display, err)
	}
}

func TestSessionTearsDownOnStdinEOF(t *testing.T) {
	h := startAgent(t)
	ready := h.start()
	if ready.Version != protocol.Version || ready.Display == "" || ready.TunnelAddr == "" {
		t.Fatalf("ready %+v", ready)
	}
	if !strings.HasPrefix(ready.TunnelAddr, "127.0.0.1:") {
		t.Fatalf("tunnel listener %q must be loopback-only", ready.TunnelAddr)
	}
	if ready.Width != 800 || ready.Height != 600 {
		t.Fatalf("initial size %dx%d, want 800x600", ready.Width, ready.Height)
	}
	if ready.Mode != protocol.ModeSpawn || ready.Encoder != protocol.EncoderSoftware || ready.Audio {
		t.Fatalf("ready reports mode %q, encoder %q, audio %v", ready.Mode, ready.Encoder, ready.Audio)
	}
	dirs, _ := filepath.Glob(filepath.Join(h.root, "*", "Xauthority"))
	if len(dirs) != 1 {
		t.Fatalf("run dir missing its Xauthority: %v", dirs)
	}
	h.stdin.Close()
	h.waitGone(ready.Display)
}

func TestSessionEndsOnBye(t *testing.T) {
	h := startAgent(t)
	ready := h.start()
	if err := h.cli.Send(protocol.Message{Type: protocol.TypeBye}); err != nil {
		t.Fatal(err)
	}
	h.waitGone(ready.Display)
}

func TestSessionRejectsWrongVersion(t *testing.T) {
	h := startAgent(t)
	h.cli.Send(protocol.Message{Type: protocol.TypeStart, Start: &protocol.Start{Version: protocol.Version + 1}})
	m, ok := <-h.msgs
	// The CLI shows this as "agent: <message>", so it names no agent itself.
	want := fmt.Sprintf("the CLI speaks protocol %d, this agent %d", protocol.Version+1, protocol.Version)
	if !ok || m.Type != protocol.TypeError || m.Error != want {
		t.Fatalf("got %+v, want error %q", m, want)
	}
	select {
	case <-h.exited:
		if h.runErr == nil {
			t.Fatal("Run accepted a mismatched protocol version")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("agent did not exit")
	}
}

func TestSessionWantsStartFirst(t *testing.T) {
	h := startAgent(t)
	h.cli.Send(protocol.Message{Type: protocol.TypeOffer, SDP: "v=0"})
	m, ok := <-h.msgs
	if want := `expected start, got "offer"`; !ok || m.Type != protocol.TypeError || m.Error != want {
		t.Fatalf("got %+v, want error %q", m, want)
	}
}

// TestSessionStreamsToABrowser drives a full offer/answer through the session
// with a Pion peer playing the browser, then resizes over the control channel
// and reconnects after a hangup, as a page reload does.
func TestSessionStreamsToABrowser(t *testing.T) {
	h := startAgent(t)
	ready := h.start()
	cursors := make(chan protocol.Cursor, 16)

	connect := func() (*webrtc.PeerConnection, *webrtc.DataChannel, chan *webrtc.TrackRemote) {
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
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			var c protocol.Control
			if json.Unmarshal(msg.Data, &c) == nil && c.T == protocol.ControlCursor {
				select {
				case cursors <- *c.Cursor:
				default:
				}
			}
		})
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

	pc, dc, tracks := connect()
	var tr *webrtc.TrackRemote
	select {
	case tr = <-tracks:
	case <-time.After(15 * time.Second):
		t.Fatalf("no video track; ICE state %s", pc.ICEConnectionState())
	}
	if _, _, err := tr.ReadRTP(); err != nil {
		t.Fatal(err)
	}
	waitSize(t, h.sizes, 800, 600)
	// The page gets the pointer image, PNG included, as soon as its channel
	// opens.
	select {
	case c := <-cursors:
		if len(c.PNG) == 0 {
			t.Fatalf("first cursor %d came without its image", c.ID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no cursor sent to the page")
	}

	deadline := time.Now().Add(10 * time.Second)
	for dc.ReadyState() != webrtc.DataChannelStateOpen && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := json.Marshal(protocol.Control{T: protocol.ControlResize, W: 1000, H: 700})
	dc.SendText(string(b))
	waitSize(t, h.sizes, 1000, 700)
	// Larger than the session maximum: scaled down to fit, keeping aspect.
	b, _ = json.Marshal(protocol.Control{T: protocol.ControlResize, W: 2560, H: 1440})
	dc.SendText(string(b))
	waitSize(t, h.sizes, 1280, 720)

	// A reload: the CLI hangs up the old peer, a new page offers again.
	h.cli.Send(protocol.Message{Type: protocol.TypeHangup})
	pc.Close()
	_, _, tracks = connect()
	select {
	case tr = <-tracks:
	case <-time.After(15 * time.Second):
		t.Fatal("no video track after reconnect")
	}
	if _, _, err := tr.ReadRTP(); err != nil {
		t.Fatal(err)
	}

	h.stdin.Close()
	h.waitGone(ready.Display)
}

func waitSize(t *testing.T, sizes chan [2]int, w, h int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	var last [2]int
	for {
		select {
		case s := <-sizes:
			if s == [2]int{w, h} {
				return
			}
			last = s
		case <-deadline:
			t.Fatalf("encoder never saw %dx%d (last %dx%d)", w, h, last[0], last[1])
		}
	}
}

func TestFit(t *testing.T) {
	for _, tc := range []struct{ w, h, mw, mh, ww, wh int }{
		{800, 600, 1920, 1080, 800, 600},
		{1001, 701, 1920, 1080, 1000, 700},
		{2560, 1440, 1920, 1080, 1920, 1080},
		{3000, 1000, 1920, 1080, 1920, 640},
		{0, 600, 1920, 1080, 0, 0},
	} {
		if gw, gh := fit(tc.w, tc.h, tc.mw, tc.mh); gw != tc.ww || gh != tc.wh {
			t.Errorf("fit(%d,%d into %dx%d) = %dx%d, want %dx%d", tc.w, tc.h, tc.mw, tc.mh, gw, gh, tc.ww, tc.wh)
		}
	}
}

func TestCursorCacheForgetsTheOldest(t *testing.T) {
	s := &session{cursors: map[uint32]protocol.Cursor{}, cursorsShown: map[uint32]bool{}}
	for id := uint32(1); id <= maxCursors; id++ {
		s.keepCursor(protocol.Cursor{ID: id, PNG: []byte{1}})
		s.cursorsShown[id] = true
	}
	// The desktop sends 1 again after forgetting it itself: it is the newest.
	s.keepCursor(protocol.Cursor{ID: 1, PNG: []byte{2}})
	s.keepCursor(protocol.Cursor{ID: maxCursors + 1, PNG: []byte{1}})

	if len(s.cursors) != maxCursors || len(s.cursorOrder) != maxCursors {
		t.Fatalf("kept %d images, %d in order; want %d", len(s.cursors), len(s.cursorOrder), maxCursors)
	}
	if _, ok := s.cursors[2]; ok || s.cursorsShown[2] {
		t.Error("the oldest image, 2, is still kept")
	}
	if c, ok := s.cursors[1]; !ok || c.PNG[0] != 2 {
		t.Errorf("image 1 = %+v, %v; want the one sent again", c, ok)
	}
	if !s.cursorsShown[1] {
		t.Error("the page lost track of image 1, which it still has")
	}
}

func TestSessionRejectsUnknownEncoder(t *testing.T) {
	h := startAgent(t)
	h.cli.Send(protocol.Message{Type: protocol.TypeStart, Start: &protocol.Start{Version: protocol.Version, Encoder: "quantum"}})
	m, ok := <-h.msgs
	if !ok || m.Type != protocol.TypeError || !strings.Contains(m.Error, "quantum") {
		t.Fatalf("got %+v", m)
	}
}
