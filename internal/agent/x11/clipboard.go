//go:build linux

package x11

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xfixes"
	"github.com/jezek/xgb/xproto"
)

// MaxClipboard caps clipboard text in either direction.
const MaxClipboard = 1 << 20

const (
	ownTimeout = 2 * time.Second
	// handOffTimeout bounds how long Close waits for a clipboard manager to
	// copy the text we hold.
	handOffTimeout = time.Second
)

// clipboard owns CLIPBOARD from a hidden window when the browser sets it, and
// reads it whenever another client becomes the owner. The event loop calls
// the on* methods; SetClipboard runs on the caller's goroutine.
type clipboard struct {
	x      *xgb.Conn
	win    xproto.Window
	maxReq int
	a      atoms

	setMu    sync.Mutex // one SetClipboard at a time
	stamps   chan xproto.Timestamp
	saved    chan struct{} // the clipboard manager answered SAVE_TARGETS
	loopDone <-chan struct{}

	mu    sync.Mutex
	owned bool
	since xproto.Timestamp
	text  string

	updates chan string

	// Event loop only: an INCR transfer in progress.
	incoming *incr
}

type incr struct {
	data     []byte
	overflow bool
}

type atoms struct {
	clipboard, targets, utf8, text, plain, incr, data, stamp xproto.Atom
	manager, saveTargets, saveList                           xproto.Atom
}

func (cb *clipboard) init(x *xgb.Conn, root xproto.Window, maxReq int, loopDone <-chan struct{}) error {
	cb.x, cb.maxReq, cb.loopDone = x, maxReq, loopDone
	cb.stamps = make(chan xproto.Timestamp, 1)
	cb.saved = make(chan struct{}, 1)
	cb.updates = make(chan string, 1)
	names := []string{"CLIPBOARD", "TARGETS", "UTF8_STRING", "TEXT", "text/plain;charset=utf-8",
		"INCR", "_ROBIN_SELECTION", "_ROBIN_TIMESTAMP",
		"CLIPBOARD_MANAGER", "SAVE_TARGETS", "_ROBIN_SAVE_TARGETS"}
	ids := make([]xproto.Atom, len(names))
	for i, n := range names {
		r, err := xproto.InternAtom(x, false, uint16(len(n)), n).Reply()
		if err != nil {
			return err
		}
		ids[i] = r.Atom
	}
	cb.a = atoms{ids[0], ids[1], ids[2], ids[3], ids[4], ids[5], ids[6], ids[7], ids[8], ids[9], ids[10]}

	var err error
	if cb.win, err = xproto.NewWindowId(x); err != nil {
		return err
	}
	err = xproto.CreateWindowChecked(x, 0, cb.win, root, -10, -10, 1, 1, 0,
		xproto.WindowClassInputOnly, 0,
		xproto.CwOverrideRedirect|xproto.CwEventMask,
		[]uint32{1, xproto.EventMaskPropertyChange}).Check()
	if err != nil {
		return err
	}
	return xfixes.SelectSelectionInputChecked(x, cb.win, cb.a.clipboard,
		xfixes.SelectionEventMaskSetSelectionOwner).Check()
}

// SetClipboard makes text the CLIPBOARD contents, served from this
// connection until another client takes the selection.
func (c *Conn) SetClipboard(text string) error {
	if len(text) > MaxClipboard {
		return fmt.Errorf("x11: clipboard text is %d bytes, limit %d", len(text), MaxClipboard)
	}
	cb := &c.clip
	cb.setMu.Lock()
	defer cb.setMu.Unlock()

	// ICCCM wants a real timestamp for SetSelectionOwner. A zero-length
	// append changes nothing but makes the server send one back.
	select {
	case <-cb.stamps:
	default:
	}
	xproto.ChangeProperty(cb.x, xproto.PropModeAppend, cb.win, cb.a.stamp, xproto.AtomString, 8, 0, nil)
	var t xproto.Timestamp
	select {
	case t = <-cb.stamps:
	case <-cb.loopDone:
		return errors.New("x11: connection closed")
	case <-time.After(ownTimeout):
		return errors.New("x11: no timestamp from the server")
	}

	cb.mu.Lock()
	cb.owned, cb.since, cb.text = true, t, text
	cb.mu.Unlock()
	xproto.SetSelectionOwner(cb.x, cb.win, cb.a.clipboard, t)
	r, err := xproto.GetSelectionOwner(cb.x, cb.a.clipboard).Reply()
	if err == nil && r.Owner != cb.win {
		err = errors.New("another client holds a newer selection")
	}
	if err != nil {
		cb.mu.Lock()
		cb.owned, cb.text = false, ""
		cb.mu.Unlock()
		return fmt.Errorf("x11: own clipboard: %w", err)
	}
	return nil
}

