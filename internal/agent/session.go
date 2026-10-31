//go:build linux

// Package agent runs one robin session on the host: it speaks signaling
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
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/yasinmiran/robin/internal/agent/desktop"
	"github.com/yasinmiran/robin/internal/agent/peer"
	"github.com/yasinmiran/robin/internal/agent/share"
	"github.com/yasinmiran/robin/internal/protocol"
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
	// RunDir is this session's own directory, when the caller made it (a
	// daemon's, named by session id). Empty makes one under RunRoot.
	RunDir string
	// Keep is how long the session waits for a controller to come back
	// after one left without bye. Zero ends it with its controller.
	Keep time.Duration
	// FirstController bounds the wait for the first controller, so a daemon
	// whose proxy died before it connected does not wait for ever. Zero
	// waits startTimeout.
	FirstController time.Duration
	// StateDir keeps what outlives a session, such as the Wayland portal's
	// restore token. Empty keeps nothing.
	StateDir string
	Build    string
	// Encoders maps Start.Encoder names to constructors. It must hold
	// protocol.EncoderSoftware, the default.
	Encoders map[string]NewEncoderFunc
	// NewAudio captures the desktop's sound; nil when this build has none.
	NewAudio NewAudioFunc
	// NewMic and NewCamera give the host the browser's microphone and
	// camera; nil when this build has none.
	NewMic    NewMicFunc
	NewCamera NewCameraFunc
	// Keeper is the argv that makes this executable the child that undoes a
	// private session's changes if the agent dies (see keeper.New); without
	// it an attached desktop gets no private session.
	Keeper []string
	// Silence makes an attached desktop's room silent; nil when this build
	// cannot.
	Silence SilenceFunc
	// Door takes guests of other accounts on the host; nil shares nothing.
	// The session lets its guests go when it ends; the door is the
	// caller's to close.
	Door *share.Door
	Log  *slog.Logger
}

// Run serves one session over r and w (the SSH exec channel) until the CLI
// says bye, the channel closes or goes silent, the desktop goes away, or ctx
// ends. Everything it started is torn down before it returns.
func Run(ctx context.Context, r io.Reader, w io.Writer, o Options) error {
	return Serve(ctx, Stdio(r, w), o)
}

// firstController waits at most wait for the first controller.
func firstController(ctx context.Context, cs Controllers, wait time.Duration) (*protocol.Conn, error) {
	if wait <= 0 {
		wait = startTimeout
	}
	ctx, cancel := context.WithTimeoutCause(ctx, wait, fmt.Errorf("agent: no controller connected within %s", wait))
	defer cancel()
	conn, err := cs.Next(ctx)
	if err != nil && ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	return conn, err
}

