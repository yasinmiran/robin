//go:build linux

package wayland

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/yasinmiran/robin/internal/protocol"
)

// textMimes are the clipboard types offered and accepted for text, best
// first.
var textMimes = []string{"text/plain;charset=utf-8", "text/plain", "UTF8_STRING", "STRING", "TEXT"}

// richMimes are the types of each format the rich clipboard carries.
var richMimes = []string{textMimes[0], protocol.MIMEHTML, protocol.MIMEPNG}

const (
	maxClipboard = protocol.MaxClip
	// clipboardTimeout bounds how long a pipe to or from a program may sit
	// idle before the clipboard gives up on it.
	clipboardTimeout = 2 * time.Second
	clipboardChunk   = 64 << 10
	// inputTimeout bounds a portal call that input or a paste waits on,
	// keys and the paste's offer, so that a portal that stops answering
	// cannot hold the keyboard up for good.
	inputTimeout = time.Second
)

// clipboard syncs the desktop's clipboard through the portal's Clipboard
// interface: SetSelection offers the browser's content by MIME type and
// SelectionTransfer asks for one of them; SelectionOwnerChanged reports a
// copy on the desktop, which SelectionRead fetches type by type.
type clipboard struct {
	s        *portalSession
	updates  chan string
	contents chan protocol.Clip
	stops    []func()
	log      *slog.Logger

	mu      sync.Mutex
	content protocol.Clip // what the agent offers
	// held is the user's clipboard: what a program on the desktop copied
	// or the page set, as last seen, or the text there before the session,
	// once a paste has read it; empty when none of it is in a format the
	// agent reads. known is false until held is any of these. A paste of
	// typed text changes neither.
	held  protocol.Clip
	known bool
	// served counts the requests to read what the agent offers; servedNow
	// is closed and replaced at each one, waking whoever waits for a read.
	served    uint64
	servedNow chan struct{}
	// waiting, where a test sets it, runs each time readsAfter is about
	// to wait for a read, so the test can time a read inside that wait.
	waiting func()
	// paste puts back what pastes of typed text replaced.
	paste pasteRestore
}

func newClipboard(s *portalSession, log *slog.Logger) (*clipboard, error) {
	c := &clipboard{s: s, updates: make(chan string, 1), contents: make(chan protocol.Clip, 1), log: log}
	owner, stopOwner, err := s.p.sigs.watch(portalPath, ifaceClipboard, "SelectionOwnerChanged")
	if err != nil {
		return nil, err
	}
	transfer, stopTransfer, err := s.p.sigs.watch(portalPath, ifaceClipboard, "SelectionTransfer")
	if err != nil {
		stopOwner()
		return nil, err
	}
	c.stops = []func(){stopOwner, stopTransfer}
	go c.serve(transfer)
	go c.follow(owner)
	return c, nil
}

// mimesOf lists the MIME types content can be pasted as.
func mimesOf(content protocol.Clip) []string {
	var mimes []string
	if content.Text != "" {
		mimes = append(mimes, textMimes...)
	}
	if content.HTML != "" {
		mimes = append(mimes, protocol.MIMEHTML)
	}
	if len(content.PNG) > 0 {
		mimes = append(mimes, protocol.MIMEPNG)
	}
	return mimes
}

// dataFor is content as mime, or nil when it has none.
func dataFor(content protocol.Clip, mime string) []byte {
	var data []byte
	switch {
	case mime == protocol.MIMEHTML:
		data = []byte(content.HTML)
	case mime == protocol.MIMEPNG:
		data = content.PNG
	case slices.Contains(textMimes, mime):
		data = []byte(content.Text)
	}
	if len(data) == 0 {
		return nil
	}
	return data
}

// set offers the page's content to the desktop as its clipboard, in place
// of whatever pastes of typed text were to put back.
func (c *clipboard) set(content protocol.Clip) error {
	if len(mimesOf(content)) == 0 {
		return errors.New("nothing to put on the clipboard")
	}
	c.paste.forget()
	c.mu.Lock()
	c.held, c.known = content, true
	c.mu.Unlock()
	return c.offer(content)
}