// Clipboard delivers CLIPBOARD text whenever another client takes the
// selection. Only the latest text is kept if the reader falls behind. The
// channel is closed when the connection closes.
func (c *Conn) Clipboard() <-chan string { return c.clip.updates }

func (cb *clipboard) publish(text string) {
	select {
	case cb.updates <- text:
		return
	default:
	}
	select { // full: replace the stale value
	case <-cb.updates:
	default:
	}
	cb.updates <- text
}

func (cb *clipboard) closeUpdates() { close(cb.updates) }

func (cb *clipboard) onProperty(e xproto.PropertyNotifyEvent) {
	if e.Window != cb.win || e.State != xproto.PropertyNewValue {
		return
	}
	switch e.Atom {
	case cb.a.stamp:
		select {
		case cb.stamps <- e.Time:
		default:
		}
	case cb.a.data:
		if cb.incoming != nil {
			cb.readChunk()
		}
	}
}

func (cb *clipboard) onClear(e xproto.SelectionClearEvent) {
	if e.Selection != cb.a.clipboard {
		return
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()
	// A clear queued before our latest SetSelectionOwner is stale.
	if cb.owned && int32(e.Time-cb.since) >= 0 {
		cb.owned, cb.text = false, ""
	}
}

// onOwner asks a new foreign owner for its text as UTF-8.
func (cb *clipboard) onOwner(e xfixes.SelectionNotifyEvent) {
	if e.Selection != cb.a.clipboard || e.Owner == cb.win || e.Owner == 0 {
		return
	}
	cb.incoming = nil
	xproto.DeleteProperty(cb.x, cb.win, cb.a.data)
	xproto.ConvertSelection(cb.x, cb.win, cb.a.clipboard, cb.a.utf8, cb.a.data, e.SelectionTimestamp)
}

// handOff asks the session's clipboard manager, when it runs one, to copy
// the text we serve, so that text stays on the clipboard once we disconnect
// (the freedesktop.org clipboard manager protocol). Without a manager the
// selection simply loses its owner, as when any X client exits.
func (cb *clipboard) handOff() {
	cb.mu.Lock()
	owned := cb.owned
	cb.mu.Unlock()
	if !owned {
		return
	}
	r, err := xproto.GetSelectionOwner(cb.x, cb.a.manager).Reply()
	if err != nil || r.Owner == 0 {
		return
	}
	list := []xproto.Atom{cb.a.utf8, xproto.AtomString, cb.a.text, cb.a.plain}
	buf := make([]byte, 4*len(list))
	for i, a := range list {
		xgb.Put32(buf[4*i:], uint32(a))
	}
	xproto.ChangeProperty(cb.x, xproto.PropModeReplace, cb.win, cb.a.saveList, xproto.AtomAtom, 32, uint32(len(list)), buf)
	xproto.ConvertSelection(cb.x, cb.win, cb.a.manager, cb.a.saveTargets, cb.a.saveList, xproto.TimeCurrentTime)
	select {
	case <-cb.saved:
	case <-cb.loopDone:
	case <-time.After(handOffTimeout):
	}
}

// onNotify handles the answer to a ConvertSelection of ours.
func (cb *clipboard) onNotify(e xproto.SelectionNotifyEvent) {
	if e.Requestor == cb.win && e.Selection == cb.a.manager {
		select {
		case cb.saved <- struct{}{}:
		default:
		}
		return
	}
	if e.Requestor != cb.win || e.Selection != cb.a.clipboard || e.Property == 0 {
		return
	}
	r, err := xproto.GetProperty(cb.x, true, cb.win, cb.a.data, xproto.GetPropertyTypeAny, 0, MaxClipboard/4+1).Reply()
	if err != nil {
		return
	}
	switch {
	case r.Type == cb.a.incr:
		// Deleting the INCR property (done by the read) starts the transfer.
		cb.incoming = &incr{}
	case r.BytesAfter > 0:
		xproto.DeleteProperty(cb.x, cb.win, cb.a.data) // too big; drop it
	default:
		cb.publish(decodeText(r.Type, r.Value))
	}
}

// readChunk consumes one INCR chunk; an empty chunk ends the transfer.
func (cb *clipboard) readChunk() {
	r, err := xproto.GetProperty(cb.x, true, cb.win, cb.a.data, xproto.GetPropertyTypeAny, 0, MaxClipboard/4+1).Reply()
	if err != nil {
		cb.incoming = nil
		return
	}
	in := cb.incoming
	if len(r.Value) == 0 {
		cb.incoming = nil
		if !in.overflow {
			cb.publish(decodeText(r.Type, in.data))
		}
		return
	}
	if r.BytesAfter > 0 {
		xproto.DeleteProperty(cb.x, cb.win, cb.a.data)
	}
	if in.overflow = in.overflow || r.BytesAfter > 0 || len(in.data)+len(r.Value) > MaxClipboard; in.overflow {
		in.data = nil // keep reading so the owner can finish, but keep nothing
		return
	}
	in.data = append(in.data, r.Value...)
}

// onRequest serves our text to another client.
func (cb *clipboard) onRequest(e xproto.SelectionRequestEvent) {
	prop := e.Property
	if prop == 0 { // pre-ICCCM requestor
		prop = e.Target
	}
	cb.mu.Lock()
	owned, text := cb.owned, cb.text
	cb.mu.Unlock()
	if e.Selection != cb.a.clipboard || e.Owner != cb.win || !owned || !cb.answer(e, prop, text) {
		prop = 0 // refusal
	}
	ev := xproto.SelectionNotifyEvent{
		Time: e.Time, Requestor: e.Requestor, Selection: e.Selection, Target: e.Target, Property: prop,
	}
	xproto.SendEvent(cb.x, false, e.Requestor, xproto.EventMaskNoEvent, string(ev.Bytes()))
}

func (cb *clipboard) answer(e xproto.SelectionRequestEvent, prop xproto.Atom, text string) bool {
	switch e.Target {
	case cb.a.targets:
		list := []xproto.Atom{cb.a.targets, cb.a.utf8, xproto.AtomString, cb.a.text, cb.a.plain}
		buf := make([]byte, 4*len(list))
		for i, a := range list {
			xgb.Put32(buf[4*i:], uint32(a))
		}
		xproto.ChangeProperty(cb.x, xproto.PropModeReplace, e.Requestor, prop, xproto.AtomAtom, 32, uint32(len(list)), buf)
	case cb.a.utf8, cb.a.text:
		cb.write(e.Requestor, prop, cb.a.utf8, []byte(text))
	case cb.a.plain:
		cb.write(e.Requestor, prop, cb.a.plain, []byte(text))
	case xproto.AtomString:
		cb.write(e.Requestor, prop, xproto.AtomString, latin1(text))
	default:
		return false
	}
	return true
}

// write stores data in pieces: xgb cannot send a request over the core
// 256 KiB limit, so a large text is appended chunk by chunk.
func (cb *clipboard) write(w xproto.Window, prop, typ xproto.Atom, data []byte) {
	mode := byte(xproto.PropModeReplace)
	for {
		n := min(len(data), cb.maxReq)
		xproto.ChangeProperty(cb.x, mode, w, prop, typ, 8, uint32(n), data[:n])
		data = data[n:]
		if len(data) == 0 {
			return
		}
		mode = xproto.PropModeAppend
	}
}

// latin1 encodes text for the STRING target, which ICCCM defines as
// ISO 8859-1; characters outside it become '?'.
func latin1(text string) []byte {
	b := make([]byte, 0, len(text))
	for _, r := range text {
		if r > 0xff {
			r = '?'
		}
		b = append(b, byte(r))
	}
	return b
}

func decodeText(typ xproto.Atom, data []byte) string {
	if typ != xproto.AtomString {
		return string(data)
	}
	r := make([]rune, len(data))
	for i, b := range data {
		r[i] = rune(b)
	}
	return string(r)
}
