package gui

import (
	"context"
	"fmt"

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
	a, err := agentbin.For(h.report.Arch)
	if err != nil {
		return err
	}
	path, err := h.deployAgent(ctx, a)
	if err != nil {
		return err
	}
	if err := endSession(ctx, h.m, path, s.ID); err != nil {
		return fmt.Errorf("end session %s on %s: %w", s.ID, o.Dest, err)
	}
	fmt.Fprintf(o.Stderr, "robin: ended session %s (%s) on %s\n", s.ID, s.Display, o.Dest)
	return nil
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
