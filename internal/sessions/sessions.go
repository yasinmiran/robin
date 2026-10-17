// Package sessions names what spawn mode can run inside the display it
// starts. The probe checks a host against these lists and the agent picks
// from them, so both sides agree on what counts as usable.
package sessions

// Desktops are complete sessions; they need nothing started beside them.
var Desktops = []string{"xfce4-session"}

// WindowManagers manage windows but show nothing on their own, so the agent
// opens a terminal beside them.
var WindowManagers = []string{"openbox-session", "openbox", "fluxbox", "icewm-session", "twm"}

// Terminals are the terminal emulators the agent can open, in order of
// preference.
var Terminals = []string{"xterm", "x-terminal-emulator", "xfce4-terminal"}
