//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/yasinmiran/robin/internal/protocol"
)

// Controllers yields each connection a CLI makes to the session, in turn:
// the one SSH exec channel when the agent runs on stdio, or the connections
// a daemon accepts on its socket.
type Controllers interface {
	// Next blocks until the next controller connects. It returns an error
	// once no more can, which ends a session that has no controller.
	Next(ctx context.Context) (*protocol.Conn, error)
	// Changed is told whenever the session's state changes: the desktop is
	// up, a controller came or went. A daemon records it for the probe.
	Changed(protocol.Session)
}

// ErrNoControllers is what Next returns when the session will never get
// another controller.
var ErrNoControllers = errors.New("agent: no more controllers")

// Stdio yields one controller, speaking on r and w, and no more.
func Stdio(r io.Reader, w io.Writer) Controllers {
	return &stdio{conn: protocol.NewConn(r, w, protocol.DefaultPingInterval, protocol.DefaultIdleTimeout)}
}

type stdio struct {
	conn *protocol.Conn
	used bool
}

func (s *stdio) Next(ctx context.Context) (*protocol.Conn, error) {
	if s.used {
		return nil, ErrNoControllers
	}
	s.used = true
	return s.conn, nil
}

func (s *stdio) Changed(protocol.Session) {}

// controller is the session's current controller connection, swapped on a
// resume. Its methods go to whichever connection is current, from any
// goroutine.
type controller struct {
	cur atomic.Pointer[protocol.Conn]
}

func newController(c *protocol.Conn) *controller {
	ctl := &controller{}
	ctl.cur.Store(c)
	return ctl
}

// Send writes m to the current controller.
func (c *controller) Send(m protocol.Message) error { return c.cur.Load().Send(m) }

// sender is what fail needs: the whole connection or the session's view of it.
type sender interface{ Send(protocol.Message) error }

// serve runs the session's message loop for controller after controller,
// until one says bye, the keep window runs out with none attached, the
// desktop goes away or ctx ends. ready is sent again to each controller that
// resumes.
func (s *session) serve(ctx context.Context, cs Controllers, ready *protocol.Ready, desktopDone <-chan error, keep time.Duration, state protocol.Session) error {
	state.Connected = true
	cs.Changed(state)
	conn := s.conn.cur.Load()
	for {
		bye, err := s.relay(ctx, conn, desktopDone)
		if err != nil {
			return err
		}
		if bye {
			s.ended(conn, "the CLI ended the session")
			return nil
		}
		s.closePeer()
		s.ownerAway()
		conn.Close()
		if keep <= 0 {
			return nil
		}
		until := time.Now().Add(keep)
		s.log.Info("controller gone; keeping the desktop", "until", until.Format(time.RFC3339))
		state.Connected, state.Until = false, &until
		cs.Changed(state)
		conn, err = s.linger(ctx, cs, ready, desktopDone, until)
		switch {
		case errors.Is(err, errKeepRanOut):
			s.log.Info("keep window ran out; ending the session")
			return nil
		case errors.Is(err, ErrNoControllers):
			s.log.Info("no controller can come back; ending the session")
			return nil
		case err != nil:
			return err
		}
		s.ownerBack()
		state.Connected, state.Until = true, nil
		cs.Changed(state)
	}
}

// relay serves one controller until it says bye (true) or goes away (false,
// nil). The desktop dying or ctx ending is an error that ends the session.
func (s *session) relay(ctx context.Context, conn *protocol.Conn, desktopDone <-chan error) (bool, error) {
	msgs := make(chan protocol.Message)
	go func() {
		defer close(msgs)
		for {
			m, err := conn.Recv(ctx)
			if err != nil {
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
			s.ended(conn, "the agent was told to stop")
			return false, ctx.Err()
		case err := <-desktopDone:
			return false, fail(conn, fmt.Errorf("desktop went away: %w", err))
		case m, ok := <-msgs:
			if !ok {
				if ctx.Err() != nil {
					s.ended(conn, "the agent was told to stop")
					return false, ctx.Err()
				}
				s.log.Info("controller disconnected", "why", conn.Err())
				return false, nil
			}
			if done, err := s.handle(m); done || err != nil {
				return done, err
			}
		}
	}
}

// resumedReady is ready as a controller that resumes gets it: marked
// resumed, with the private session as it is now, which the page may have
// switched since the start, and the encoder in use now, which may have
// given way to software since.
func (s *session) resumedReady(ready *protocol.Ready) *protocol.Ready {
	r := *ready
	r.Resumed = true
	r.Private = s.private.snapshot()
	r.Encoder, r.EncoderError = s.encoder.inUse()
	return &r
}

// errKeepRanOut is linger's answer when the keep window closed with no
// controller back.
var errKeepRanOut = errors.New("agent: the keep window ran out")

// linger waits for the next controller until the keep window ends. It
// returns the resumed connection, or an error: errKeepRanOut, ErrNoControllers
// when none can come back, or why the desktop went away or ctx ended. A
// controller admit refuses is dropped; the wait goes on.
func (s *session) linger(ctx context.Context, cs Controllers, ready *protocol.Ready, desktopDone <-chan error, until time.Time) (*protocol.Conn, error) {
	type next struct {
		conn *protocol.Conn
		err  error
	}
	waitCtx, stopWait := context.WithCancel(ctx)
	arrivals := make(chan next, 1)
	defer func() {
		// A controller that arrived after the decision has nobody to serve it.
		stopWait()
		select {
		case a := <-arrivals:
			if a.conn != nil {
				a.conn.Close()
			}
		default:
		}
	}()
	go func() {
		c, err := cs.Next(waitCtx)
		arrivals <- next{c, err}
	}()
	expiry := time.NewTimer(time.Until(until))
	defer expiry.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case err := <-desktopDone:
			return nil, fmt.Errorf("desktop went away: %w", err)
		case <-expiry.C:
			return nil, errKeepRanOut
		case a := <-arrivals:
			if a.err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, ErrNoControllers
			}
			if err := s.admit(ctx, a.conn, ready.ID); err != nil {
				s.log.Warn("controller refused", "err", err)
				a.conn.Close()
				go func() {
					c, err := cs.Next(waitCtx)
					arrivals <- next{c, err}
				}()
				continue
			}
			s.conn.cur.Store(a.conn)
			if err := a.conn.Send(protocol.Message{Type: protocol.TypeReady, Ready: s.resumedReady(ready)}); err != nil {
				return nil, err
			}
			s.log.Info("controller resumed the session")
			return a.conn, nil
		}
	}
}

