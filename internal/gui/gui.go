// Package gui runs `robin gui`: it connects over SSH, probes the host,
// deploys and starts the agent, forwards the ICE-TCP tunnel, serves the
// browser page and relays signaling between the page and the agent. It also
// runs `robin ls` and `robin end`, which look after the sessions a host
// keeps between CLI connections.
package gui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/yasinmiran/robin/internal/agentbin"
	"github.com/yasinmiran/robin/internal/deploy"
	"github.com/yasinmiran/robin/internal/probe"
	"github.com/yasinmiran/robin/internal/protocol"
	"github.com/yasinmiran/robin/internal/relay"
	"github.com/yasinmiran/robin/internal/sshmux"
	"github.com/yasinmiran/robin/internal/tail"
	"github.com/yasinmiran/robin/internal/web"
)

// Options are the user's choices for one session. Zero values pick defaults.
type Options struct {
	Dest    string
	SSHPath string
	SSHArgs []string
	// NoMux gives every ssh command and forward a connection of its own
	// instead of one ControlMaster; see sshmux.Options.NoMux.
	NoMux bool

	Transport           relay.Transport
	MaxWidth, MaxHeight int
	FPS                 int
	MaxKbps             int
	Session             string
	// Attach streams a running desktop instead of spawning one: an X11
	// display, "auto", or protocol.AttachWayland.
	Attach string
	// Monitor is the monitor of an attached X11 desktop to show first; see
	// protocol.Start.Monitor.
	Monitor string
	Audio   bool
	Encoder string
	// HiDPI sizes the remote display in the browser's device pixels.
	HiDPI bool
	STUN  []string

	// Keep is how long the host keeps the desktop after this CLI goes
	// without ending the session, for a later robin gui to resume. Zero
	// lets the desktop's mode pick: protocol.DefaultKeep for a spawned
	// desktop, none for an attached one. KeepNone keeps nothing.
	Keep time.Duration
	// Resume names the kept session to pick up. Empty resumes the one kept
	// spawned session when there is exactly one, unless New is set, which
	// always starts another.
	Resume string
	New    bool

	Listen    string // page address, default 127.0.0.1:0
	NoBrowser bool   // print the URL on stdout instead of opening a browser
	Verbose   bool

	Stdout, Stderr io.Writer
	Log            *slog.Logger
}

// KeepNone as Options.Keep ends the session with this CLI, whatever the
// desktop's mode.
const KeepNone time.Duration = -1

const readyTimeout = 60 * time.Second

// Reconnect backoff: the first wait after a lost connection and the most
// between two tries.
const (
	reconnectFirst = time.Second
	reconnectMax   = 15 * time.Second
)

