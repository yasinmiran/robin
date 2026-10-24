package gui

import (
	"errors"

	"github.com/yasinmiran/robin/internal/protocol"
)

// checkPrivate refuses a private session on a desktop the agent spawns,
// which nobody sits in front of.
func checkPrivate(o Options) error {
	if (o.Curtain || o.Silent) && o.Attach == "" {
		return errors.New("--curtain and --silent are for a desktop you attach to with --attach; nobody sits in front of one robin starts")
	}
	return nil
}

// privateNote is the CLI's hint, once attached, that the room at the host
// sees and hears the session, and how to stop it: empty when the session
// already does, or cannot.
func privateNote(r *protocol.Ready) string {
	p := r.Private
	if p == nil || p.Curtain.On || p.Silent.On {
		return ""
	}
	curtain, silent := p.Curtain.Unsupported == "", p.Silent.Unsupported == ""
	switch {
	case curtain && silent:
		return "robin: the host's screen and speakers show this session to whoever is there; --curtain blacks out the screen and --silent keeps the speakers quiet, or switch them in the page"
	case curtain:
		return "robin: the host's screen shows this session to whoever is there; --curtain blacks it out, or switch it in the page"
	case silent:
		return "robin: the host's speakers play this session to whoever is there; --silent keeps them quiet, or switch it in the page"
	}
	return ""
}