// admit reads the handshake of a controller picking up kept session id and
// does what its Resume asks (takeResume). An error refuses the controller,
// which has been told why if it is still there to hear it.
func (s *session) admit(ctx context.Context, conn *protocol.Conn, id string) error {
	r, err := awaitResume(ctx, conn, id)
	if err != nil {
		return err
	}
	if err := s.takeResume(r); err != nil {
		return fail(conn, err)
	}
	return nil
}

// awaitResume reads the handshake of a controller picking up a kept
// session: a Resume for this session at this protocol version.
func awaitResume(ctx context.Context, conn *protocol.Conn, id string) (protocol.Resume, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, startTimeout, errors.New("no resume message from the CLI"))
	defer cancel()
	m, err := conn.Recv(ctx)
	switch {
	case err != nil:
		return protocol.Resume{}, err
	case m.Type != protocol.TypeResume || m.Resume == nil:
		return protocol.Resume{}, fail(conn, fmt.Errorf("expected resume, got %q", m.Type))
	case m.Resume.Version != protocol.Version:
		return protocol.Resume{}, fail(conn, fmt.Errorf("the CLI speaks protocol %d, this agent %d", m.Resume.Version, protocol.Version))
	case m.Resume.ID != id:
		return protocol.Resume{}, fail(conn, fmt.Errorf("no session %q here", m.Resume.ID))
	}
	return *m.Resume, nil
}

// takeResume does what a resuming CLI asks of the session beyond giving it
// back: it turns on the private session's switches the CLI names, as Start
// would have, and checks that the session has the devices the CLI offers,
// which it has only if it started with them. An error refuses the resume
// and leaves the session as it was.
func (s *session) takeResume(r protocol.Resume) error {
	var flags, devices []string
	if r.Mic && !s.start.Mic {
		flags, devices = append(flags, "--mic"), append(devices, "a microphone")
	}
	if r.Camera && !s.start.Camera {
		flags, devices = append(flags, "--camera"), append(devices, "a camera")
	}
	if len(flags) > 0 {
		return fmt.Errorf("session %s started without %s, and a kept session keeps the devices it started with: resume it without %s, or end it with robin end and start another",
			r.ID, strings.Join(devices, " or "), strings.Join(flags, " and "))
	}
	return s.private.require(switches{Curtain: r.Curtain, Silent: r.Silent})
}

// ended tells the controller the session is over on purpose, so it does
// not try to come back.
func (s *session) ended(conn *protocol.Conn, why string) {
	if err := conn.Send(protocol.Message{Type: protocol.TypeEnded, Error: why}); err != nil {
		s.log.Debug("send ended", "err", err)
	}
}

// prepareRunDir returns this session's private directory: o.RunDir when the
// caller made one, else a fresh one named by pid under o.RunRoot. Either
// way, directories left by sessions whose process is gone are removed.
func prepareRunDir(o Options) (string, error) {
	root := o.RunRoot
	if o.RunDir != "" {
		root = filepath.Dir(o.RunDir)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	sweepRunRoot(root, o.RunDir)
	if o.RunDir != "" {
		return o.RunDir, nil
	}
	dir := filepath.Join(root, strconv.Itoa(os.Getpid()))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// sweepRunRoot removes every session directory under root but keep whose
// owner is gone. A daemon names its directory after its session: one with
// session.json or a socket stays as long as this user's process runs that
// daemon, the socket alone meaning it is still starting. One with neither
// was named by its pid, as a stdio session names its own, and stays while
// that pid is one this user can signal.
func sweepRunRoot(root, keep string) {
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		if dir == keep || !e.IsDir() {
			continue
		}
		if pid := sessionPID(dir); pid != 0 {
			if pid != os.Getpid() && !IsDaemon(pid, e.Name()) {
				os.RemoveAll(dir)
			}
			continue
		}
		if hasSocket(dir) {
			if FindDaemon(e.Name()) == 0 {
				os.RemoveAll(dir)
			}
			continue
		}
		pid, _ := strconv.Atoi(e.Name())
		if pid == 0 || pid == os.Getpid() {
			continue
		}
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH || err == syscall.EPERM {
			os.RemoveAll(dir)
		}
	}
}

// hasSocket reports whether dir holds a daemon's control socket.
func hasSocket(dir string) bool {
	fi, err := os.Lstat(filepath.Join(dir, SocketFile))
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

// sessionPID reads the daemon pid out of dir's session.json, or 0.
func sessionPID(dir string) int {
	st, err := ReadSession(dir)
	if err != nil {
		return 0
	}
	return st.PID
}