// Run holds one session until ctx ends, the session ends on the host, or
// the page detaches. A lost SSH connection is redialed for as long as the
// host keeps the desktop. Its error, like everything it writes to Stderr,
// is safe to print on a terminal, whatever the host sent.
func Run(ctx context.Context, o Options) (err error) {
	o, log := o.fill()
	defer func() {
		if err != nil {
			err = printableError{err}
		}
	}()
	h, err := connect(ctx, o, log)
	if err != nil {
		return err
	}
	defer h.close()
	report := h.report
	if o.Attach == "auto" {
		if o.Attach, err = pickAttach(report); err != nil {
			return err
		}
		log.Info("attaching", "to", o.Attach)
	}
	if missing := report.SpawnMissing(); o.Attach == "" && len(missing) > 0 {
		return fmt.Errorf("%s cannot run a headless desktop yet: missing %s\ninstall with: %s",
			o.Dest, strings.Join(missing, ", "), report.InstallHint())
	}
	if err := report.CheckEncoder(o.Encoder); err != nil {
		return fmt.Errorf("%s: %w", o.Dest, err)
	}
	if o.Attach == protocol.AttachWayland {
		if err := checkWayland(o.Dest, report); err != nil {
			return err
		}
	}
	a, path, err := h.deployAgent(ctx)
	if err != nil {
		return err
	}
	resume, err := pickSession(o, report, a.Name())
	if err != nil {
		return err
	}
	keep := keepWindow(o)
	if resume == "" && keep > 0 {
		if note := keepNote(report); note != "" {
			fmt.Fprintf(o.Stderr, "robin: %s: %s\n", o.Dest, note)
		}
	}

	ag, err := startAgent(ctx, h.m, path, serveArgs{verbose: o.Verbose, keep: keep, resume: resume, detach: detachMode(report)}, o.Stderr)
	if err != nil {
		return err
	}
	defer func() { ag.stop() }()

	var ready *protocol.Ready
	start, picked := startMessage(o, report)
	if resume == "" {
		if picked != "" {
			log.Info("encoder picked", "encoder", start.Encoder, "why", picked)
		}
		if err := ag.conn.Send(protocol.Message{Type: protocol.TypeStart, Start: &start}); err != nil {
			return ag.explain(err)
		}
		ready, err = ag.awaitReady(ctx, hostOf(o.Dest), o.Stderr)
	} else {
		ready, err = ag.resume(ctx, resume)
	}
	if err != nil {
		return err
	}
	log.Info("agent ready", "mode", ready.Mode, "display", ready.Display, "session", ready.Session,
		"size", fmt.Sprintf("%dx%d", ready.Width, ready.Height), "monitor", ready.Monitor,
		"encoder", ready.Encoder, "audio", ready.Audio, "id", ready.ID, "keep", ready.Keep, "resumed", ready.Resumed)
	if ready.EncoderError != "" {
		log.Info("encoder fell back to software", "why", ready.EncoderError)
	}
	if o.Audio && !ready.Audio && !ready.Resumed {
		fmt.Fprintf(o.Stderr, "robin: no sound from %s: %s\n", o.Dest, ready.AudioError)
	}

	t, err := openTunnel(ctx, h.m, ready.TunnelAddr, o.Transport, log)
	if err != nil {
		return h.abandon(err, ag, ready, path)
	}
	defer t.Close()

	srv, err := web.Listen(web.Options{Addr: o.Listen})
	if err != nil {
		return h.abandon(err, ag, ready, path)
	}
	serveCtx, stopServe := context.WithCancel(ctx)
	defer stopServe()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(serveCtx) }()

	if o.NoBrowser {
		fmt.Fprintln(o.Stdout, srv.URL())
	} else if err := openBrowser(srv.URL()); err != nil {
		fmt.Fprintf(o.Stderr, "open this URL in your browser: %s\n", srv.URL())
	}
	fmt.Fprintf(o.Stderr, "robin: %s; press Ctrl+C to end the session\n", sessionNote(o.Dest, ready))

	b := newBridge(ag.conn, relay.New(relay.Policy{Transport: o.Transport, TunnelPort: t.Port()}),
		protocol.Config{ICEServers: iceServers(start.STUN), HiDPI: o.HiDPI, Keep: ready.Keep}, log)
	go b.accept(ctx, srv)
	defer b.close()

	for {
		err = b.run(ctx, ag.conn, h.m.Done(), serveErr)
		switch {
		case ctx.Err() != nil:
			// Ctrl+C in a terminal reaches ssh too, so the master may die
			// first; the session must end either way.
			return h.end(ag, ready.ID, keepUntil(ready), path)
		case errors.Is(err, errPageEnded):
			// The page only asked; the host has to confirm before the page
			// is told the desktop is gone.
			if err := h.end(ag, ready.ID, keepUntil(ready), path); err != nil {
				return err
			}
			b.ended(err.Error())
			log.Info("session ended", "why", err)
			return nil
		case errors.Is(err, errDetached):
			ag.stop()
			fmt.Fprintf(o.Stderr, "robin: desktop kept on %s for %s; robin gui %s resumes it, robin end %s ends it\n",
				o.Dest, keepDuration(ready.Keep), o.Dest, o.Dest)
			return nil
		case errors.Is(err, errEnded):
			b.ended(err.Error())
			log.Info("session ended", "why", err)
			return nil
		case ready.Keep == 0 || !resumable(err):
			return h.abandon(ag.explain(err), ag, ready, path)
		}
		until := keepUntil(ready)
		b.reconnecting()
		fmt.Fprintf(o.Stderr, "robin: %v; reconnecting to %s until %s (Ctrl+C ends the session)\n", err, o.Dest, until.Format(time.Kitchen))
		ag.stop()
		if ag, ready, err = h.reconnect(ctx, path, ready.ID, until, t); err != nil {
			return err
		}
		fmt.Fprintf(o.Stderr, "robin: reconnected to %s\n", o.Dest)
	}
}

