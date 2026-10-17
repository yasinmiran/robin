//go:build linux

// Package agent runs one remotejump session on the host: it speaks signaling
// on stdio, owns the desktop, and streams it to whichever browser the CLI
// relays.
package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"github.com/yasinmiran/remotejump/internal/agent/desktop"
	"github.com/yasinmiran/remotejump/internal/agent/peer"
	"github.com/yasinmiran/remotejump/internal/protocol"
)

// Limits applied to whatever the CLI asks for.
const (
	maxDimension  = 3840
	maxFPS        = 60
	minBitrate    = 300_000
	startBitrate  = 2_000_000
	startTimeout  = 30 * time.Second
	clipboardSize = peer.MaxControlBytes - 1024 // room for the JSON envelope
)

// Options configure Run.
type Options struct {
	// RunRoot holds one private directory per live session. Directories left
	// by sessions whose process is gone are removed on start.
	RunRoot string
	Build   string
	// Encoders maps Start.Encoder names to constructors. It must hold
	// protocol.EncoderSoftware, the default.
	Encoders map[string]NewEncoderFunc
	// NewAudio captures the desktop's sound; nil when this build has none.
	NewAudio NewAudioFunc
	Log      *slog.Logger
}

// Run serves one session over r and w (the SSH exec channel) until the CLI
// says bye, the channel closes or goes silent, the desktop goes away, or ctx
// ends. Everything it started is torn down before it returns.
func Run(ctx context.Context, r io.Reader, w io.Writer, o Options) error {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	conn := protocol.NewConn(r, w, protocol.DefaultPingInterval, protocol.DefaultIdleTimeout)
	defer conn.Close()

	start, err := awaitStart(ctx, conn)
	if err != nil {
		return err
	}
	encoderName := start.Encoder
	if encoderName == "" {
		encoderName = protocol.EncoderSoftware
	}
	newEncoder, ok := o.Encoders[encoderName]
	if !ok {
		return fail(conn, fmt.Errorf("encoder %q is not available in this agent", encoderName))
	}
	runDir, err := makeRunDir(o.RunRoot)
	if err != nil {
		return fail(conn, err)
	}
	defer os.RemoveAll(runDir)

	// Sound comes up before a spawned desktop so its programs inherit the
	// sound server's address.
	var audio AudioSource
	var desktopEnv []string
	if start.Audio {
		if o.NewAudio == nil {
			log.Warn("audio requested but this agent has no audio support")
		} else if audio, desktopEnv, err = o.NewAudio(ctx, AudioOptions{
			RunDir: runDir, Attached: start.Attach != "", Log: log,
		}); err != nil {
			log.Warn("audio unavailable", "err", err)
			audio = nil
		} else {
			defer audio.Close()
		}
	}

	d, err := openDesktop(ctx, start, runDir, desktopEnv, log)
	if err != nil {
		return fail(conn, err)
	}
	defer d.close()
	log.Info("desktop up", "mode", d.mode, "display", d.name, "session", d.session)

	w0, h0 := fit(start.Width, start.Height, start.MaxWidth, start.MaxHeight)
	if w0 > 0 && h0 > 0 {
		if _, _, err := d.desk.Resize(w0, h0); err != nil && !errors.Is(err, desktop.ErrFixedSize) {
			log.Warn("initial resize", "err", err)
		}
	}

	// The tunnel listener is loopback-only: the SSH forward is the one way in.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fail(conn, fmt.Errorf("listen for ICE-TCP: %w", err))
	}
	tcpMux := ice.NewTCPMuxDefault(ice.TCPMuxParams{Listener: ln, ReadBufferSize: 64})
	defer tcpMux.Close()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fail(conn, err)
	}
	cert, err := webrtc.GenerateCertificate(key)
	if err != nil {
		return fail(conn, err)
	}

	s := &session{
		conn: conn, desk: d.desk, start: start, log: log, tcpMux: tcpMux, cert: cert, audio: audio != nil,
		stream:  newStream(d.desk, start.FPS, start.MaxWidth, start.MaxHeight, newEncoder, log),
		cursors: map[uint32]protocol.Cursor{},
	}
	defer s.closePeer()

	width, height := d.desk.Size()
	if err := conn.Send(protocol.Message{Type: protocol.TypeReady, Ready: &protocol.Ready{
		Version: protocol.Version, AgentBuild: o.Build, Display: d.name, Session: d.session,
		Width: width, Height: height, TunnelAddr: ln.Addr().String(),
		Mode: d.mode, Audio: audio != nil, Encoder: encoderName,
	}}); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	run := func(f func(context.Context)) {
		wg.Add(1)
		go func() { defer wg.Done(); f(ctx) }()
	}
	run(s.stream.run)
	run(s.forwardClipboard)
	run(s.forwardCursor)
	run(s.heartbeat)
	if audio != nil {
		run(func(ctx context.Context) { s.forwardAudio(ctx, audio) })
	}

	msgs := make(chan protocol.Message)
	go func() {
		for {
			m, err := conn.Recv()
			if err != nil {
				close(msgs)
				return
			}
			select {
			case msgs <- m:
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-d.done:
			return fail(conn, fmt.Errorf("desktop went away: %w", err))
		case m, ok := <-msgs:
			if !ok {
				if err := conn.Err(); err != nil && !errors.Is(err, io.EOF) {
					return err
				}
				return nil
			}
			if done, err := s.handle(m); done || err != nil {
				return err
			}
		}
	}
}

