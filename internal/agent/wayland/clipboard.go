//go:build linux

package wayland

import (
	"errors"
	"fmt"
	"io"
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

const (
	maxClipboard = protocol.MaxClip
	// clipboardTimeout bounds how long a pipe to or from a program may sit
	// idle before the clipboard gives up on it.
	clipboardTimeout = 2 * time.Second
	clipboardChunk   = 64 << 10
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

	mu      sync.Mutex
	content protocol.Clip
}

func newClipboard(s *portalSession) (*clipboard, error) {
	c := &clipboard{s: s, updates: make(chan string, 1), contents: make(chan protocol.Clip, 1)}
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

// set offers content to the desktop as its clipboard.
func (c *clipboard) set(content protocol.Clip) error {
	mimes := mimesOf(content)
	if len(mimes) == 0 {
		return errors.New("nothing to put on the clipboard")
	}
	c.mu.Lock()
	c.content = content
	c.mu.Unlock()
	return c.s.p.obj.Call(ifaceClipboard+".SetSelection", 0, c.s.handle, vardict{
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
		ok := c.write(serial, mime) == nil
		c.s.p.obj.Call(ifaceClipboard+".SelectionWriteDone", 0, c.s.handle, serial, ok)
	}
}

func (c *clipboard) write(serial uint32, mime string) error {
	c.mu.Lock()
	data := dataFor(c.content, mime)
	c.mu.Unlock()
	if data == nil {
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
		content := c.readAll(mimeList(opts["mime_types"].Value()))
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
	read := func(mime string) []byte {
		if mime == "" || !slices.Contains(offered, mime) {
			return nil
		}
		b, err := c.read(mime, left)
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

func (c *clipboard) read(mime string, limit int) ([]byte, error) {
	var fd dbus.UnixFD
	if err := c.s.p.obj.Call(ifaceClipboard+".SelectionRead", 0, c.s.handle, mime).Store(&fd); err != nil {
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