// keepWindow is how long the host is asked to keep the desktop.
func keepWindow(o Options) time.Duration {
	switch {
	case o.Keep == KeepNone:
		return 0
	case o.Keep > 0:
		return o.Keep
	case o.Attach != "":
		return 0
	}
	return protocol.DefaultKeep
}

// keepUntil is when the host lets a desktop go that lost its controller
// now, or the zero time for a desktop that goes with its controller.
func keepUntil(r *protocol.Ready) time.Time {
	if r.Keep <= 0 {
		return time.Time{}
	}
	return time.Now().Add(time.Duration(r.Keep) * time.Second)
}

// fill applies the defaults and wraps the terminal writers.
func (o Options) fill() (Options, *slog.Logger) {
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	// The host's text (the agent's log and errors, the probe's findings,
	// what ssh prints) leaves through these two ways only.
	o.Stderr = printableWriter{o.Stderr}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	return o, log
}

// host is the SSH connection to the host and what the probe found there.
type host struct {
	o      Options
	log    *slog.Logger
	m      *sshmux.Master
	report *probe.Report
}

// connect dials the host and probes it.
func connect(ctx context.Context, o Options, log *slog.Logger) (*host, error) {
	log.Info("connecting", "dest", o.Dest)
	m, err := dial(ctx, o)
	if err != nil {
		return nil, err
	}
	report, err := runProbe(ctx, m)
	if err != nil {
		m.Close()
		return nil, err
	}
	log.Info("probed host", "distro", report.Distro.ID+" "+report.Distro.VersionID, "arch", report.Arch, "cpus", report.CPUs, "agentdir", report.AgentDir)
	if report.AgentDir == "" {
		m.Close()
		var why []string
		for _, r := range report.AgentDirRejected {
			why = append(why, r.Path+": "+r.Reason)
		}
		return nil, fmt.Errorf("no usable directory for the agent on %s (%s)", o.Dest, strings.Join(why, "; "))
	}
	return &host{o: o, log: log, m: m, report: report}, nil
}

func (h *host) close() {
	if h.m != nil {
		h.m.Close()
		h.m = nil
	}
}

// deployAgent puts this CLI's agent for the host's architecture in place and
// returns it and its path there.
func (h *host) deployAgent(ctx context.Context) (agentbin.Agent, string, error) {
	a, err := agentbin.For(h.report.Arch)
	if err != nil {
		return agentbin.Agent{}, "", err
	}
	path, err := deploy.Ensure(ctx, h.m, deploy.Target{
		Dir: h.report.AgentDir, PresentHash: h.report.AgentHash(a.Name()), SHA256Tool: h.report.SHA256Tool,
	}, a)
	if err != nil {
		return agentbin.Agent{}, "", fmt.Errorf("deploy agent: %w", err)
	}
	h.log.Info("agent in place", "path", path)
	return a, path, nil
}

