//go:build linux

package wayland

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yasinmiran/robin/internal/agent/keysym"
)

const (
	// pasteSettle is how long Type waits after Ctrl+V, so that input after
	// it reaches the program after the pasted text does.
	pasteSettle = 100 * time.Millisecond
	// restoreAfter is how long the pasted text stays on the clipboard
	// before the text it replaced goes back. A program reads the clipboard
	// when Ctrl+V reaches it, and GNOME's own clipboard manager reads each
	// new offer at once, so a restore cannot wait for "the" read.
	restoreAfter = time.Second
	// keyV is the keysym of a lowercase v, what Ctrl+V pastes with.
	keyV = 'v'
)

// Type types text. Text in US ASCII goes as keysyms, as key-downs of those
// characters do; any other text is pasted whole. The compositor drops a
// keysym its layout lacks, a Wayland client cannot add one, and the portal
// does not say what the layout has, but every Latin layout has ASCII. A
// paste offers the text as the clipboard and presses Ctrl+V; a second
// later the clipboard gets back the user's text, unless something has been
// copied since, or empty text where the agent knows of none, so the typed
// text does not stay there. Without the clipboard, everything goes as
// keysyms and only what the layout has is typed.
func (d *Desktop) Type(ctx context.Context, text string) error {
	if d.in.viewOnly {
		return errViewOnly
	}
	if d.clip != nil && !ascii(text) {
		if err := ctx.Err(); err != nil {
			return err
		}
		return d.paste(ctx, printable(text))
	}
	for _, r := range text {
		if err := ctx.Err(); err != nil {
			return err
		}
		if sym, ok := keysym.Text(r); ok {
			if err := d.in.tap(sym); err != nil {
				return err
			}
		}
	}
	return nil
}

func ascii(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// printable drops the characters that type nothing, as typing would.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if _, ok := keysym.Text(r); !ok {
			return -1
		}
		return r
	}, s)
}

func (d *Desktop) paste(ctx context.Context, text string) error {
	if err := d.clip.offerPaste(text); err != nil {
		return err
	}
	d.clip.armPutBack()
	if err := d.in.ctrl(keyV); err != nil {
		return err
	}
	t := time.NewTimer(pasteSettle)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// pasteRestore is what a run of pastes is to put back on the clipboard.
type pasteRestore struct {
	mu sync.Mutex
	// gen counts pastes and forgets; a put-back armed for an older one
	// does nothing.
	gen   uint64
	run   bool   // pastes have replaced the clipboard
	ok    bool   // saved is the user's text
	saved string // what the clipboard held before the first of them
	timer *time.Timer
}

// forget drops what the pastes were to put back.
func (p *pasteRestore) forget() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gen++
	p.run = false
	if p.timer != nil {
		p.timer.Stop()
	}
}

// offerPaste offers text for a paste, keeping the user's clipboard from
// before the first of a run of pastes. Before the first copy the agent
// sees, that is whatever was there when the session began, which the
// portal does not report, so it is read once.
func (c *clipboard) offerPaste(text string) error {
	p := &c.paste
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gen++
	if p.timer != nil {
		p.timer.Stop()
	}
	if !p.run {
		c.mu.Lock()
		known := c.known
		c.mu.Unlock()
		if !known {
			before, err := c.read(textMimes[0])
			c.mu.Lock()
			if !c.known {
				c.held, c.heldOK, c.known = before, err == nil, true
			}
			c.mu.Unlock()
		}
		c.mu.Lock()
		p.run, p.saved, p.ok = true, c.held, c.heldOK
		c.mu.Unlock()
	}
	return c.offer(text)
}

// armPutBack puts back what the clipboard held, restoreAfter from now,
// unless another paste or a copy comes first.
func (c *clipboard) armPutBack() {
	p := &c.paste
	p.mu.Lock()
	defer p.mu.Unlock()
	gen := p.gen
	p.timer = time.AfterFunc(restoreAfter, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.gen != gen {
			return
		}
		if err := c.putBackLocked(); err != nil {
			c.log.Warn("put the clipboard back after a paste", "err", err)
		}
	})
}

// putBack puts back at once what pastes replaced, as the session ends.
func (c *clipboard) putBack() error {
	p := &c.paste
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.timer != nil {
		p.timer.Stop()
	}
	return c.putBackLocked()
}

// putBackLocked offers the user's text again, or empty text when there is
// none to put back. p.mu must be held, which also keeps the next paste's
// offer from landing before this one.
func (c *clipboard) putBackLocked() error {
	p := &c.paste
	p.gen++
	if !p.run {
		return nil
	}
	p.run = false
	if !p.ok {
		return c.offer("")
	}
	return c.offer(p.saved)
}

// tap presses and releases sym.
func (in *input) tap(sym uint32) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if err := in.keysym(sym, true); err != nil {
		return err
	}
	return in.keysym(sym, false)
}

// ctrl taps sym with Control held.
func (in *input) ctrl(sym uint32) error {
	control, _ := keysym.Named("Control", "ControlLeft")
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, k := range []struct {
		sym  uint32
		down bool
	}{{control, true}, {sym, true}, {sym, false}, {control, false}} {
		if err := in.keysym(k.sym, k.down); err != nil {
			return err
		}
	}
	return nil
}
