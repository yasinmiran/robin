package protocol

import "time"

// Signaling for sessions that outlive one CLI connection. The host keeps a
// session daemon running after its controller (the CLI, through the agent's
// proxy) goes away without Bye, for as long as Start asked with Keep, and a
// later controller resumes it.
const (
	// TypeResume is the first message of a controller that picks up a kept
	// session, in place of Start. The agent answers with Ready again.
	TypeResume Type = "resume"
	// TypeEnded goes from agent to CLI when the session ends on purpose:
	// Bye, the keep window running out, or robin end. Error says which. A
	// controller that sees it does not reconnect.
	TypeEnded Type = "ended"

	// TypeReconnecting goes from CLI to browser when the host connection
	// dropped and the CLI is redialing; the page drops its peer connection
	// and waits for the next Config to build another.
	TypeReconnecting Type = "reconnecting"
	// TypeDetach goes from browser to CLI: end this CLI but keep the desktop
	// on the host for another robin gui to pick up.
	TypeDetach Type = "detach"
)

// Code is what an Error message means, for the CLI to act on without
// reading the text.
type Code string

// CodeBusy: another controller is attached to the kept session. The CLI
// may try again; any other error on a resume is final.
const CodeBusy Code = "busy"

// ExitNoSession is the exit status of `robin-agent end` when the host has
// no such session, so a CLI whose bye got through sees the end as done.
const ExitNoSession = 3

// Resume names the kept session a controller wants back.
type Resume struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
}

// Detach is how a session daemon leaves the SSH session that started it,
// the agent's -detach flag.
type Detach string

const (
	// DetachSetsid starts it in a session of its own, which sshd's SIGHUP
	// and the exec channel's pipes never reach. It stays in logind's
	// session scope, so KillUserProcesses=yes would still kill it.
	DetachSetsid Detach = "setsid"
	// DetachSystemdRun starts it as a transient unit of the user's systemd
	// manager, outside the session scope. The unit lives while the user has
	// any session or lingering is enabled.
	DetachSystemdRun Detach = "systemd-run"
)

// DefaultKeep is how long a spawned desktop waits for its controller to
// come back, when the user names no --keep. Attached desktops are not kept:
// nothing of theirs would be lost.
const DefaultKeep = 8 * time.Hour

// Session is what a session daemon records about itself in session.json,
// on one line, inside its run directory. The probe reports each live one,
// so robin ls and robin gui can find it without talking to the daemon.
type Session struct {
	ID string `json:"id"`
	// PID is the daemon's process id; the probe skips a file whose PID is
	// gone.
	PID int `json:"pid"`
	// Agent is the file name of the agent binary the daemon runs from, and
	// Build its version. A CLI that carries a different agent refuses to
	// resume, since deploy may have replaced the binary beneath the daemon.
	Agent string `json:"agent"`
	Build string `json:"build"`
	// Mode, Display and Desktop describe the desktop as Ready reports them.
	Mode    string `json:"mode"`
	Display string `json:"display"`
	Desktop string `json:"desktop,omitempty"`
	// Keep is the linger window in seconds; 0 ends the session with its
	// controller.
	Keep int `json:"keep"`
	// Started is when the desktop came up, and Connected whether a
	// controller is attached now. Until is when the keep window runs out,
	// set only while no controller is attached.
	Started   time.Time  `json:"started"`
	Connected bool       `json:"connected"`
	Until     *time.Time `json:"until,omitempty"`
}