type session struct {
	conn   *protocol.Conn
	desk   desktop.Desktop
	start  protocol.Start
	log    *slog.Logger
	tcpMux ice.TCPMux
	cert   *webrtc.Certificate
	audio  bool
	stream *stream

	mu   sync.Mutex
	peer *peer.Peer
	// cursors holds every pointer image seen, so a page that connects later
	// can be sent images the desktop reported before it arrived.
	cursors      map[uint32]protocol.Cursor
	cursor       uint32          // current pointer image
	cursorsShown map[uint32]bool // images the current page already has
}

// handle processes one CLI message; done reports a clean end of session.
func (s *session) handle(m protocol.Message) (done bool, err error) {
	switch m.Type {
	case protocol.TypeOffer:
		p, err := s.currentPeer()
		if err != nil {
			return false, fail(s.conn, err)
		}
		answer, err := p.Answer(m.SDP)
		if err != nil {
			// A bad offer is the browser's problem, not a reason to drop the
			// desktop: report it and wait for the next one.
			s.log.Warn("offer rejected", "err", err)
			return false, s.conn.Send(protocol.Message{Type: protocol.TypeError, Error: err.Error()})
		}
		return false, s.conn.Send(protocol.Message{Type: protocol.TypeAnswer, SDP: answer})
	case protocol.TypeCandidate:
		s.mu.Lock()
		p := s.peer
		s.mu.Unlock()
		if p != nil && m.Candidate != nil {
			if err := p.AddCandidate(*m.Candidate); err != nil {
				s.log.Warn("remote candidate rejected", "err", err)
			}
		}
		return false, nil
	case protocol.TypeHangup:
		s.closePeer()
		return false, nil
	case protocol.TypeBye:
		return true, nil
	}
	s.log.Warn("ignoring message", "type", m.Type)
	return false, nil
}

// currentPeer returns the live peer, creating one for a fresh browser.
func (s *session) currentPeer() (*peer.Peer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peer != nil {
		return s.peer, nil
	}
	maxBitrate := max(s.start.MaxKbps*1000, startBitrate)
	p, err := peer.New(peer.Config{
		STUN: s.start.STUN, TCPMux: s.tcpMux, IncludeLoopback: true, Certificate: s.cert, Audio: s.audio,
		MinBitrate: minBitrate, StartBitrate: min(startBitrate, maxBitrate), MaxBitrate: maxBitrate,
		Log: s.log,
	}, peer.Handlers{
		Candidate: func(c protocol.Candidate) {
			if err := s.conn.Send(protocol.Message{Type: protocol.TypeCandidate, Candidate: &c}); err != nil {
				s.log.Warn("send candidate", "err", err)
			}
		},
		Control:     s.control,
		ControlOpen: s.sendCurrentCursor,
		Keyframe:    s.stream.requestKeyframe,
		Bitrate:     s.stream.setBitrate,
		State: func(st webrtc.PeerConnectionState) {
			if st == webrtc.PeerConnectionStateConnected {
				s.desk.Input(protocol.Control{T: protocol.ControlRelease})
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create peer: %w", err)
	}
	s.peer = p
	s.cursorsShown = map[uint32]bool{}
	s.stream.setPeer(p)
	return p, nil
}

func (s *session) closePeer() {
	s.mu.Lock()
	p := s.peer
	s.peer = nil
	s.mu.Unlock()
	if p == nil {
		return
	}
	s.stream.setPeer(nil)
	// Whatever the departing browser held down must not stay pressed.
	s.desk.Input(protocol.Control{T: protocol.ControlRelease})
	p.Close()
}

func (s *session) control(m protocol.Control) {
	switch m.T {
	case protocol.ControlKey, protocol.ControlMove, protocol.ControlButton, protocol.ControlWheel, protocol.ControlRelease:
		if err := s.desk.Input(m); err != nil {
			s.log.Debug("input", "err", err)
		}
	case protocol.ControlResize:
		w, h := fit(m.W, m.H, s.start.MaxWidth, s.start.MaxHeight)
		if w <= 0 || h <= 0 {
			return
		}
		gw, gh, err := s.desk.Resize(w, h)
		switch {
		case errors.Is(err, desktop.ErrFixedSize):
			// An attached desktop keeps its size; the page scales the video.
			return
		case err != nil:
			s.log.Warn("resize", "w", w, "h", h, "err", err)
			return
		}
		s.log.Info("resized", "w", gw, "h", gh)
		s.stream.requestKeyframe()
	case protocol.ControlClipboard:
		if len(m.Text) > clipboardSize {
			s.log.Warn("clipboard from browser too large", "bytes", len(m.Text))
			return
		}
		if err := s.desk.SetClipboard(m.Text); err != nil {
			s.log.Warn("set clipboard", "err", err)
		}
	}
}

// heartbeat tells the page every second that the media path still works; the
// page restarts ICE when they stop arriving.
func (s *session) heartbeat(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if p := s.currentOrNil(); p != nil {
			p.SendControl(protocol.Control{T: protocol.ControlHeartbeat})
		}
	}
}

func (s *session) currentOrNil() *peer.Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peer
}

func (s *session) forwardClipboard(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case text, ok := <-s.desk.Clipboard():
			if !ok {
				return
			}
			if len(text) > clipboardSize {
				s.log.Warn("remote clipboard too large to send", "bytes", len(text))
				continue
			}
			if p := s.currentOrNil(); p != nil {
				if err := p.SendControl(protocol.Control{T: protocol.ControlClipboard, Text: text}); err != nil && !errors.Is(err, peer.ErrNoControl) {
					s.log.Warn("send clipboard", "err", err)
				}
			}
		}
	}
}

