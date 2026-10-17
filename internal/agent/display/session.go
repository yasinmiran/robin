//go:build linux

package display

import (
	"errors"
	"strings"

	"github.com/yasinmiran/robin/internal/sessions"
)

// sessionPlan is what to run inside the display.
type sessionPlan struct {
	name      string
	main      []string // watched: its exit ends the display
	companion []string // started alongside, not watched
}

// pickSession resolves custom, or the lightest complete desktop on PATH: Xfce,
// else a window manager with a terminal beside it so the screen is never
// empty, else a bare terminal.
func pickSession(custom string, lookPath func(string) (string, error)) (sessionPlan, error) {
	if custom != "" {
		return sessionPlan{name: custom, main: []string{"/bin/sh", "-c", custom}}, nil
	}
	first := func(names []string) (string, []string) {
		for _, n := range names {
			if p, err := lookPath(n); err == nil {
				return n, []string{p}
			}
		}
		return "", nil
	}
	if desk, argv := first(sessions.Desktops); desk != "" {
		// A desktop expects a session bus of its own.
		if dbus, err := lookPath("dbus-run-session"); err == nil {
			argv = append([]string{dbus, "--"}, argv...)
		}
		return sessionPlan{name: desk, main: argv}, nil
	}
	term, termArgv := first(sessions.Terminals)
	if wm, wmArgv := first(sessions.WindowManagers); wm != "" {
		if term == "" {
			return sessionPlan{name: wm, main: wmArgv}, nil
		}
		return sessionPlan{name: wm + " + " + term, main: wmArgv, companion: termArgv}, nil
	}
	if term != "" {
		return sessionPlan{name: term, main: termArgv}, nil
	}
	return sessionPlan{}, errors.New("display: no X session found; install xfce4-session, a window manager (" +
		strings.Join(sessions.WindowManagers, ", ") + ") or a terminal (" + strings.Join(sessions.Terminals, ", ") + ")")
}