// end finishes session id on purpose: bye over the connection when it is
// still up, else the agent's end command over the master, else over a fresh
// connection, since a desktop left kept by mistake would stay for hours.
// until is when the host would let the desktop go on its own; the zero
// time says it goes with its controller, so a lost bye costs nothing.
func (h *host) end(ag *agentProc, id string, until time.Time, path string) error {
	if ag.bye() || until.IsZero() {
		return nil
	}
	fmt.Fprintf(h.o.Stderr, "robin: ending the kept session on %s\n", h.o.Dest)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var err error
	if h.m != nil && !h.m.Dead() {
		err = endSession(ctx, h.m, path, id)
	} else {
		err = errors.New("the connection is gone")
	}
	if err != nil && !errors.Is(err, errNoSession) {
		h.close()
		var m *sshmux.Master
		if m, err = h.dial(ctx); err == nil {
			h.m = m
			err = endSession(ctx, m, path, id)
		}
	}
	if errors.Is(err, errNoSession) {
		// The bye got through after all, and the session is gone.
		return nil
	}
	if err != nil {
		return fmt.Errorf("could not end session %s on %s: %w\nit ends by itself at %s; robin end %s ends it sooner",
			id, h.o.Dest, err, until.Format(time.Kitchen), h.o.Dest)
	}
	return nil
}

// abandon ends the session the host runs when the CLI cannot go on with
// it, and returns err, with what came of the end when that failed too.
func (h *host) abandon(err error, ag *agentProc, ready *protocol.Ready, path string) error {
	if endErr := h.end(ag, ready.ID, keepUntil(ready), path); endErr != nil {
		return fmt.Errorf("%w\n%v", err, endErr)
	}
	return err
}

// dial opens another SSH connection to the host.
func (h *host) dial(ctx context.Context) (*sshmux.Master, error) {
	return dial(ctx, h.o)
}

// dial opens an SSH connection to o.Dest.
func dial(ctx context.Context, o Options) (*sshmux.Master, error) {
	return sshmux.Dial(ctx, o.Dest, sshmux.Options{SSHPath: o.SSHPath, Args: o.SSHArgs, Stderr: o.Stderr, NoMux: o.NoMux})
}

// reconnect redials the host with backoff and resumes session id, until it
// works, ctx ends or the keep window closes at until. The tunnel is
// forwarded again over the new connection.
func (h *host) reconnect(ctx context.Context, path, id string, until time.Time, t *tunnel) (*agentProc, *protocol.Ready, error) {
	backoff := reconnectFirst
	for attempt := 1; ; attempt++ {
		h.close()
		ag, ready, err := h.redial(ctx, path, id, until, t)
		if err == nil {
			return ag, ready, nil
		}
		if ctx.Err() != nil {
			// Nothing is connected to send bye to.
			return nil, nil, h.end(nil, id, until, path)
		}
		var final *finalError
		if errors.As(err, &final) {
			return nil, nil, err
		}
		h.log.Warn("reconnect failed", "attempt", attempt, "err", err)
		if time.Now().Add(backoff).After(until) {
			return nil, nil, fmt.Errorf("could not reconnect to %s before the host let the desktop go: %w", h.o.Dest, err)
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
		}
		backoff = min(backoff*2, reconnectMax)
	}
}

// redial makes one attempt at reconnect's job.
func (h *host) redial(ctx context.Context, path, id string, until time.Time, t *tunnel) (*agentProc, *protocol.Ready, error) {
	dialCtx, cancel := context.WithDeadline(ctx, until)
	defer cancel()
	m, err := h.dial(dialCtx)
	if err != nil {
		return nil, nil, err
	}
	h.m = m
	ag, err := startAgent(ctx, m, path, serveArgs{verbose: h.o.Verbose, resume: id}, h.o.Stderr)
	if err != nil {
		return nil, nil, err
	}
	ready, err := ag.resume(dialCtx, id)
	if err != nil {
		ag.stop()
		return nil, nil, err
	}
	if err := t.forward(ctx, m, ready.TunnelAddr); err != nil {
		h.log.Warn("SSH tunnel unavailable after reconnect; direct UDP only", "err", err)
	}
	return ag, ready, nil
}

// finalError is a reconnect failure no retry can mend: the host says the
// session is gone, or speaks another protocol.
type finalError struct{ err error }

func (e *finalError) Error() string { return e.err.Error() }
func (e *finalError) Unwrap() error { return e.err }