// offer offers content to the desktop as its clipboard. Empty content goes
// as empty text, which leaves nothing there to paste.
func (c *clipboard) offer(content protocol.Clip) error {
	mimes := mimesOf(content)
	if len(mimes) == 0 {
		mimes = textMimes
	}
	c.mu.Lock()
	c.content = content
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), inputTimeout)
	defer cancel()
	return c.s.p.obj.CallWithContext(ctx, ifaceClipboard+".SetSelection", 0, c.s.handle, vardict{
		"mime_types": dbus.MakeVariant(mimes),
	}).Err
}

func (c *clipboard) close() {
	for _, stop := range c.stops {
		stop()
	}
}

// serve answers paste requests for the selection we offered, each in the
// MIME type it asks for.
func (c *clipboard) serve(transfer <-chan *dbus.Signal) {
	for sig := range transfer {
		if len(sig.Body) < 3 || sig.Body[0] != c.s.handle {
			continue
		}
		mime, _ := sig.Body[1].(string)
		serial, _ := sig.Body[2].(uint32)
		c.countRead()
		ok := c.write(serial, mime) == nil
		c.s.p.obj.Call(ifaceClipboard+".SelectionWriteDone", 0, c.s.handle, serial, ok)
	}
}

// countRead counts one request to read what the agent offers, and wakes
// whoever waits for one.
func (c *clipboard) countRead() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.served++
	if c.servedNow != nil {
		close(c.servedNow)
	}
	c.servedNow = make(chan struct{})
}

// reads is how many requests to read the agent's offers there have been.
func (c *clipboard) reads() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.served
}

// awaitRead waits up to d for a read after the first n, and reports
// whether one came.
func (c *clipboard) awaitRead(ctx context.Context, n uint64, d time.Duration) (bool, error) {
	t := time.NewTimer(d)
	defer t.Stop()
	_, read, err := c.readsAfter(ctx, n, t.C)
	return read, err
}

// awaitQuiet waits up to first for a read after the first n and, once
// reads come, until none has come for quiet. It returns the count then.
func (c *clipboard) awaitQuiet(ctx context.Context, n uint64, first, quiet time.Duration) (uint64, error) {
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		now, read, err := c.readsAfter(ctx, n, t.C)
		if err != nil || !read {
			return now, err
		}
		n = now
		t.Reset(quiet)
	}
}

// readsAfter waits for a read after the first n or for timeout, and
// returns the count and whether it grew.
func (c *clipboard) readsAfter(ctx context.Context, n uint64, timeout <-chan time.Time) (uint64, bool, error) {
	for {
		c.mu.Lock()
		served := c.served
		if c.servedNow == nil {
			c.servedNow = make(chan struct{})
		}
		next := c.servedNow
		c.mu.Unlock()
		if served > n {
			return served, true, nil
		}
		if c.waiting != nil {
			c.waiting()
		}
		select {
		case <-next:
		case <-timeout:
			return served, false, nil
		case <-ctx.Done():
			return served, false, ctx.Err()
		}
	}
}

