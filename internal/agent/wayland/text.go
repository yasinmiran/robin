//go:build linux

package wayland

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yasinmiran/robin/internal/agent/keysym"
	"github.com/yasinmiran/robin/internal/protocol"
)

const (
	// pasteSettle is how long Type waits after the paste keys, so that
	// input after them reaches the program after the pasted text does.
	pasteSettle = 100 * time.Millisecond
	// managerRead bounds the wait for a clipboard manager to read a new
	// offer, as mutter's does at once, before the paste keys go, and
	// managerQuiet is how long reads must stop for before they do: a read
	// after the keys is then the focused program's.
	managerRead  = 250 * time.Millisecond
	managerQuiet = 50 * time.Millisecond
	// pasteRead bounds the wait for the focused program to read the offer
	// after Ctrl+Shift+V, before Ctrl+V goes instead.
	pasteRead = 300 * time.Millisecond
	// restoreAfter is how long the pasted text stays on the clipboard
	// before the text it replaced goes back. A program reads the clipboard
	// when the paste keys reach it, and GNOME's own clipboard manager reads
	// each new offer at once, so a restore cannot wait for "the" read.
	restoreAfter = time.Second
)

// Type types text. Where the compositor types any keysym, as KWin does,
// every character goes as its keysym. Elsewhere the compositor drops a
// keysym its layout lacks and a Wayland client cannot add one, so only
// ASCII the layout has goes as keysyms, and any other text is pasted
// whole.
//
// A paste offers the text as the clipboard and presses Ctrl+Shift+V, which
// terminals paste with; if no program reads the offer, it presses Ctrl+V,
// which everything else pastes with. A second later the clipboard gets
// back what the user had there, unless something has been copied since,
// or empty text where the agent knows of nothing, so the typed text does
// not stay there. Without the clipboard, everything goes as keysyms and
// only what the layout has is typed.
func (d *Desktop) Type(ctx context.Context, text string) error {
	if d.in.viewOnly {
		return errViewOnly
	}
	if d.clip != nil && !d.typable(text) {
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

// typable reports whether text types as keysyms here: any text where the
// compositor types any keysym, and elsewhere ASCII the layout has.
func (d *Desktop) typable(text string) bool {
	if d.anyKeysym {
		return true
	}
	for _, r := range text {
		if r >= utf8.RuneSelf || d.in.lacks != nil && d.in.lacks(r) {
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
	if text == "" {
		return nil
	}
	n := d.clip.reads()
	if err := d.clip.offerPaste(text); err != nil {
		return err
	}
	// From here the clipboard holds the text, so it goes back whatever
	// happens, a cancelled ctx included.
	defer d.clip.armPutBack()
	n, err := d.clip.awaitQuiet(ctx, n, managerRead, managerQuiet)
	if err != nil {
		return err
	}
	if err := d.in.chord('v', keysym.ControlL, keysym.ShiftL); err != nil {
		return err
	}
	read, err := d.clip.awaitRead(ctx, n, pasteRead)
	if err != nil {
		return err
	}
	if !read {
		if err := d.in.chord('v', keysym.ControlL); err != nil {
			return err
		}
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
	run   bool          // pastes have replaced the clipboard
	saved protocol.Clip // what the clipboard held before the first of them
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
// portal does not report, so it is read once, in each format the rich
// clipboard knows; a format the clipboard lacks reads as nothing.
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
			before := c.readAll(richMimes)
			c.mu.Lock()
			if !c.known {
				c.held, c.known = before, true
			}
			c.mu.Unlock()
		}
		c.mu.Lock()
		p.run, p.saved = true, c.held
		c.mu.Unlock()
	}
	return c.offer(protocol.Clip{Text: text})
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

// putBackLocked offers the user's clipboard again, in each format it had,
// or empty text when there is none to put back. p.mu must be held, which
// also keeps the next paste's offer from landing before this one.
func (c *clipboard) putBackLocked() error {
	p := &c.paste
	p.gen++
	if !p.run {
		return nil
	}
	p.run = false
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

// chord taps the key of r with mods, keysyms every layout has, held:
// pressed in order and let go the other way round.
func (in *input) chord(r rune, mods ...uint32) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, m := range mods {
		if err := in.keysym(m, true); err != nil {
			return err
		}
	}
	if err := in.char(r); err != nil {
		return err
	}
	for i := len(mods) - 1; i >= 0; i-- {
		if err := in.keysym(mods[i], false); err != nil {
			return err
		}
	}
	return nil
}