// Serve runs one session for the controllers cs yields, one at a time: the
// first sends Start, each later one Resume. It returns when a controller
// says bye, the last one leaves and o.Keep runs out with no other, the
// desktop goes away, or ctx ends. Everything it started is torn down before
// it returns.
func Serve(ctx context.Context, cs Controllers, o Options) error {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	conn, err := firstController(ctx, cs, o.FirstController)
	if err != nil {
		return err
	}
	defer conn.Close()

	start, err := awaitStart(ctx, conn)
	if err != nil {
		return err
	}
	video, err := chooseEncoder(start, o.Encoders, log)
	if err != nil {
		return fail(conn, err)
	}
	runDir, err := prepareRunDir(o)
	if err != nil {
		return fail(conn, err)
	}
	defer os.RemoveAll(runDir)
	shared, err := openFiles(runDir, log)
	if err != nil {
		log.Warn("files unavailable", "err", err)
	} else {
		defer shared.close()
	}

	// Sound comes up before a spawned desktop so its programs inherit the
	// sound server's address.
	var sound AudioSource
	var desktopEnv []string
	var soundErr string
	if start.Audio || spawnMic(start) {
		if o.NewAudio == nil {
			soundErr = "this agent was built without audio"
			log.Warn("audio requested but this agent has no audio support")
		} else if src, env, err := o.NewAudio(ctx, AudioOptions{
			RunDir: runDir, Attached: start.Attach != "", Mic: spawnMic(start), NoCapture: !start.Audio, Log: log,
		}); err != nil {
			// Nothing from a failed start may reach the desktop, its env least of
			// all: it could point programs at a server that is not there.
			soundErr = err.Error()
			log.Warn("audio unavailable", "err", err)
		} else {
			sound, desktopEnv = src, env
			defer sound.Close()
		}
	}
	if shared != nil {
		desktopEnv = append(desktopEnv, shared.env()...)
	}

	// Opening can wait minutes on a consent dialog; a CLI that goes away
	// meanwhile must not leave the dialog up.
	openCtx, stopOpen := context.WithCancel(ctx)
	go func() {
		select {
		case <-conn.Done():
			stopOpen()
		case <-openCtx.Done():
		}
	}()
	notify := func(n protocol.Notice) {
		if err := conn.Send(protocol.Message{Type: protocol.TypeNotice, Notice: &n}); err != nil {
			log.Warn("send notice", "err", err)
		}
	}
	d, err := openDesktop(openCtx, start, runDir, o.StateDir, desktopEnv, notify, log)
	stopOpen()
	if err != nil {
		return fail(conn, err)
	}
	defer d.close()
	log.Info("desktop up", "mode", d.mode, "display", d.name, "session", d.session)

	mic, micReady := startMic(ctx, start, o.NewMic, sound, soundErr, log)
	if mic != nil {
		defer mic.Close()
	}
	camera, cameraReady := startCamera(ctx, start, o.NewCamera, log)
	if camera != nil {
		defer camera.Close()
	}
	var audio AudioSource
	var audioErr string
	if start.Audio {
		audio, audioErr = sound, soundErr
	}

	w0, h0 := fit(start.Width, start.Height, start.MaxWidth, start.MaxHeight)
	if w0 > 0 && h0 > 0 {
		if _, _, err := d.desk.Resize(w0, h0); err != nil && !errors.Is(err, desktop.ErrFixedSize) {
			log.Warn("initial resize", "err", err)
		}
	}

	guestUIDs := newUIDSet()
	tcpMux, tunnelAddr, err := listenTunnel(log, guestUIDs.has)
	if err != nil {
		return fail(conn, err)
	}
	defer tcpMux.Close()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fail(conn, err)
	}
	cert, err := webrtc.GenerateCertificate(key)
	if err != nil {
		return fail(conn, err)
	}

	st, err := newStream(d.desk, start.FPS, start.MaxWidth, start.MaxHeight, video.open, log)
	if err != nil {
		return fail(conn, err)
	}
	s := &session{
		conn: newController(conn), desk: d.desk, start: start, log: log, tcpMux: tcpMux, cert: cert, audio: audio != nil,
		stream:  st,
		encoder: video,
		cursors: map[uint32]protocol.Cursor{},
		files:   shared,
		mic:     mic,
		camera:  camera,
		shared:  &sharing{door: o.Door, owner: share.UserName(os.Getuid()), uids: guestUIDs},
	}
	if audio != nil {
		if s.sound, err = peer.NewAudioTrack(); err != nil {
			return fail(conn, err)
		}
	}
	// Registered after the microphone's and camera's Close, so it runs
	// before them: no peer, the guests' included, outlives the session.
	defer func() {
		s.endGuests()
		s.closePeer()
		s.endPlays()
	}()
	if s.private, err = openPrivate(d, start, o, audio, s.sendPrivate, log); err != nil {
		return fail(conn, err)
	}
	defer s.private.close()

	width, height := d.desk.Size()
	encoderName, encoderErr := video.inUse()
	ready := &protocol.Ready{
		Version: protocol.Version, AgentBuild: o.Build, Display: d.name, Session: d.session,
		Width: width, Height: height, TunnelAddr: tunnelAddr,
		Mode: d.mode, Audio: audio != nil, AudioError: audioErr, Encoder: encoderName, EncoderError: encoderErr,
		ID: filepath.Base(runDir), Keep: int(o.Keep / time.Second),
		Mic: micReady, Camera: cameraReady,
		Private: s.private.snapshot(),
	}
	if mm, ok := d.desk.(desktop.MultiMonitor); ok {
		ms := mm.MonitorState()
		ready.Monitors, ready.Monitor = ms.List, ms.Current
	}
	s.ready = ready
	if err := conn.Send(protocol.Message{Type: protocol.TypeReady, Ready: ready}); err != nil {
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
	run(s.serveFiles)
	run(s.forwardCursor)
	run(s.forwardMonitors)
	run(s.heartbeat)
	run(s.acceptGuests)
	if audio != nil {
		run(func(ctx context.Context) { s.forwardAudio(ctx, audio) })
	}

	return s.serve(ctx, cs, ready, d.done, o.Keep, protocol.Session{
		ID: ready.ID, PID: os.Getpid(), Build: o.Build, Mode: d.mode, Display: d.name, Desktop: d.session,
		Keep: ready.Keep, Started: time.Now(),
	})
}

