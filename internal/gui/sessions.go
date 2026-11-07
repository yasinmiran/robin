package gui

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/yasinmiran/robin/internal/agentbin"
	"github.com/yasinmiran/robin/internal/protocol"
)

// List runs `robin ls`: it prints the sessions the host keeps, one per
// line, to o.Stdout.
func List(ctx context.Context, o Options) (err error) {
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
	if len(h.report.Kept) == 0 {
		fmt.Fprintf(o.Stderr, "robin: no kept sessions on %s\n", o.Dest)
		return nil
	}
	for _, s := range h.report.Kept {
		fmt.Fprintln(o.Stdout, printable(describeSession(s)))
	}
	return nil
}

// End runs `robin end`: it ends kept session id on the host, or the one
// kept session when id is empty.
func End(ctx context.Context, o Options, id string) (err error) {
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
	s, err := chooseToEnd(o.Dest, h.report.Kept, id)
	if err != nil {
		return err
	}
	if err := h.endKept(ctx, s); err != nil {
		return fmt.Errorf("end session %s on %s: %w", s.ID, o.Dest, err)
	}
	fmt.Fprintf(o.Stderr, "robin: ended session %s (%s) on %s\n", s.ID, s.Display, o.Dest)
	return nil
}

// endKept ends kept session s with the agent it runs from, which deploy
// leaves in place beside this CLI's, so ending a session of another build
// uploads nothing. Only when that agent cannot end it (its file is gone,
// say) does this CLI's agent go in place to do it.
func (h *host) endKept(ctx context.Context, s protocol.Session) error {
	if own, ok := h.keptAgentPath(s); ok {
		err := endSession(ctx, h.m, own, s.ID)
		if err == nil || errors.Is(err, errNoSession) {
			return err
		}
		h.log.Info("the session's own agent did not end it", "agent", own, "err", err)
	}
	a, err := agentbin.For(h.report.Arch)
	if err != nil {
		return err
	}
	path, err := h.deployAgent(ctx, a)
	if err != nil {
		return err
	}
	return endSession(ctx, h.m, path, s.ID)
}

// keptAgentPath is where the agent of kept session s lies on the host. The
// name comes from the session's own file, so only a bare agent file name
// in the agent directory counts.
func (h *host) keptAgentPath(s protocol.Session) (string, bool) {
	name, ok := strings.CutPrefix(s.Agent, agentbin.NamePrefix)
	if !ok || name == "" || strings.ContainsAny(name, "/\x00") || !path.IsAbs(h.report.AgentDir) {
		return "", false
	}
	return path.Join(h.report.AgentDir, s.Agent), true
}

// chooseToEnd picks the session robin end means.
func chooseToEnd(dest string, kept []protocol.Session, id string) (protocol.Session, error) {
	if id != "" {
		for _, s := range kept {
			if s.ID == id {
				return s, nil
			}
		}
		return protocol.Session{}, fmt.Errorf("no kept session %s on %s%s", id, dest, keptList(kept))
	}
	switch len(kept) {
	case 0:
		return protocol.Session{}, fmt.Errorf("no kept sessions on %s", dest)
	case 1:
		return kept[0], nil
	}
	return protocol.Session{}, fmt.Errorf("%s keeps %d sessions; say which: robin end %s <id>%s", dest, len(kept), dest, keptList(kept))
}