func (c *clipboard) write(serial uint32, mime string) error {
	c.mu.Lock()
	content := c.content
	c.mu.Unlock()
	data := dataFor(content, mime)
	// Empty content is a put-back's empty text, which pastes as nothing
	// rather than fail, so a clipboard manager reads it in place of the
	// pasted text.
	if data == nil && (content.Size() > 0 || !slices.Contains(textMimes, mime)) {
		return fmt.Errorf("nothing on the clipboard as %s", mime)
	}
	var fd dbus.UnixFD
	if err := c.s.p.obj.Call(ifaceClipboard+".SelectionWrite", 0, c.s.handle, serial).Store(&fd); err != nil {
		return err
	}
	f, err := pollable(fd, "selection-write")
	if err != nil {
		return err
	}
	defer f.Close()
	for len(data) > 0 {
		n := min(len(data), clipboardChunk)
		f.SetWriteDeadline(time.Now().Add(clipboardTimeout))
		if _, err := f.Write(data[:n]); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

// follow reads the clipboard each time a program on the desktop takes it.
func (c *clipboard) follow(owner <-chan *dbus.Signal) {
	defer close(c.updates)
	defer close(c.contents)
	for sig := range owner {
		// Only the newest owner matters.
		for len(owner) > 0 {
			if next, ok := <-owner; ok {
				sig = next
			}
		}
		if len(sig.Body) < 2 || sig.Body[0] != c.s.handle {
			continue
		}
		opts, _ := sig.Body[1].(vardict)
		if mine, _ := opts["session_is_owner"].Value().(bool); mine {
			continue
		}
		// What a program copies is the user's: no paste puts back over it.
		c.paste.forget()
		content := c.readAll(mimeList(opts["mime_types"].Value()))
		c.mu.Lock()
		c.held, c.known = content, true
		c.mu.Unlock()
		if content.Size() == 0 {
			continue
		}
		latest(c.contents, content)
		if content.Text != "" {
			latest(c.updates, content.Text)
		}
	}
}

// readAll reads each format of interest the owner offers, as long as
// they fit maxClipboard together; one that does not fit is left out.
func (c *clipboard) readAll(offered []string) protocol.Clip {
	var content protocol.Clip
	left := maxClipboard
	// KDE's portal can announce a new owner with the formats of the one
	// before it, and never answers a read of one the new owner lacks. Once
	// a read goes unanswered, the rest of the list is not tried either.
	unanswered := false
	read := func(mime string) []byte {
		if unanswered || mime == "" || !slices.Contains(offered, mime) {
			return nil
		}
		b, err := c.read(mime, left)
		if errors.Is(err, context.DeadlineExceeded) {
			unanswered = true
		}
		if err != nil {
			return nil
		}
		left -= len(b)
		return b
	}
	content.Text = strings.ToValidUTF8(string(read(pickText(offered))), "\uFFFD")
	content.HTML = strings.ToValidUTF8(string(read(protocol.MIMEHTML)), "\uFFFD")
	content.PNG = read(protocol.MIMEPNG)
	return content
}

// latest puts v on ch, a channel of one, replacing a value nobody took.
// Only follow sends on these channels.
func latest[T any](ch chan T, v T) {
	select {
	case <-ch:
	default:
	}
	ch <- v
}

// read reads the clipboard as mime, up to limit bytes. The portal's answer
// is bounded by clipboardTimeout too, as a pipe that sits idle is.
func (c *clipboard) read(mime string, limit int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), clipboardTimeout)
	defer cancel()
	var fd dbus.UnixFD
	if err := c.s.p.obj.CallWithContext(ctx, ifaceClipboard+".SelectionRead", 0, c.s.handle, mime).Store(&fd); err != nil {
		return nil, err
	}
	f, err := pollable(fd, "selection-read")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []byte
	buf := make([]byte, clipboardChunk)
	for {
		f.SetReadDeadline(time.Now().Add(clipboardTimeout))
		n, err := f.Read(buf)
		if len(out)+n > limit {
			return nil, errors.New("clipboard too large")
		}
		out = append(out, buf[:n]...)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// pollable wraps a pipe the portal handed over so that deadlines work on it:
// a stalled program at the other end must not hang the clipboard.
func pollable(fd dbus.UnixFD, name string) (*os.File, error) {
	if err := syscall.SetNonblock(int(fd), true); err != nil {
		syscall.Close(int(fd))
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return os.NewFile(uintptr(fd), name), nil
}

// mimeList accepts the specified "as" and the "(as)" that
// xdg-desktop-portal-gnome sends in SelectionOwnerChanged.
func mimeList(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		if len(t) == 1 {
			return mimeList(t[0])
		}
	}
	return nil
}

func pickText(offered []string) string {
	for _, m := range textMimes {
		if slices.Contains(offered, m) {
			return m
		}
	}
	return ""
}