type session struct {
	conn   *controller
	ready  *protocol.Ready // the owner's first Ready
	desk   desktop.Desktop
	start  protocol.Start // the owner's; s.mu guards Tunnel, which a resume sets
	log    *slog.Logger
	tcpMux ice.TCPMux
	cert   *webrtc.Certificate
	audio  bool
	stream *stream
	// encoder opens the stream's encoders and says which one Ready names.
	encoder *videoEncoder
	typing  lane
	// inputMu is held while a browser's control message applies. closePeer
	// takes it too, so that once it has let a browser go nothing that
	// browser sent is still applying, and nothing it sends later will.
	inputMu sync.Mutex
	// private is an attached desktop's curtain and silent room; nil for a
	// spawned one.
	private *private

	mic    Mic    // nil unless the session takes the browser's microphone
	camera Camera // and its camera

	// plays runs the Play of each track the browser sends; once playsEnded
	// is set, under playsMu, no more start.
	playsMu    sync.Mutex
	playsEnded bool
	plays      sync.WaitGroup

	mu sync.Mutex
	// owner is the owner's page, nil while none is connected, and gens
	// counts the pages' peers made so far.
	owner *viewer
	gens  uint64
	// cursors holds the last maxCursors pointer images, so a page that
	// connects later can be sent images the desktop reported before it
	// arrived. cursorOrder lists their IDs oldest first.
	cursors     map[uint32]protocol.Cursor
	cursorOrder []uint32
	cursor      uint32 // current pointer image
	// sound is the Opus track every page's peer sends; nil without sound.
	sound *webrtc.TrackLocalStaticSample
	// shared holds the guests; never nil.
	shared *sharing

	files   *fileShare   // nil when the session takes no files
	clipsIn atomic.Int32 // clipboard transfers from the browser under way
}

// maxCursors bounds the pointer images a session keeps. The X11 desktop
// sends each image once in a window of the same size and names it by ID
// after that, so this window must be no smaller and must forget in the
// order images arrived.
const maxCursors = 64

// keepCursor stores c as the newest image, forgetting the oldest beyond
// maxCursors. s.mu must be held.
func (s *session) keepCursor(c protocol.Cursor) {
	if _, ok := s.cursors[c.ID]; ok {
		s.cursorOrder = slices.DeleteFunc(s.cursorOrder, func(id uint32) bool { return id == c.ID })
	}
	s.cursors[c.ID] = c
	s.cursorOrder = append(s.cursorOrder, c.ID)
	if len(s.cursorOrder) > maxCursors {
		old := s.cursorOrder[0]
		s.cursorOrder = s.cursorOrder[1:]
		delete(s.cursors, old)
		if s.owner != nil {
			delete(s.owner.cursorsShown, old)
		}
		for _, g := range s.shared.guests {
			if g.view != nil {
				delete(g.view.cursorsShown, old)
			}
		}
	}
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
		if p := s.ownerPeer(); p != nil && m.Candidate != nil {
			// The peer logs the first candidate past its bound itself.
			if err := p.AddCandidate(*m.Candidate); err != nil && !errors.Is(err, peer.ErrTooManyCandidates) {
				s.log.Warn("remote candidate rejected", "err", err)
			}
		}
		return false, nil
	case protocol.TypeHangup:
		s.closePeer()
		return false, nil
	case protocol.TypeBye:
		return true, nil
	case protocol.TypeInvite:
		return false, s.answerInvite(m)
	}
	s.log.Warn("ignoring message", "type", m.Type)
	return false, nil
}

// applyInput takes key, pointer, wheel and release messages.
func (s *session) applyInput(m protocol.Control) {
	if err := s.desk.Input(s.stream.toDesktop(m)); err != nil {
		s.log.Debug("input", "err", err)
	}
}

func (s *session) applyResize(m protocol.Control) {
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
}

