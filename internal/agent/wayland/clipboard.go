//go:build linux

package wayland

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
)

// textMimes are the clipboard types offered and accepted, best first.
var textMimes = []string{"text/plain;charset=utf-8", "text/plain", "UTF8_STRING", "STRING", "TEXT"}

const (
	maxClipboard     = 1 << 20
	clipboardTimeout = 2 * time.Second
)

// clipboard syncs the desktop's clipboard through the portal's Clipboard
// interface: SetSelection offers the browser's text and SelectionTransfer
// asks for it; SelectionOwnerChanged reports a copy on the desktop, which
// SelectionRead fetches.
type clipboard struct {
	s       *portalSession
	updates chan string
	stops   []func()

	log *slog.Logger

	mu   sync.Mutex
	text string // what the agent offers
	// held is the user's clipboard: the text a program on the desktop
	// copied or the page set, as last seen, or what was there before the
	// session, once a paste has read it. heldOK is false when what is there
	// is not text, and known when held is any of these. A paste of typed
	// text changes none of them.
	held   string
	heldOK bool
	known  bool
	// paste puts back what pastes of typed text replaced.
	paste pasteRestore
}

func newClipboard(s *portalSession, log *slog.Logger) (*clipboard, error) {
	c := &clipboard{s: s, updates: make(chan string, 1), log: log}
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

// set offers the page's text to the desktop as its clipboard, in place of
// whatever pastes of typed text were to put back.
func (c *clipboard) set(text string) error {
	c.paste.forget()
	c.mu.Lock()
	c.held, c.heldOK, c.known = text, true, true
	c.mu.Unlock()
	return c.offer(text)
}

// offer offers text to the desktop as its clipboard.
func (c *clipboard) offer(text string) error {
	c.mu.Lock()
	c.text = text
	c.mu.Unlock()
	return c.s.p.obj.Call(ifaceClipboard+".SetSelection", 0, c.s.handle, vardict{
		"mime_types": dbus.MakeVariant(textMimes),
	}).Err
}

func (c *clipboard) close() {
	for _, stop := range c.stops {
		stop()
	}
}

// serve answers paste requests for the selection we offered.
func (c *clipboard) serve(transfer <-chan *dbus.Signal) {
	for sig := range transfer {
		if len(sig.Body) < 3 || sig.Body[0] != c.s.handle {
			continue
		}
		serial, _ := sig.Body[2].(uint32)
		ok := c.write(serial) == nil
		c.s.p.obj.Call(ifaceClipboard+".SelectionWriteDone", 0, c.s.handle, serial, ok)
	}
}

func (c *clipboard) write(serial uint32) error {
	var fd dbus.UnixFD
	if err := c.s.p.obj.Call(ifaceClipboard+".SelectionWrite", 0, c.s.handle, serial).Store(&fd); err != nil {
		return err
	}
	f, err := pollable(fd, "selection-write")
	if err != nil {
		return err
	}
	defer f.Close()
	c.mu.Lock()
	text := c.text
	c.mu.Unlock()
	f.SetWriteDeadline(time.Now().Add(clipboardTimeout))
	_, err = io.WriteString(f, text)
	return err
}

// follow reads the clipboard each time a program on the desktop takes it.
func (c *clipboard) follow(owner <-chan *dbus.Signal) {
	defer close(c.updates)
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
		var text string
		ok := false
		if mime := pickText(mimeList(opts["mime_types"].Value())); mime != "" {
			t, err := c.read(mime)
			text, ok = t, err == nil
		}
		c.mu.Lock()
		c.held, c.heldOK, c.known = text, ok, true
		c.mu.Unlock()
		if !ok {
			continue
		}
		select {
		case <-c.updates:
		default:
		}
		c.updates <- text
	}
}

func (c *clipboard) read(mime string) (string, error) {
	var fd dbus.UnixFD
	if err := c.s.p.obj.Call(ifaceClipboard+".SelectionRead", 0, c.s.handle, mime).Store(&fd); err != nil {
		return "", err
	}
	f, err := pollable(fd, "selection-read")
	if err != nil {
		return "", err
	}
	defer f.Close()
	f.SetReadDeadline(time.Now().Add(clipboardTimeout))
	b, err := io.ReadAll(io.LimitReader(f, maxClipboard+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxClipboard {
		return "", errors.New("clipboard too large")
	}
	return string(b), nil
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
