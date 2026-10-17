package gui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yasinmiran/robin/internal/probe"
	"github.com/yasinmiran/robin/internal/protocol"
)

// checkWayland refuses --attach wayland on a host that cannot do it, saying
// what is missing and how to install it.
func checkWayland(dest string, r *probe.Report) error {
	missing := r.WaylandMissing()
	if len(missing) == 0 {
		return nil
	}
	msg := fmt.Sprintf("%s cannot share its Wayland desktop: missing %s", dest, strings.Join(missing, ", "))
	if hint := r.WaylandInstallHint(); hint != "" {
		msg += "\ninstall with: " + hint
	}
	return errors.New(msg)
}

// waylandActive reports whether the user's live desktop on the host is a
// Wayland session: logind says so, or, on a host without logind, a
// compositor socket and the session bus are there.
func waylandActive(r *probe.Report) bool {
	if len(r.Wayland) == 0 || r.SessionBus == "" {
		return false
	}
	if len(r.Sessions) == 0 {
		return true
	}
	for _, s := range r.Sessions {
		if s.Type == "wayland" && s.Active {
			return true
		}
	}
	return false
}

// noticeText says what a notice from the agent asks of the user, or "" for
// a kind this CLI does not know.
func noticeText(n protocol.Notice, host string) string {
	switch n.Kind {
	case protocol.NoticeConsent:
		return fmt.Sprintf("robin: approve the screen sharing request on %s's screen, with remote interaction "+
			"allowed (GNOME: switch on Allow Remote Interaction, then Share; KDE Plasma: Approve). Waiting up to %s. "+
			"Once approved, later sessions start without asking.", host, time.Duration(n.Wait)*time.Second)
	case protocol.NoticeViewOnly:
		return fmt.Sprintf("robin: view only. %s shared its screen without remote interaction, so your "+
			"keyboard and mouse do nothing there. To take control, end this session, run it again "+
			"and allow remote interaction in the dialog.", host)
	}
	return ""
}

// hostOf strips the user from a [user@]host destination.
func hostOf(dest string) string {
	return dest[strings.LastIndexByte(dest, '@')+1:]
}