func (s *session) applyMonitor(m protocol.Control) {
	mm, ok := s.desk.(desktop.MultiMonitor)
	if !ok {
		return
	}
	if err := mm.ShowMonitor(m.Monitor); err != nil {
		s.log.Warn("show monitor", "err", err)
		return
	}
	s.log.Info("showing monitor", "monitor", m.Monitor)
}

// heartbeat tells every page every second that the media path still works;
// a page restarts ICE when they stop arriving.
func (s *session) heartbeat(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, v := range s.viewers() {
			v.peer.SendControl(protocol.Control{T: protocol.ControlHeartbeat})
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
			if len(c.PNG) > 0 || c.Hidden {
				s.keepCursor(c)
			}
			s.cursor = c.ID
			s.mu.Unlock()
			for _, v := range s.viewers() {
				s.sendCurrentCursor(v)
			}
		}
	}
}

// forwardMonitors tells the page whenever the monitors, or the one shown,
// change.
func (s *session) forwardMonitors(ctx context.Context) {
	mm, ok := s.desk.(desktop.MultiMonitor)
	if !ok {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-mm.MonitorUpdates():
			if !ok {
				return
			}
			s.sendMonitors()
		}
	}
}

// sendMonitors shows every page the desktop's monitors and the one shown,
// for a desktop that has them.
func (s *session) sendMonitors() {
	for _, v := range s.viewers() {
		s.sendMonitorsTo(v)
	}
}

func (s *session) sendMonitorsTo(v *viewer) {
	mm, ok := s.desk.(desktop.MultiMonitor)
	if !ok {
		return
	}
	ms := mm.MonitorState()
	if err := v.peer.SendControl(protocol.Control{T: protocol.ControlMonitors, Monitors: &ms}); err != nil && !errors.Is(err, peer.ErrNoControl) {
		s.log.Debug("send monitors", "err", err)
	}
}

// sendCurrentCursor shows v's page the current pointer image, with its
// PNG the first time this page sees it.
func (s *session) sendCurrentCursor(v *viewer) {
	s.mu.Lock()
	c, known := s.cursors[s.cursor]
	if !known {
		s.mu.Unlock()
		return
	}
	if v.cursorsShown[c.ID] {
		c.PNG = nil
	}
	s.mu.Unlock()
	if err := v.peer.SendControl(protocol.Control{T: protocol.ControlCursor, Cursor: &c}); err != nil {
		// Before the channel opens nothing is marked; ControlOpen resends.
		if !errors.Is(err, peer.ErrNoControl) {
			s.log.Debug("send cursor", "err", err)
		}
		return
	}
	s.mu.Lock()
	v.cursorsShown[c.ID] = true
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
			// One write reaches every page: the track is bound to each peer.
			if err := s.sound.WriteSample(media.Sample{Data: pkt, Duration: AudioFrame}); err != nil {
				s.log.Debug("write audio", "err", err)
			}
		}
	}
}

func awaitStart(ctx context.Context, conn *protocol.Conn) (protocol.Start, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, startTimeout, errors.New("no start message from the CLI"))
	defer cancel()
	m, err := conn.Recv(ctx)
	switch {
	case err != nil:
		return protocol.Start{}, err
	case m.Type != protocol.TypeStart || m.Start == nil:
		return protocol.Start{}, fail(conn, fmt.Errorf("expected start, got %q", m.Type))
	case m.Start.Version != protocol.Version:
		return protocol.Start{}, fail(conn, fmt.Errorf("the CLI speaks protocol %d, this agent %d", m.Start.Version, protocol.Version))
	}
	return normalize(*m.Start), nil
}

// normalize clamps the CLI's request to what the agent will do.
func normalize(s protocol.Start) protocol.Start {
	if s.MaxWidth <= 0 || s.MaxWidth > maxDimension {
		s.MaxWidth = protocol.DefaultMaxWidth
	}
	if s.MaxHeight <= 0 || s.MaxHeight > maxDimension {
		s.MaxHeight = protocol.DefaultMaxHeight
	}
	s.MaxWidth &^= 1
	s.MaxHeight &^= 1
	if s.FPS <= 0 || s.FPS > maxFPS {
		s.FPS = protocol.DefaultFPS
	}
	if s.MaxKbps <= 0 {
		s.MaxKbps = protocol.DefaultMaxKbps
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
func fail(conn sender, err error) error {
	conn.Send(protocol.Message{Type: protocol.TypeError, Error: err.Error()})
	return err
}