func (s *session) forwardCursor(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case c, ok := <-s.desk.Cursor():
			if !ok {
				return
			}
			s.mu.Lock()
			if len(c.PNG) > 0 {
				s.cursors[c.ID] = c
			}
			s.cursor = c.ID
			s.mu.Unlock()
			s.sendCurrentCursor()
		}
	}
}

// sendCurrentCursor shows the page the current pointer image, with its PNG
// the first time this page sees it.
func (s *session) sendCurrentCursor() {
	s.mu.Lock()
	p := s.peer
	c, known := s.cursors[s.cursor]
	if p == nil || !known {
		s.mu.Unlock()
		return
	}
	if s.cursorsShown[c.ID] {
		c.PNG = nil
	}
	s.mu.Unlock()
	if err := p.SendControl(protocol.Control{T: protocol.ControlCursor, Cursor: &c}); err != nil {
		// Before the channel opens nothing is marked; ControlOpen resends.
		if !errors.Is(err, peer.ErrNoControl) {
			s.log.Debug("send cursor", "err", err)
		}
		return
	}
	s.mu.Lock()
	if s.peer == p {
		s.cursorsShown[c.ID] = true
	}
	s.mu.Unlock()
}

func (s *session) forwardAudio(ctx context.Context, src AudioSource) {
	for {
		select {
		case <-ctx.Done():
			return
		case pkt, ok := <-src.Packets():
			if !ok {
				return
			}
			if p := s.currentOrNil(); p != nil {
				if err := p.WriteAudio(pkt, AudioFrame); err != nil {
					s.log.Debug("write audio", "err", err)
				}
			}
		}
	}
}

func awaitStart(ctx context.Context, conn *protocol.Conn) (protocol.Start, error) {
	got := make(chan protocol.Message, 1)
	errc := make(chan error, 1)
	go func() {
		m, err := conn.Recv()
		if err != nil {
			errc <- err
			return
		}
		got <- m
	}()
	select {
	case <-ctx.Done():
		return protocol.Start{}, ctx.Err()
	case err := <-errc:
		return protocol.Start{}, err
	case <-time.After(startTimeout):
		return protocol.Start{}, errors.New("agent: no start message")
	case m := <-got:
		if m.Type != protocol.TypeStart || m.Start == nil {
			return protocol.Start{}, fail(conn, fmt.Errorf("agent: expected start, got %q", m.Type))
		}
		if m.Start.Version != protocol.Version {
			return protocol.Start{}, fail(conn, fmt.Errorf("agent speaks protocol %d, CLI speaks %d", protocol.Version, m.Start.Version))
		}
		return normalize(*m.Start), nil
	}
}

// normalize clamps the CLI's request to what the agent will do.
func normalize(s protocol.Start) protocol.Start {
	if s.MaxWidth <= 0 || s.MaxWidth > maxDimension {
		s.MaxWidth = 1920
	}
	if s.MaxHeight <= 0 || s.MaxHeight > maxDimension {
		s.MaxHeight = 1080
	}
	s.MaxWidth &^= 1
	s.MaxHeight &^= 1
	if s.FPS <= 0 || s.FPS > maxFPS {
		s.FPS = 30
	}
	if s.MaxKbps <= 0 {
		s.MaxKbps = 10_000
	}
	return s
}

// fit scales w x h down to fit inside maxW x maxH, keeping the aspect ratio,
// and rounds to even dimensions.
func fit(w, h, maxW, maxH int) (int, int) {
	if w <= 0 || h <= 0 {
		return 0, 0
	}
	if w > maxW || h > maxH {
		scale := min(float64(maxW)/float64(w), float64(maxH)/float64(h))
		w, h = int(float64(w)*scale), int(float64(h)*scale)
	}
	return max(w&^1, 2), max(h&^1, 2)
}

// fail tells the CLI why the session is ending, then returns err.
func fail(conn *protocol.Conn, err error) error {
	conn.Send(protocol.Message{Type: protocol.TypeError, Error: err.Error()})
	return err
}

// makeRunDir creates this session's private directory under root, after
// sweeping directories whose owning process no longer exists.
func makeRunDir(root string) (string, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
	dir := filepath.Join(root, strconv.Itoa(os.Getpid()))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}
