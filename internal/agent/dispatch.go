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

// controlHandler applies one kind of control message. Input applies in the
// order it came with the text the browser types, so it waits behind any
// being typed; the rest applies as it arrives.
type controlHandler struct {
	apply func(*session, protocol.Control)
	input bool
}

// handleControl registers h for control messages of type t, which apply as
// they arrive. It panics on a nil h and on a type registered twice.
func handleControl(t protocol.ControlType, h func(*session, protocol.Control)) {
	addControl(t, controlHandler{apply: h})
}

// handleInput registers h for control messages of type t that are input:
// they wait behind text being typed rather than overtake it. It panics as
// handleControl does.
func handleInput(t protocol.ControlType, h func(*session, protocol.Control)) {
	addControl(t, controlHandler{apply: h, input: true})
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
	handleControl(protocol.ControlResize, (*session).applyResize)
	handleControl(protocol.ControlMonitor, (*session).applyMonitor)
	// The page sets the clipboard before it pastes, so that must not
	// overtake text typed before it either.
	handleInput(protocol.ControlClipboard, (*session).applyClipboard)
}

// control applies one message from the browser; types nobody handles are
// ignored, as from a newer page. Input waits behind text being typed.
func (s *session) control(m protocol.Control) {
	h, ok := controlHandlers[m.T]
	if !ok {
		return
	}
	if !h.input {
		h.apply(s, m)
		return
	}
	if !s.typing.run(m.T, func(context.Context) { h.apply(s, m) }) {
		s.log.Warn("input dropped: typing has fallen behind", "type", m.T)
		// It may have been a key-up; the lane never drops a release.
		s.control(protocol.Control{T: protocol.ControlRelease})
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
