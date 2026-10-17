// Package gui runs `remotejump gui`: it connects over SSH, probes the host,
// deploys and starts the agent, forwards the ICE-TCP tunnel, serves the
// browser page and relays signaling between the page and the agent.
package gui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/yasinmiran/remotejump/internal/agentbin"
	"github.com/yasinmiran/remotejump/internal/deploy"
	"github.com/yasinmiran/remotejump/internal/probe"
	"github.com/yasinmiran/remotejump/internal/protocol"
	"github.com/yasinmiran/remotejump/internal/relay"
	"github.com/yasinmiran/remotejump/internal/sshmux"
	"github.com/yasinmiran/remotejump/internal/tail"
	"github.com/yasinmiran/remotejump/internal/web"
)

// Options are the user's choices for one session. Zero values pick defaults.
type Options struct {
	Dest    string
	SSHPath string
	SSHArgs []string

	Transport           relay.Transport
	MaxWidth, MaxHeight int
	FPS                 int
	MaxKbps             int
	Session             string
	// Attach streams a running desktop instead of spawning one: an X11
	// display, "auto", or protocol.AttachWayland.
	Attach  string
	Audio   bool
	Encoder string
	// HiDPI sizes the remote display in the browser's device pixels.
	HiDPI bool
	STUN  []string

	Listen    string // page address, default 127.0.0.1:0
	NoBrowser bool   // print the URL on stdout instead of opening a browser
	Verbose   bool

	Stdout, Stderr io.Writer
	Log            *slog.Logger
}

const readyTimeout = 60 * time.Second

// Run holds one session until ctx ends, the agent exits or SSH drops.
func Run(ctx context.Context, o Options) error {
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}

	log.Info("connecting", "dest", o.Dest)
	m, err := sshmux.Dial(ctx, o.Dest, sshmux.Options{SSHPath: o.SSHPath, Args: o.SSHArgs, Stderr: o.Stderr})
	if err != nil {
		return err
	}
	defer m.Close()

	report, err := runProbe(ctx, m)
	if err != nil {
		return err
	}
	log.Info("probed host", "distro", report.Distro.ID+" "+report.Distro.VersionID, "arch", report.Arch, "cpus", report.CPUs, "agentdir", report.AgentDir)
	if report.AgentDir == "" {
		var why []string
		for _, r := range report.AgentDirRejected {
			why = append(why, r.Path+": "+r.Reason)
		}
		return fmt.Errorf("no usable directory for the agent on %s (%s)", o.Dest, strings.Join(why, "; "))
	}
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
	a, err := agentbin.For(report.Arch)
	if err != nil {
		return err
	}
	path, err := deploy.Ensure(ctx, m, deploy.Target{
		Dir: report.AgentDir, PresentHash: report.AgentHash(a.Name()), SHA256Tool: report.SHA256Tool,
	}, a)
	if err != nil {
		return fmt.Errorf("deploy agent: %w", err)
	}
	log.Info("agent in place", "path", path)

	ag, err := startAgent(ctx, m, path, o.Verbose, o.Stderr)
	if err != nil {
		return err
	}
	defer ag.stop()

	start := startMessage(o, report)
	if err := ag.conn.Send(protocol.Message{Type: protocol.TypeStart, Start: &start}); err != nil {
		return ag.explain(err)
	}
	ready, err := ag.awaitReady(ctx)
	if err != nil {
		return err
	}
	log.Info("agent ready", "mode", ready.Mode, "display", ready.Display, "session", ready.Session,
		"size", fmt.Sprintf("%dx%d", ready.Width, ready.Height), "encoder", ready.Encoder, "audio", ready.Audio)

	tunnelPort := 0
	if ready.TunnelAddr != "" && o.Transport != relay.Direct {
		t, err := openTunnel(ctx, m, ready.TunnelAddr, log)
		if err != nil {
			if o.Transport == relay.Tunnel {
				return err
			}
			log.Warn("SSH tunnel unavailable; direct UDP only", "err", err)
		} else {
			defer t.Close()
			tunnelPort = t.Port()
		}
	}

	srv, err := web.Listen(web.Options{Addr: o.Listen})
	if err != nil {
		return err
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
	fmt.Fprintf(o.Stderr, "remotejump: %s on %s; press Ctrl+C to end the session\n", ready.Display, o.Dest)

	b := &bridge{
		agent: ag.conn, relay: relay.New(relay.Policy{Transport: o.Transport, TunnelPort: tunnelPort}),
		config: protocol.Config{ICEServers: iceServers(start.STUN), HiDPI: o.HiDPI}, log: log,
	}
	err = b.run(ctx, srv, m.Done(), serveErr)
	// Ctrl+C in a terminal reaches ssh too, so the master may die first; a
	// signal is a clean exit either way. The agent sees its stdin close and
	// tears the display down by itself.
	if ctx.Err() != nil {
		err = nil
	}
	ag.conn.Send(protocol.Message{Type: protocol.TypeBye})
	return ag.explain(err)
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
		return nil, fmt.Errorf("host runs %s; remotejump gui needs Linux", r.OS)
	}
	return r, nil
}

