//go:build linux

package x11

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xfixes"
	"github.com/jezek/xgb/xproto"

	"github.com/yasinmiran/robin/internal/protocol"
)

// MaxClipboard caps clipboard content, every format of it together, in
// either direction.
const MaxClipboard = protocol.MaxClip

const (
	ownTimeout = 2 * time.Second
	// handOffTimeout bounds how long Close waits for a clipboard manager to
	// copy what we hold.
	handOffTimeout = time.Second
	// incrTimeout is how long an INCR transfer to another client may wait
	// for it to take the next chunk before it is dropped as abandoned.
	incrTimeout = 30 * time.Second
	// maxIncr bounds the INCR transfers in flight at once.
	maxIncr = 16
)

// clipboard owns CLIPBOARD from a hidden window when the browser sets it, and
// reads it whenever another client becomes the owner. The event loop calls
// the on* methods; SetClipboardContent runs on the caller's goroutine.
type clipboard struct {
	x      *xgb.Conn
	win    xproto.Window
	maxReq int
	a      atoms

	setMu    sync.Mutex // one SetClipboardContent at a time
	stamps   chan xproto.Timestamp
	saved    chan struct{} // the clipboard manager answered SAVE_TARGETS
	loopDone <-chan struct{}

	mu      sync.Mutex
	owned   bool
	since   xproto.Timestamp
	content protocol.Clip

	updates  chan string
	contents chan protocol.Clip

	// Event loop only: the copy being read from another client, and the
	// INCR transfers under way to others.
	reading *reading
	sending map[incrKey]*incrOut
}

// reading follows one copy by another client: its TARGETS first, then
// each format of interest it offers, one conversion at a time.
type reading struct {
	stamp  xproto.Timestamp
	target xproto.Atom   // asked for now
	queue  []xproto.Atom // still to ask for
	got    protocol.Clip
	size   int   // bytes kept so far
	incr   *incr // the current target, while it arrives by INCR
}

type incr struct {
	typ      xproto.Atom
	data     []byte
	overflow bool
}

type incrKey struct {
	win  xproto.Window
	prop xproto.Atom
}

// incrOut is data on its way to another client by INCR, a chunk each time
// it deletes the property.
type incrOut struct {
	typ  xproto.Atom
	data []byte
	at   time.Time // of the last chunk
}

type atoms struct {
	clipboard, targets, utf8, text, plain, incr, data, stamp xproto.Atom
	manager, saveTargets, saveList                           xproto.Atom
	html, png                                                xproto.Atom
}

func (cb *clipboard) init(x *xgb.Conn, root xproto.Window, maxReq int, loopDone <-chan struct{}) error {
	cb.x, cb.maxReq, cb.loopDone = x, maxReq, loopDone
	cb.stamps = make(chan xproto.Timestamp, 1)
	cb.saved = make(chan struct{}, 1)
	cb.updates = make(chan string, 1)
	cb.contents = make(chan protocol.Clip, 1)
	cb.sending = map[incrKey]*incrOut{}
	names := []string{"CLIPBOARD", "TARGETS", "UTF8_STRING", "TEXT", "text/plain;charset=utf-8",
		"INCR", "_ROBIN_SELECTION", "_ROBIN_TIMESTAMP",
		"CLIPBOARD_MANAGER", "SAVE_TARGETS", "_ROBIN_SAVE_TARGETS",
		protocol.MIMEHTML, protocol.MIMEPNG}
	ids := make([]xproto.Atom, len(names))
	for i, n := range names {
		r, err := xproto.InternAtom(x, false, uint16(len(n)), n).Reply()
		if err != nil {
			return err
		}
		ids[i] = r.Atom
	}
	cb.a = atoms{ids[0], ids[1], ids[2], ids[3], ids[4], ids[5], ids[6], ids[7], ids[8], ids[9], ids[10], ids[11], ids[12]}

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
	return c.SetClipboardContent(protocol.Clip{Text: text})
}

