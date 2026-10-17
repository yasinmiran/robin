//go:build linux

package display

import (
	"errors"
	"strings"
)

// sessionPlan is what to run inside the display.
type sessionPlan struct {
	name      string
	main      []string // watched: its exit ends the display
	companion []string // started alongside, not watched
}

var (
	windowManagers = []string{"openbox-session", "openbox", "fluxbox", "icewm-session", "twm"}
	terminals      = []string{"xterm", "x-terminal-emulator", "xfce4-terminal"}
)

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
	if xfce, err := lookPath("xfce4-session"); err == nil {
		if dbus, err := lookPath("dbus-run-session"); err == nil {
			return sessionPlan{name: "xfce4-session", main: []string{dbus, "--", xfce}}, nil
		}
		return sessionPlan{name: "xfce4-session", main: []string{xfce}}, nil
	}
	term, termArgv := first(terminals)
	if wm, wmArgv := first(windowManagers); wm != "" {
		if term == "" {
			return sessionPlan{name: wm, main: wmArgv}, nil
		}
		return sessionPlan{name: wm + " + " + term, main: wmArgv, companion: termArgv}, nil
	}
	if term != "" {
		return sessionPlan{name: term, main: termArgv}, nil
	}
	return sessionPlan{}, errors.New("display: no X session found; install xfce4-session, a window manager (" +
		strings.Join(windowManagers, ", ") + ") or a terminal (" + strings.Join(terminals, ", ") + ")")
}
