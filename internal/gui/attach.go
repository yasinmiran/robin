package gui

import (
	"errors"
	"fmt"
	"strings"

	"github.com/yasinmiran/robin/internal/probe"
	"github.com/yasinmiran/robin/internal/protocol"
)

// pickAttach chooses the running desktop that --attach auto means: the
// user's Wayland session when it is the live one, else the X display of the
// user's active local X11 session, else the one X server socket the user
// owns. GDM registers X11 sessions before their server starts, so the
// session often names no display and the socket decides.
//
// A Wayland session runs Xwayland too, whose root window shows only its X11
// windows, so an active Wayland session the probe cannot reach gets no X11
// guess either.
func pickAttach(r *probe.Report) (string, error) {
	if waylandActive(r) {
		return protocol.AttachWayland, nil
	}
	wayland := false
	for _, s := range r.Sessions {
		if !s.Active || s.Remote {
			continue
		}
		switch s.Type {
		case "x11":
			if s.Display != "" {
				return s.Display, nil
			}
		case "wayland":
			wayland = true
		}
	}
	var mine []string
	for _, x := range r.X11 {
		if x.UID == r.UID {
			mine = append(mine, x.Display)
		}
	}
	if len(mine) == 1 && !wayland {
		return mine[0], nil
	}

	var why string
	switch {
	case wayland:
		why = "your active session is Wayland; use --attach wayland, or --attach :N for one X display"
	case len(mine) > 1:
		why = "you run more than one X server; choose one with --attach :N"
	default:
		why = "no X11 desktop of yours is running; name one with --attach :N"
	}
	return "", errors.New("--attach auto: " + why + "\n" + found(r))
}

// found describes the sessions and X displays the probe saw, for an error.
func found(r *probe.Report) string {
	var sessions, displays []string
	for _, s := range r.Sessions {
		desc := []string{s.Type}
		if s.Display != "" {
			desc = append(desc, "display "+s.Display)
		}
		if s.Active {
			desc = append(desc, "active")
		}
		if s.Remote {
			desc = append(desc, "remote")
		}
		sessions = append(sessions, fmt.Sprintf("%s (%s)", s.ID, strings.Join(desc, ", ")))
	}
	for _, x := range r.X11 {
		owner := x.Owner
		if owner == "" {
			owner = fmt.Sprintf("uid %d", x.UID)
		}
		displays = append(displays, fmt.Sprintf("%s (%s)", x.Display, owner))
	}
	none := func(list []string) string {
		if len(list) == 0 {
			return "none"
		}
		return strings.Join(list, ", ")
	}
	return fmt.Sprintf("found sessions: %s; X displays: %s", none(sessions), none(displays))
}

// monitorNote says which monitor of an attached desktop the page shows, for
// a desktop with more than one.
func monitorNote(r *protocol.Ready) string {
	if len(r.Monitors) < 2 {
		return ""
	}
	if r.Monitor == protocol.MonitorAll {
		return fmt.Sprintf(", showing all %d monitors as one picture (switch in the page)", len(r.Monitors))
	}
	for _, m := range r.Monitors {
		if m.Name == r.Monitor {
			return fmt.Sprintf(", showing monitor %s (%dx%d), one of %d (switch in the page)", m.Name, m.W, m.H, len(r.Monitors))
		}
	}
	return ""
}