// resumable says whether a lost connection is worth redialing: SSH or the
// agent's proxy went away without a word, as a dropped network does.
func resumable(err error) bool {
	return errors.Is(err, errSSHLost) || errors.Is(err, errAgentGone)
}

// pickSession decides which kept session, if any, robin gui resumes.
func pickSession(o Options, r *probe.Report, agentName string) (string, error) {
	if o.Attach != "" && o.Resume == "" {
		// An attached desktop is never kept; nothing to resume.
		return "", nil
	}
	var kept []protocol.Session
	for _, s := range r.Kept {
		if s.Mode == protocol.ModeSpawn || s.ID == o.Resume {
			kept = append(kept, s)
		}
	}
	var pick *protocol.Session
	switch {
	case o.Resume != "":
		for i := range kept {
			if kept[i].ID == o.Resume {
				pick = &kept[i]
			}
		}
		if pick == nil {
			return "", fmt.Errorf("no kept session %s on %s%s", o.Resume, o.Dest, keptList(r.Kept))
		}
	case o.New || len(kept) == 0:
		return "", nil
	case len(kept) == 1:
		pick = &kept[0]
	default:
		return "", fmt.Errorf("%s keeps %d sessions; pick one with --id, or start another with --new%s", o.Dest, len(kept), keptList(kept))
	}
	if pick.Agent != agentName {
		return "", fmt.Errorf("session %s on %s runs agent %s (build %s), this robin carries %s: run robin end %s %s to end it, or robin gui --new %s to start another beside it",
			pick.ID, o.Dest, pick.Agent, pick.Build, agentName, o.Dest, pick.ID, o.Dest)
	}
	return pick.ID, nil
}

// keptList describes kept sessions, one per line, for messages.
func keptList(kept []protocol.Session) string {
	var b strings.Builder
	for _, s := range kept {
		b.WriteString("\n  " + describeSession(s))
	}
	return b.String()
}

// describeSession is one line of robin ls.
func describeSession(s protocol.Session) string {
	state := "connected"
	if !s.Connected {
		state = "kept"
		if s.Until != nil {
			state += " until " + s.Until.Local().Format(time.Kitchen)
		}
	}
	desktop := s.Display
	if s.Desktop != "" {
		desktop += " (" + s.Desktop + ")"
	}
	return fmt.Sprintf("%s  %s  %s  started %s  %s", s.ID, s.Mode, desktop, s.Started.Local().Format("Jan 2 15:04"), state)
}

// detachMode picks how the daemon leaves the SSH session on this host:
// under the user's systemd where logind would otherwise kill it on logout.
func detachMode(r *probe.Report) protocol.Detach {
	if r.KillUserProcesses == "yes" && r.Commands["systemd-run"] != "" {
		return protocol.DetachSystemdRun
	}
	return protocol.DetachSetsid
}

// keepNote warns where a kept desktop would not survive the user logging
// out, and says what would make it.
func keepNote(r *probe.Report) string {
	if r.KillUserProcesses != "yes" {
		return ""
	}
	if r.Commands["systemd-run"] == "" {
		return "logind kills user processes on logout here and systemd-run is missing, so the desktop ends with this connection after all"
	}
	if !r.Linger {
		return "logind kills user processes on logout here; the desktop runs under your systemd user manager, which stops with your last login unless you run loginctl enable-linger"
	}
	return ""
}

// sessionNote is the line that says what the user got.
func sessionNote(dest string, r *protocol.Ready) string {
	desktop := r.Display
	if r.Mode == protocol.ModeAttach {
		desktop = "attached to " + r.Display
	}
	note := fmt.Sprintf("%s on %s%s", desktop, dest, monitorNote(r))
	if r.Resumed {
		note = fmt.Sprintf("resumed session %s, %s", r.ID, note)
	}
	if r.Keep > 0 {
		note += fmt.Sprintf("; kept for %s if the connection drops", keepDuration(r.Keep))
	}
	return note
}