// SetClipboardContent makes clip the CLIPBOARD contents, offered as text,
// HTML and a PNG image as far as clip holds them, and served from this
// connection until another client takes the selection.
func (c *Conn) SetClipboardContent(clip protocol.Clip) error {
	if clip.Size() > MaxClipboard {
		return fmt.Errorf("x11: clipboard content is %d bytes, limit %d", clip.Size(), MaxClipboard)
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
	cb.owned, cb.since, cb.content = true, t, clip
	cb.mu.Unlock()
	xproto.SetSelectionOwner(cb.x, cb.win, cb.a.clipboard, t)
	r, err := xproto.GetSelectionOwner(cb.x, cb.a.clipboard).Reply()
	if err == nil && r.Owner != cb.win {
		err = errors.New("another client holds a newer selection")
	}
	if err != nil {
		cb.mu.Lock()
		cb.owned, cb.content = false, protocol.Clip{}
		cb.mu.Unlock()
		return fmt.Errorf("x11: own clipboard: %w", err)
	}
	return nil
}

// Clipboard delivers CLIPBOARD text whenever another client takes the
// selection with some. Only the latest text is kept if the reader falls
// behind. The channel is closed when the connection closes.
func (c *Conn) Clipboard() <-chan string { return c.clip.updates }

// ClipboardContent delivers the CLIPBOARD contents, as text, HTML and a PNG
// image as far as the owner offers them, whenever another client takes the
// selection. Only the latest is kept if the reader falls behind. The
// channel is closed when the connection closes.
func (c *Conn) ClipboardContent() <-chan protocol.Clip { return c.clip.contents }

func (cb *clipboard) publish(clip protocol.Clip) {
	latest(cb.contents, clip)
	if clip.Text != "" {
		latest(cb.updates, clip.Text)
	}
}

// latest puts v on ch, a channel of one, replacing a value nobody took.
// Only the event loop sends on these channels.
func latest[T any](ch chan T, v T) {
	select {
	case ch <- v:
		return
	default:
	}
	select { // full: replace the stale value
	case <-ch:
	default:
	}
	ch <- v
}

func (cb *clipboard) closeUpdates() {
	close(cb.updates)
	close(cb.contents)
}

func (cb *clipboard) onProperty(e xproto.PropertyNotifyEvent) {
	if e.Window != cb.win {
		if e.State == xproto.PropertyDelete {
			cb.sendChunk(incrKey{e.Window, e.Atom})
		}
		return
	}
	if e.State != xproto.PropertyNewValue {
		return
	}
	switch e.Atom {
	case cb.a.stamp:
		select {
		case cb.stamps <- e.Time:
		default:
		}
	case cb.a.data:
		if cb.reading != nil && cb.reading.incr != nil {
			cb.readChunk(cb.reading)
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
		cb.owned, cb.content = false, protocol.Clip{}
	}
}

// onOwner starts reading what a new foreign owner offers, from its list
// of targets.
func (cb *clipboard) onOwner(e xfixes.SelectionNotifyEvent) {
	if e.Selection != cb.a.clipboard || e.Owner == cb.win || e.Owner == 0 {
		return
	}
	cb.reading = &reading{stamp: e.SelectionTimestamp}
	cb.ask(cb.reading, cb.a.targets)
}

func (cb *clipboard) ask(r *reading, target xproto.Atom) {
	r.target, r.incr = target, nil
	xproto.DeleteProperty(cb.x, cb.win, cb.a.data)
	xproto.ConvertSelection(cb.x, cb.win, cb.a.clipboard, target, cb.a.data, r.stamp)
}

// next asks for the next target, or publishes what was read once none is
// left.
func (cb *clipboard) next(r *reading) {
	if len(r.queue) > 0 {
		target := r.queue[0]
		r.queue = r.queue[1:]
		cb.ask(r, target)
		return
	}
	cb.reading = nil
	if r.got.Size() > 0 {
		cb.publish(r.got)
	}
}

// wanted picks what to read of the targets an owner offers: the best of
// its text targets, HTML and PNG. TEXT is left out, since it may come as
// COMPOUND_TEXT.
func (cb *clipboard) wanted(offered []xproto.Atom) []xproto.Atom {
	has := func(a xproto.Atom) bool {
		for _, o := range offered {
			if o == a {
				return true
			}
		}
		return false
	}
	var out []xproto.Atom
	for _, a := range []xproto.Atom{cb.a.utf8, cb.a.plain, xproto.AtomString} {
		if has(a) {
			out = append(out, a)
			break
		}
	}
	for _, a := range []xproto.Atom{cb.a.html, cb.a.png} {
		if has(a) {
			out = append(out, a)
		}
	}
	return out
}

// keep stores what arrived for the current target and moves on.
func (cb *clipboard) keep(r *reading, typ xproto.Atom, data []byte) {
	switch r.target {
	case cb.a.targets:
		r.queue = cb.wanted(atomList(data))
	case cb.a.html:
		r.got.HTML = decodeHTML(data)
	case cb.a.png:
		r.got.PNG = data
	default:
		r.got.Text = decodeText(typ, data)
	}
	if r.target != cb.a.targets {
		r.size += len(data)
	}
	cb.next(r)
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
	r := cb.reading
	if e.Requestor != cb.win || e.Selection != cb.a.clipboard || r == nil || e.Target != r.target {
		return
	}
	if e.Property == 0 {
		// The owner cannot convert to this target. One that lists no
		// TARGETS may still give text.
		if r.target == cb.a.targets {
			r.queue = []xproto.Atom{cb.a.utf8}
		}
		cb.next(r)
		return
	}
	reply, err := xproto.GetProperty(cb.x, true, cb.win, cb.a.data, xproto.GetPropertyTypeAny, 0, uint32(MaxClipboard-r.size)/4+1).Reply()
	if err != nil {
		cb.reading = nil
		return
	}
	switch {
	case reply.Type == cb.a.incr:
		// Deleting the INCR property (done by the read) starts the transfer.
		r.incr = &incr{}
	case reply.BytesAfter > 0:
		xproto.DeleteProperty(cb.x, cb.win, cb.a.data) // too big; skip it
		cb.next(r)
	default:
		cb.keep(r, reply.Type, reply.Value)
	}
}

// readChunk consumes one INCR chunk; an empty chunk ends the transfer.
func (cb *clipboard) readChunk(r *reading) {
	reply, err := xproto.GetProperty(cb.x, true, cb.win, cb.a.data, xproto.GetPropertyTypeAny, 0, MaxClipboard/4+1).Reply()
	if err != nil {
		cb.reading = nil
		return
	}
	in := r.incr
	if len(reply.Value) == 0 {
		r.incr = nil
		if in.overflow {
			cb.next(r)
		} else {
			cb.keep(r, in.typ, in.data)
		}
		return
	}
	if reply.BytesAfter > 0 {
		xproto.DeleteProperty(cb.x, cb.win, cb.a.data)
	}
	in.typ = reply.Type
	if in.overflow = in.overflow || reply.BytesAfter > 0 || r.size+len(in.data)+len(reply.Value) > MaxClipboard; in.overflow {
		in.data = nil // keep reading so the owner can finish, but keep nothing
		return
	}
	in.data = append(in.data, reply.Value...)
}

// handOff asks the session's clipboard manager, when it runs one, to copy
// what we serve, so it stays on the clipboard once we disconnect (the
// freedesktop.org clipboard manager protocol). Without a manager the
// selection simply loses its owner, as when any X client exits.
func (cb *clipboard) handOff() {
	cb.mu.Lock()
	owned, content := cb.owned, cb.content
	cb.mu.Unlock()
	if !owned {
		return
	}
	r, err := xproto.GetSelectionOwner(cb.x, cb.a.manager).Reply()
	if err != nil || r.Owner == 0 {
		return
	}
	list := cb.targetsOf(content)[1:] // all but TARGETS itself
	xproto.ChangeProperty(cb.x, xproto.PropModeReplace, cb.win, cb.a.saveList, xproto.AtomAtom, 32, uint32(len(list)), atomBytes(list))
	xproto.ConvertSelection(cb.x, cb.win, cb.a.manager, cb.a.saveTargets, cb.a.saveList, xproto.TimeCurrentTime)
	select {
	case <-cb.saved:
	case <-cb.loopDone:
	case <-time.After(handOffTimeout):
	}
}

// targetsOf lists what clip can be converted to, TARGETS first.
func (cb *clipboard) targetsOf(clip protocol.Clip) []xproto.Atom {
	list := []xproto.Atom{cb.a.targets}
	if clip.Text != "" {
		list = append(list, cb.a.utf8, xproto.AtomString, cb.a.text, cb.a.plain)
	}
	if clip.HTML != "" {
		list = append(list, cb.a.html)
	}
	if len(clip.PNG) > 0 {
		list = append(list, cb.a.png)
	}
	return list
}

// onRequest serves what we hold to another client.
func (cb *clipboard) onRequest(e xproto.SelectionRequestEvent) {
	prop := e.Property
	if prop == 0 { // pre-ICCCM requestor
		prop = e.Target
	}
	cb.mu.Lock()
	owned, content := cb.owned, cb.content
	cb.mu.Unlock()
	if e.Selection != cb.a.clipboard || e.Owner != cb.win || !owned || !cb.answer(e, prop, content) {
		prop = 0 // refusal
	}
	ev := xproto.SelectionNotifyEvent{
		Time: e.Time, Requestor: e.Requestor, Selection: e.Selection, Target: e.Target, Property: prop,
	}
	xproto.SendEvent(cb.x, false, e.Requestor, xproto.EventMaskNoEvent, string(ev.Bytes()))
}

func (cb *clipboard) answer(e xproto.SelectionRequestEvent, prop xproto.Atom, clip protocol.Clip) bool {
	text := clip.Text != ""
	switch {
	case e.Target == cb.a.targets:
		list := cb.targetsOf(clip)
		xproto.ChangeProperty(cb.x, xproto.PropModeReplace, e.Requestor, prop, xproto.AtomAtom, 32, uint32(len(list)), atomBytes(list))
	case text && (e.Target == cb.a.utf8 || e.Target == cb.a.text):
		cb.write(e.Requestor, prop, cb.a.utf8, []byte(clip.Text))
	case text && e.Target == cb.a.plain:
		cb.write(e.Requestor, prop, cb.a.plain, []byte(clip.Text))
	case text && e.Target == xproto.AtomString:
		cb.write(e.Requestor, prop, xproto.AtomString, latin1(clip.Text))
	case clip.HTML != "" && e.Target == cb.a.html:
		cb.write(e.Requestor, prop, cb.a.html, []byte(clip.HTML))
	case len(clip.PNG) > 0 && e.Target == cb.a.png:
		cb.write(e.Requestor, prop, cb.a.png, clip.PNG)
	default:
		return false
	}
	return true
}

// write stores data in the requestor's property: at once when it fits one
// request, else by INCR, a chunk each time the requestor deletes the
// property, as ICCCM asks for data larger than a request.
func (cb *clipboard) write(w xproto.Window, prop, typ xproto.Atom, data []byte) {
	if len(data) <= cb.maxReq {
		xproto.ChangeProperty(cb.x, xproto.PropModeReplace, w, prop, typ, 8, uint32(len(data)), data)
		return
	}
	now := time.Now()
	for k, o := range cb.sending {
		if now.Sub(o.at) > incrTimeout {
			cb.dropIncr(k)
		}
	}
	for len(cb.sending) >= maxIncr {
		var oldest incrKey
		var at time.Time
		for k, o := range cb.sending {
			if at.IsZero() || o.at.Before(at) {
				oldest, at = k, o.at
			}
		}
		cb.dropIncr(oldest)
	}
	// The requestor's deletes must reach us before the first one happens.
	xproto.ChangeWindowAttributes(cb.x, w, xproto.CwEventMask, []uint32{xproto.EventMaskPropertyChange})
	size := make([]byte, 4)
	xgb.Put32(size, uint32(len(data)))
	xproto.ChangeProperty(cb.x, xproto.PropModeReplace, w, prop, cb.a.incr, 32, 1, size)
	cb.sending[incrKey{w, prop}] = &incrOut{typ: typ, data: data, at: now}
}

// sendChunk sends the next chunk of an INCR transfer once its requestor
// has taken the last one; the empty chunk after the data ends it.
func (cb *clipboard) sendChunk(k incrKey) {
	o := cb.sending[k]
	if o == nil {
		return
	}
	n := min(len(o.data), cb.maxReq)
	xproto.ChangeProperty(cb.x, xproto.PropModeReplace, k.win, k.prop, o.typ, 8, uint32(n), o.data[:n])
	o.data, o.at = o.data[n:], time.Now()
	if n == 0 {
		cb.dropIncr(k)
	}
}

// dropIncr forgets an INCR transfer, and stops listening to its window's
// properties when no other transfer goes there.
func (cb *clipboard) dropIncr(k incrKey) {
	delete(cb.sending, k)
	for other := range cb.sending {
		if other.win == k.win {
			return
		}
	}
	xproto.ChangeWindowAttributes(cb.x, k.win, xproto.CwEventMask, []uint32{0})
}

func atomBytes(list []xproto.Atom) []byte {
	buf := make([]byte, 4*len(list))
	for i, a := range list {
		xgb.Put32(buf[4*i:], uint32(a))
	}
	return buf
}

func atomList(data []byte) []xproto.Atom {
	list := make([]xproto.Atom, len(data)/4)
	for i := range list {
		list[i] = xproto.Atom(xgb.Get32(data[4*i:]))
	}
	return list
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
		return strings.ToValidUTF8(string(data), "\uFFFD")
	}
	r := make([]rune, len(data))
	for i, b := range data {
		r[i] = rune(b)
	}
	return string(r)
}

// decodeHTML reads text/html as UTF-8, or as UTF-16 when it starts with a
// byte order mark, which Firefox has served it as.
func decodeHTML(data []byte) string {
	if len(data) >= 2 && (data[0] == 0xff && data[1] == 0xfe || data[0] == 0xfe && data[1] == 0xff) {
		big := data[0] == 0xfe
		units := make([]uint16, (len(data)-2)/2)
		for i := range units {
			lo, hi := data[2+2*i], data[3+2*i]
			if big {
				lo, hi = hi, lo
			}
			units[i] = uint16(hi)<<8 | uint16(lo)
		}
		return string(utf16.Decode(units))
	}
	return strings.ToValidUTF8(string(data), "\uFFFD")
}