// startMessage fills in the session parameters, capping size and frame rate
// on small hosts: software H.264 at 1080p30 costs one to two cores.
func startMessage(o Options, r *probe.Report) protocol.Start {
	s := protocol.Start{
		Version: protocol.Version, MaxWidth: o.MaxWidth, MaxHeight: o.MaxHeight,
		FPS: o.FPS, MaxKbps: o.MaxKbps, Session: o.Session, STUN: o.STUN,
		Attach: o.Attach, Audio: o.Audio, Encoder: o.Encoder,
	}
	small := r.CPUs > 0 && r.CPUs <= 2
	if s.MaxWidth <= 0 || s.MaxHeight <= 0 {
		s.MaxWidth, s.MaxHeight = 1920, 1080
		if small {
			s.MaxWidth, s.MaxHeight = 1280, 720
		}
	}
	if s.FPS <= 0 {
		s.FPS = 30
		if small {
			s.FPS = 20
		}
	}
	if s.MaxKbps <= 0 {
		s.MaxKbps = 10_000
	}
	return s
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
	wait  func() error
	log   *tail.Buffer // the end of the agent's stderr
	once  sync.Once
}

func startAgent(ctx context.Context, m *sshmux.Master, path string, verbose bool, stderr io.Writer) (*agentProc, error) {
	args := []string{path, "serve"}
	if verbose {
		args = append(args, "-v")
	}
	cmd := m.Command(ctx, sshmux.Quote(args...))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	logTail := &tail.Buffer{Max: 8 << 10}
	if verbose {
		cmd.Stderr = io.MultiWriter(logTail, prefixWriter(stderr, "agent: "))
	} else {
		cmd.Stderr = logTail
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start agent: %w", err)
	}
	return &agentProc{
		conn:  protocol.NewConn(stdout, stdin, protocol.DefaultPingInterval, protocol.DefaultIdleTimeout),
		stdin: stdin, wait: cmd.Wait, log: logTail,
	}, nil
}

func (a *agentProc) awaitReady(ctx context.Context) (*protocol.Ready, error) {
	type result struct {
		m   protocol.Message
		err error
	}
	got := make(chan result, 1)
	go func() {
		m, err := a.conn.Recv()
		got <- result{m, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(readyTimeout):
		return nil, a.explain(errors.New("agent did not become ready"))
	case r := <-got:
		switch {
		case r.err != nil:
			return nil, a.explain(r.err)
		case r.m.Type == protocol.TypeError:
			return nil, a.explain(fmt.Errorf("agent: %s", r.m.Error))
		case r.m.Type != protocol.TypeReady || r.m.Ready == nil:
			return nil, a.explain(fmt.Errorf("agent sent %q before ready", r.m.Type))
		case r.m.Ready.Version != protocol.Version:
			return nil, fmt.Errorf("agent speaks protocol %d, this CLI speaks %d", r.m.Ready.Version, protocol.Version)
		}
		return r.m.Ready, nil
	}
}

// stop closes the agent's stdin, which is its signal to tear the display
// down, and waits briefly for it to exit.
func (a *agentProc) stop() {
	a.once.Do(func() {
		a.stdin.Close()
		done := make(chan struct{})
		go func() { a.wait(); close(done) }()
		select {
		case <-done:
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

// tunnel is the client end of the ICE-TCP fallback: a loopback TCP listener
// whose connections are spliced into an SSH forward to the agent's listener.
type tunnel struct {
	ln net.Listener
}

func openTunnel(ctx context.Context, m *sshmux.Master, remote string, log *slog.Logger) (*tunnel, error) {
	sock := m.SocketPath("ice.sock")
	if err := m.ForwardLocal(ctx, sock, remote); err != nil {
		return nil, fmt.Errorf("forward ICE-TCP over SSH: %w", err)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	t := &tunnel{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go splice(c, sock, log)
		}
	}()
	return t, nil
}

func (t *tunnel) Port() int { return t.ln.Addr().(*net.TCPAddr).Port }

func (t *tunnel) Close() error { return t.ln.Close() }

func splice(c net.Conn, sock string, log *slog.Logger) {
	defer c.Close()
	u, err := net.Dial("unix", sock)
	if err != nil {
		log.Warn("tunnel dial", "err", err)
		return
	}
	defer u.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(u, c); done <- struct{}{} }()
	go func() { io.Copy(c, u); done <- struct{}{} }()
	<-done
}