func keepDuration(secs int) string {
	return (time.Duration(secs) * time.Second).String()
}

func runProbe(ctx context.Context, m *sshmux.Master) (*probe.Report, error) {
	names := agentbin.Names()
	out, err := m.Output(ctx, sshmux.Quote(append([]string{"sh", "-s", "--"}, names...)...), strings.NewReader(probe.Script))
	if err != nil {
		return nil, fmt.Errorf("probe host: %w", err)
	}
	r, err := probe.Parse(out)
	if err != nil {
		return nil, fmt.Errorf("probe host: %w", err)
	}
	if r.OS != "Linux" {
		return nil, fmt.Errorf("host runs %s; robin gui needs Linux", r.OS)
	}
	return r, nil
}

// startMessage fills in the session parameters, with lower defaults on
// small hosts, and picks the encoder when o leaves it to auto. The string
// says why it picked the one it did; it is empty when the user chose.
func startMessage(o Options, r *probe.Report) (protocol.Start, string) {
	s := protocol.Start{
		Version: protocol.Version, MaxWidth: o.MaxWidth, MaxHeight: o.MaxHeight,
		FPS: o.FPS, MaxKbps: o.MaxKbps, Session: o.Session, STUN: o.STUN,
		Attach: o.Attach, Monitor: o.Monitor, Audio: o.Audio, Encoder: o.Encoder,
	}
	small := r.CPUs > 0 && r.CPUs <= protocol.SmallHostCPUs
	if s.MaxWidth <= 0 || s.MaxHeight <= 0 {
		s.MaxWidth, s.MaxHeight = protocol.DefaultMaxWidth, protocol.DefaultMaxHeight
		if small {
			s.MaxWidth, s.MaxHeight = protocol.SmallHostMaxWidth, protocol.SmallHostMaxHeight
		}
	}
	if s.FPS <= 0 {
		s.FPS = protocol.DefaultFPS
		if small {
			s.FPS = protocol.SmallHostFPS
		}
	}
	if s.MaxKbps <= 0 {
		s.MaxKbps = protocol.DefaultMaxKbps
	}
	var why string
	if s.Encoder == protocol.EncoderAuto {
		s.Encoder, why = r.AutoEncoder(s.MaxWidth, s.MaxHeight)
		s.EncoderFallback = s.Encoder != protocol.EncoderSoftware
	}
	return s, why
}

func iceServers(stun []string) []protocol.ICEServer {
	if len(stun) == 0 {
		return []protocol.ICEServer{}
	}
	return []protocol.ICEServer{{URLs: stun}}
}

// agentProc is the running agent: its exec channel and the stderr tail kept
// for error messages.
type agentProc struct {
	conn  *protocol.Conn
	stdin io.WriteCloser
	// exited closes when the proxy's ssh process is gone.
	exited <-chan struct{}
	log    *tail.Buffer // the end of the agent's stderr
	once   sync.Once
}

// serveArgs are the flags of robin-agent serve.
type serveArgs struct {
	verbose bool
	keep    time.Duration
	resume  string
	detach  protocol.Detach
}

func (a serveArgs) list(path string) []string {
	args := []string{path, "serve"}
	if a.verbose {
		args = append(args, "-v")
	}
	if a.resume != "" {
		return append(args, "-resume", a.resume)
	}
	if a.keep <= 0 {
		// A session that ends with its controller needs no way out of the
		// login session, and systemd-run would cost it the login's
		// environment, which attaching to a desktop lives on.
		return args
	}
	args = append(args, "-keep", a.keep.String())
	if a.detach != "" && a.detach != protocol.DetachSetsid {
		args = append(args, "-detach", string(a.detach))
	}
	return args
}

