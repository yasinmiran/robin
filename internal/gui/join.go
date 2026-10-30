package gui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/yasinmiran/robin/internal/agentbin"
	"github.com/yasinmiran/robin/internal/protocol"
	"github.com/yasinmiran/robin/internal/relay"
	"github.com/yasinmiran/robin/internal/sshmux"
)

// inviteWait bounds the wait for the agent's answer to an invite.
const inviteWait = 10 * time.Second

// checkJoin refuses what a guest cannot ask of a session it joins: the
// desktop is the owner's, and so are its shape and its camera.
func checkJoin(o Options) error {
	if o.Join == "" {
		return nil
	}
	if _, err := protocol.ParseJoinCode(o.Join); err != nil {
		return err
	}
	switch {
	case len(o.Share) > 0:
		return errors.New("--share is the owner's; a guest cannot invite others")
	case o.Attach != "" || o.Resume != "" || o.New || o.Session != "":
		return errors.New("--join joins the owner's desktop as it is; --attach, --id, --new and --session are the owner's")
	case o.Camera:
		return errors.New("a guest's camera is not offered: the camera stays the owner's")
	case o.Curtain || o.Silent:
		return errors.New("--curtain and --silent are the owner's")
	}
	return nil
}

// invite asks the agent for an invitation for each of users and tells the
// owner how each joins. A user the host has not, or one the session
// cannot take, is reported and the session goes on.
func invite(ctx context.Context, ag *agentProc, users []string, dest string, stderr io.Writer) error {
	for _, user := range users {
		if err := ag.conn.Send(protocol.Message{Type: protocol.TypeInvite, Invite: &protocol.Invite{User: user}}); err != nil {
			return ag.explain(err)
		}
		inv, err := ag.awaitInvited(ctx)
		if err != nil {
			return err
		}
		if inv.Error != "" {
			fmt.Fprintf(stderr, "robin: cannot share with %s: %s\n", user, inv.Error)
			continue
		}
		fmt.Fprintln(stderr, inviteNote(*inv, dest))
	}
	return nil
}

// inviteNote is the line that tells the owner how inv's user joins.
func inviteNote(inv protocol.Invitation, dest string) string {
	return fmt.Sprintf("robin: %s may join once, until %s, with: robin gui --join %s %s@%s",
		inv.User, inv.Expires.Local().Format(time.Kitchen), inv.Code, inv.User, hostOf(dest))
}

// awaitInvited waits for the agent's answer to an invite.
func (a *agentProc) awaitInvited(ctx context.Context) (*protocol.Invitation, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, inviteWait, errors.New("the agent did not answer the invite"))
	defer cancel()
	for {
		m, err := a.conn.Recv(ctx)
		switch {
		case err != nil:
			return nil, a.explain(err)
		case m.Type == protocol.TypeInvited && m.Invitation != nil:
			return m.Invitation, nil
		case m.Type == protocol.TypeError:
			return nil, a.explain(&agentError{m.Error, m.Code})
		}
	}
}

// joinArgs is robin-agent join's command line for the session code names.
func joinArgs(path string, code protocol.JoinCode) []string {
	return []string{path, "join", "-session", code.Session, "-owner", strconv.Itoa(code.Owner)}
}

// runGuest joins the session o.Join names as a guest: over the user's own
// SSH login, as the user it logs in as, with its own agent, page and
// tunnel. It ends when the user leaves (Ctrl+C or the page), or the owner
// removes the guest or ends the session; a lost connection is not
// redialed, since the code it joined with is spent.
func runGuest(ctx context.Context, o Options, log *slog.Logger) error {
	code, err := protocol.ParseJoinCode(o.Join)
	if err != nil {
		return err
	}
	h, err := connect(ctx, o, log)
	if err != nil {
		return err
	}
	defer h.close()
	a, err := agentbin.For(h.report.Arch)
	if err != nil {
		return err
	}
	path, err := h.deployAgent(ctx, a)
	if err != nil {
		return err
	}
	ag, err := startProxy(h.m, joinArgs(path, code), o.Verbose, o.Stderr)
	if err != nil {
		return err
	}
	defer ag.stop()
	return h.guest(ctx, ag)
}

// guest is runGuest once the agent's proxy runs: it presents the code,
// and serves the page until the guest's session ends.
func (h *host) guest(ctx context.Context, ag *agentProc) error {
	o, log := h.o, h.log
	if err := ag.conn.Send(protocol.Message{Type: protocol.TypeJoin, Join: &protocol.Join{Version: protocol.Version, Code: o.Join}}); err != nil {
		return ag.explain(err)
	}
	ready, err := ag.awaitReady(ctx, "", io.Discard)
	if err != nil {
		return err
	}
	if ready.Share == nil {
		return fmt.Errorf("the agent on %s answered the join as no guest's", o.Dest)
	}
	log.Info("joined", "owner", ready.Share.Owner, "display", ready.Display, "size", fmt.Sprintf("%dx%d", ready.Width, ready.Height))

	t, err := openTunnel(ctx, h.m, ready.TunnelAddr, o.Transport, log)
	if err != nil {
		ag.bye()
		return err
	}
	defer t.Close()
	srv, err := listenPage(o)
	if err != nil {
		ag.bye()
		return err
	}
	serveCtx, stopServe := context.WithCancel(ctx)
	defer stopServe()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(serveCtx) }()
	showPage(o, srv)
	fmt.Fprintln(o.Stderr, guestNote(o, ready))

	stun := o.STUN
	if o.Transport == relay.Tunnel {
		stun = nil
	}
	config := protocol.Config{ICEServers: iceServers(stun), HiDPI: o.HiDPI, Share: ready.Share}
	config.Mic = o.Mic && ready.Mic != nil && ready.Mic.Available
	b := newBridge(ag.conn, relay.New(relay.Policy{Transport: o.Transport, TunnelPort: t.Port()}), config, nil, log)
	b.answered = t.answered
	go b.accept(ctx, srv)
	defer b.close()

	err = b.run(ctx, ag.conn, h.m.Done(), serveErr)
	switch {
	case ctx.Err() != nil, errors.Is(err, errPageEnded), errors.Is(err, errDetached):
		ag.bye()
		b.ended(whyGuestLeft)
		return nil
	case errors.Is(err, errEnded):
		b.ended(strings.TrimPrefix(err.Error(), errEnded.Error()+": "))
		fmt.Fprintf(o.Stderr, "robin: %v\n", err)
		return nil
	}
	return ag.explain(err)
}

// whyGuestLeft is what a guest's page says once the guest left.
const whyGuestLeft = "you left the session"

// guestNote is the line that tells a guest what they joined.
func guestNote(o Options, r *protocol.Ready) string {
	note := fmt.Sprintf("robin: joined %s's desktop %s on %s; view only until %s gives you control; press Ctrl+C to leave",
		r.Share.Owner, r.Display, o.Dest, r.Share.Owner)
	if o.Mic && (r.Mic == nil || !r.Mic.Available) {
		note += fmt.Sprintf("\nrobin: %s's session takes no microphone", r.Share.Owner)
	}
	return note
}

// startProxy runs the agent command argv over m as a controller's proxy.
func startProxy(m *sshmux.Master, argv []string, verbose bool, stderr io.Writer) (*agentProc, error) {
	return startCommand(m, sshmux.Quote(argv...), verbose, stderr)
}
