//go:build linux

package agent

import (
	"fmt"
	"strings"

	"github.com/yasinmiran/robin/internal/protocol"
)

// takeResume does what a resuming CLI asks of the session beyond giving it
// back: it turns on the private session's switches the CLI names, as Start
// would have, and checks that the session has the devices the CLI offers,
// which it has only if it started with them. An error refuses the resume
// and leaves the session as it was.
func (s *session) takeResume(r protocol.Resume) error {
	var flags, devices []string
	if r.Mic && !s.start.Mic {
		flags, devices = append(flags, "--mic"), append(devices, "a microphone")
	}
	if r.Camera && !s.start.Camera {
		flags, devices = append(flags, "--camera"), append(devices, "a camera")
	}
	if len(flags) > 0 {
		return fmt.Errorf("session %s started without %s, and a kept session keeps the devices it started with: resume it without %s, or end it with robin end and start another",
			r.ID, strings.Join(devices, " or "), strings.Join(flags, " and "))
	}
	return s.private.require(r.Curtain, r.Silent)
}