func startAgent(ctx context.Context, m *sshmux.Master, path string, args serveArgs, stderr io.Writer) (*agentProc, error) {
	cmd := m.Command(ctx, sshmux.Quote(args.list(path)...))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	logTail := &tail.Buffer{Max: 8 << 10}
	if args.verbose {
		cmd.Stderr = io.MultiWriter(logTail, prefixWriter(stderr, "agent: "))
	} else {
		cmd.Stderr = logTail
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start agent: %w", err)
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()
	return &agentProc{
		conn:  protocol.NewConn(stdout, stdin, protocol.DefaultPingInterval, protocol.DefaultIdleTimeout),
		stdin: stdin, exited: exited, log: logTail,
	}, nil
}

// awaitReady waits for the agent's Ready, telling the user about notices
// that come first. A notice that asks the user to act extends the wait by
// as long as the agent will wait for them, within maxReadyWait.
func (a *agentProc) awaitReady(ctx context.Context, host string, stderr io.Writer) (*protocol.Ready, error) {
	start := time.Now()
	wait := readyTimeout
	for {
		waitCtx, cancel := context.WithTimeoutCause(ctx, wait, errors.New("agent did not become ready"))
		m, err := a.conn.Recv(waitCtx)
		cancel()
		if err == nil && m.Type == protocol.TypeNotice && m.Notice != nil {
			if text := noticeText(*m.Notice, host); text != "" {
				fmt.Fprintln(stderr, text)
			}
			wait = readyWait(start, time.Now(), m.Notice.Wait)
			continue
		}
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case err != nil:
			return nil, a.explain(err)
		case m.Type == protocol.TypeError:
			return nil, a.explain(&agentError{m.Error, m.Code})
		case m.Type != protocol.TypeReady || m.Ready == nil:
			return nil, a.explain(fmt.Errorf("agent sent %q before ready", m.Type))
		case m.Ready.Version != protocol.Version:
			return nil, &finalError{fmt.Errorf("agent speaks protocol %d, this CLI speaks %d", m.Ready.Version, protocol.Version)}
		}
		return m.Ready, nil
	}
}

// agentError is an error message the agent sent, with its code when it
// carried one.
type agentError struct {
	text string
	code protocol.Code
}

func (e *agentError) Error() string { return "agent: " + e.text }

// resume asks the agent for kept session id and waits for its Ready. An
// agent that has no such session, or does not speak this protocol, makes
// the error final.
func (a *agentProc) resume(ctx context.Context, id string) (*protocol.Ready, error) {
	if err := a.conn.Send(protocol.Message{Type: protocol.TypeResume, Resume: &protocol.Resume{Version: protocol.Version, ID: id}}); err != nil {
		return nil, a.explain(err)
	}
	ready, err := a.awaitReady(ctx, "", io.Discard)
	var agentErr *agentError
	if errors.As(err, &agentErr) && agentErr.code != protocol.CodeBusy {
		return nil, &finalError{err}
	}
	if err != nil {
		return nil, err
	}
	if ready.ID != id || !ready.Resumed {
		return nil, &finalError{fmt.Errorf("agent resumed session %q, not %q", ready.ID, id)}
	}
	return ready, nil
}

// maxConsentWait caps how far one notice may stretch the wait for Ready, and
// maxReadyWait the whole wait, however many notices come. The agent gives
// the user two minutes, at most twice in one start.
const (
	maxConsentWait = 5 * time.Minute
	maxReadyWait   = readyTimeout + 2*maxConsentWait
)

// readyWait is how long to wait for the agent's next message after a notice,
// received at now by an awaitReady that began at start, saying the agent
// waits secs seconds for the user.
func readyWait(start, now time.Time, secs int) time.Duration {
	secs = min(max(secs, 0), int(maxConsentWait/time.Second))
	return min(time.Duration(secs)*time.Second+readyTimeout, start.Add(maxReadyWait).Sub(now))
}

