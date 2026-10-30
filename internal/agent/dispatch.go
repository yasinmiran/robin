//go:build linux

package agent

import (
	"context"
	"fmt"

	"github.com/yasinmiran/robin/internal/agent/peer"
	"github.com/yasinmiran/robin/internal/protocol"
)

// controlHandlers applies each kind of control message from the browser,
// and bulkHandlers decides on each kind of bulk transfer it opens. Features
// add theirs from init in their own file, so no two edit the same lines;
// written only from init, the maps need no lock.
var (
	controlHandlers = map[protocol.ControlType]controlHandler{}
	bulkHandlers    = map[string]func(*session, protocol.Bulk) (peer.BulkSink, error){}
)

// controlHandler applies one kind of control message from the page of a
// viewer, nil for the owner's. Input applies in the order it came with the
// text the browser types, so it waits behind any being typed; the rest
// applies as it arrives. A guest's message applies only while the guest
// has control, and never one that only the owner's page sends.
type controlHandler struct {
	apply func(*session, *viewer, protocol.Control)
	input bool
	owner bool
}

// handleControl registers h for control messages of type t, which apply as
// they arrive. It panics on a nil h and on a type registered twice.
func handleControl(t protocol.ControlType, h func(*session, protocol.Control)) {
	addControl(t, controlHandler{apply: ignoreViewer(h)})
}

// handleControlFrom is handleControl for an h that needs to know whose
// page sent the message, as one that answers it does.
func handleControlFrom(t protocol.ControlType, h func(*session, *viewer, protocol.Control)) {
	addControl(t, controlHandler{apply: h})
}

// handleOwner registers h for control messages of type t that only the
// owner's page sends: what shapes the stream, the room or the sharing for
// everyone.
func handleOwner(t protocol.ControlType, h func(*session, protocol.Control)) {
	addControl(t, controlHandler{apply: ignoreViewer(h), owner: true})
}

// handleInput registers h for control messages of type t that are input:
// they wait behind text being typed rather than overtake it. It panics as
// handleControl does.
func handleInput(t protocol.ControlType, h func(*session, protocol.Control)) {
	addControl(t, controlHandler{apply: ignoreViewer(h), input: true})
}

func ignoreViewer(h func(*session, protocol.Control)) func(*session, *viewer, protocol.Control) {
	if h == nil {
		return nil
	}
	return func(s *session, _ *viewer, m protocol.Control) { h(s, m) }
}

func addControl(t protocol.ControlType, h controlHandler) {
	if h.apply == nil {
		panic(fmt.Sprintf("agent: nil handler for control %q", t))
	}
	if _, dup := controlHandlers[t]; dup {
		panic(fmt.Sprintf("agent: control %q handled twice", t))
	}
	controlHandlers[t] = h
}

// handleBulk registers h for bulk transfers of kind from the browser. h
// returns where the payload goes, or an error that refuses it. The kind
// needs a limit from protocol.RegisterBulkKind too, which
// TestBulkKindsHaveLimits checks. It panics on a nil h and on a kind
// registered twice.
func handleBulk(kind string, h func(*session, protocol.Bulk) (peer.BulkSink, error)) {
	if h == nil {
		panic(fmt.Sprintf("agent: nil handler for bulk kind %q", kind))
	}
	if _, dup := bulkHandlers[kind]; dup {
		panic(fmt.Sprintf("agent: bulk kind %q handled twice", kind))
	}
	bulkHandlers[kind] = h
}

func init() {
	for _, t := range []protocol.ControlType{
		protocol.ControlKey, protocol.ControlMove, protocol.ControlButton, protocol.ControlWheel, protocol.ControlRelease,
	} {
		handleInput(t, (*session).applyInput)
	}
	handleOwner(protocol.ControlResize, (*session).applyResize)
	handleOwner(protocol.ControlMonitor, (*session).applyMonitor)
}

// control applies one message from the owner's page.
func (s *session) control(m protocol.Control) { s.controlAs(nil, m) }

// controlAs applies one message from v's page, nil for the owner's; types
// nobody handles are ignored, as from a newer page, and so is what a
// guest may not send. Input waits behind text being typed.
func (s *session) controlAs(v *viewer, m protocol.Control) {
	h, ok := controlHandlers[m.T]
	if !ok {
		return
	}
	if v != nil && v.guest != nil && (h.owner || !v.control()) {
		return
	}
	if !h.input {
		h.apply(s, v, m)
		return
	}
	if !s.typing.run(m.T, func(context.Context) { h.apply(s, v, m) }) {
		s.log.Warn("input dropped: typing has fallen behind", "type", m.T)
		// It may have been a key-up; the lane never drops a release.
		s.controlAs(v, protocol.Control{T: protocol.ControlRelease})
	}
}

// bulk picks the handler for a transfer the browser opened.
func (s *session) bulk(h protocol.Bulk) (peer.BulkSink, error) {
	accept := bulkHandlers[h.Kind]
	if accept == nil {
		return nil, fmt.Errorf("%w %q: this agent takes none", protocol.ErrBulkKind, h.Kind)
	}
	return accept(s, h)
}
