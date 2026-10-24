//go:build linux

package agent

import (
	"fmt"

	"github.com/yasinmiran/robin/internal/agent/peer"
	"github.com/yasinmiran/robin/internal/protocol"
)

// controlHandlers applies each kind of control message from the browser,
// and bulkHandlers decides on each kind of bulk transfer it opens. Features
// add theirs from init in their own file, so no two edit the same lines.
var (
	controlHandlers = map[protocol.ControlType]func(*session, protocol.Control){}
	bulkHandlers    = map[string]func(*session, protocol.Bulk) (peer.BulkSink, error){}
)

// handleControl registers h for control messages of type t. It panics on a
// type registered twice.
func handleControl(t protocol.ControlType, h func(*session, protocol.Control)) {
	if _, dup := controlHandlers[t]; dup {
		panic(fmt.Sprintf("agent: control %q handled twice", t))
	}
	controlHandlers[t] = h
}

// handleBulk registers h for bulk transfers of kind from the browser. h
// returns where the payload goes, or an error that refuses it. The kind
// needs a limit from protocol.RegisterBulkKind too. It panics on a kind
// registered twice.
func handleBulk(kind string, h func(*session, protocol.Bulk) (peer.BulkSink, error)) {
	if _, dup := bulkHandlers[kind]; dup {
		panic(fmt.Sprintf("agent: bulk kind %q handled twice", kind))
	}
	bulkHandlers[kind] = h
}

func init() {
	for _, t := range []protocol.ControlType{
		protocol.ControlKey, protocol.ControlMove, protocol.ControlButton, protocol.ControlWheel, protocol.ControlRelease,
	} {
		handleControl(t, (*session).applyInput)
	}
	handleControl(protocol.ControlResize, (*session).applyResize)
	handleControl(protocol.ControlMonitor, (*session).applyMonitor)
	handleControl(protocol.ControlClipboard, (*session).applyClipboard)
}

// control applies one message from the browser; types nobody handles are
// ignored, as from a newer page.
func (s *session) control(m protocol.Control) {
	if h := controlHandlers[m.T]; h != nil {
		h(s, m)
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