// bye ends the session over the live connection and reports whether the
// agent confirmed before the connection went: a bye that reached the host
// needs no second trip. The wait ends with the proxy process, since a
// write into a dying ssh's pipe can succeed and never arrive.
func (a *agentProc) bye() bool {
	if a == nil {
		return false
	}
	if err := a.conn.Send(protocol.Message{Type: protocol.TypeBye}); err != nil {
		a.stop()
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		select {
		case <-a.exited:
			cancel()
		case <-ctx.Done():
		}
	}()
	ended := false
	for !ended {
		m, err := a.conn.Recv(ctx)
		if err != nil {
			break
		}
		ended = m.Type == protocol.TypeEnded
	}
	a.stop()
	return ended
}

// stop closes the agent's stdin, which tells the host this controller is
// gone, and waits briefly for the proxy to exit.
func (a *agentProc) stop() {
	if a == nil {
		return
	}
	a.once.Do(func() {
		a.stdin.Close()
		select {
		case <-a.exited:
		case <-time.After(5 * time.Second):
		}
		a.conn.Close()
	})
}

// explain attaches the tail of the agent's stderr to an error, since that is
// usually where the real reason is.
func (a *agentProc) explain(err error) error {
	if err == nil {
		return nil
	}
	if t := strings.TrimSpace(a.log.String()); t != "" {
		return fmt.Errorf("%w\nagent log:\n%s", err, t)
	}
	return err
}

// errNoSession is endSession's answer when the host has no such session
// any more.
var errNoSession = errors.New("no such session on the host")

// endSession runs the agent's end command for session id over m.
func endSession(ctx context.Context, m *sshmux.Master, path, id string) error {
	_, err := m.Output(ctx, sshmux.Quote(path, "end", id), nil)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == protocol.ExitNoSession {
		return fmt.Errorf("%w: %v", errNoSession, err)
	}
	return err
}

// tunnel is the client end of the ICE-TCP fallback: a loopback TCP listener
// whose connections are spliced into an SSH forward to the agent's listener.
// The listener lives as long as the session; the forward is remade on each
// SSH connection, so the port the browser was told never changes.
type tunnel struct {
	ln  net.Listener
	log *slog.Logger

	mu  sync.Mutex
	fwd *sshmux.Forward // nil while there is none
}

// openTunnel sets the tunnel up for transport: none for Direct, and for
// the others the listener plus the forward over m, where a forward that
// fails is an error only when the tunnel is all the user allowed.
func openTunnel(ctx context.Context, m *sshmux.Master, remote string, transport relay.Transport, log *slog.Logger) (*tunnel, error) {
	t := &tunnel{log: log}
	if remote == "" || transport == relay.Direct {
		return t, nil
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	t.ln = ln
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go t.splice(c)
		}
	}()
	if err := t.forward(ctx, m, remote); err != nil {
		if transport == relay.Tunnel {
			ln.Close()
			return nil, err
		}
		log.Warn("SSH tunnel unavailable; direct UDP only", "err", err)
	}
	return t, nil
}

// forward points the tunnel at remote through m.
func (t *tunnel) forward(ctx context.Context, m *sshmux.Master, remote string) error {
	if t.ln == nil {
		return nil
	}
	fwd, err := m.Forward(ctx, remote)
	if err != nil {
		return fmt.Errorf("forward ICE-TCP over SSH: %w", err)
	}
	t.mu.Lock()
	t.fwd = fwd
	t.mu.Unlock()
	return nil
}

// Port is the loopback port the browser's tunnel candidate names, or 0
// without a tunnel.
func (t *tunnel) Port() int {
	if t.ln == nil {
		return 0
	}
	return t.ln.Addr().(*net.TCPAddr).Port
}

func (t *tunnel) Close() error {
	if t.ln == nil {
		return nil
	}
	return t.ln.Close()
}

func (t *tunnel) splice(c net.Conn) {
	defer c.Close()
	t.mu.Lock()
	fwd := t.fwd
	t.mu.Unlock()
	if fwd == nil {
		return
	}
	u, err := fwd.Dial()
	if err != nil {
		t.log.Warn("tunnel dial", "err", err)
		return
	}
	defer u.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(u, c); done <- struct{}{} }()
	go func() { io.Copy(c, u); done <- struct{}{} }()
	<-done
}
