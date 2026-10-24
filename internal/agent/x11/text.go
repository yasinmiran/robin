//go:build linux

package x11

import (
	"context"
	"time"

	"github.com/jezek/xgb/xproto"

	"github.com/yasinmiran/robin/internal/agent/keysym"
)

// spareSettle is how long a spare keycode keeps its keysym after a press
// before Type binds it to another. A client looks the keycode up when it
// gets to the key event, in the keymap as it is by then, so one still
// behind reads the earlier character as the later one: xterm in Xvfb did
// with no wait, and with 5 ms in 7 runs of 10; 15 ms was enough there.
const spareSettle = 50 * time.Millisecond

// Type types text a character at a time, as key-downs of those characters
// do, with newlines as Return and tabs as Tab. Characters missing from the
// keymap take spare keycodes; once each has been used, the next waits for
// the oldest to settle, without holding the keyboard meanwhile.
func (c *Conn) Type(ctx context.Context, text string) error {
	k := &c.kb
	for _, r := range text {
		if err := ctx.Err(); err != nil {
			return err
		}
		sym, ok := keysym.Text(r)
		if !ok {
			continue
		}
		for {
			wait, err := k.typeSettled(xproto.Keysym(sym))
			if err != nil {
				return err
			}
			if wait <= 0 {
				break
			}
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C:
			}
		}
	}
	return nil
}

// typeSettled types sym, or, when that needs a spare keycode that has not
// settled yet, types nothing and says how long to wait.
func (k *keyboard) typeSettled(sym xproto.Keysym) (time.Duration, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.disableAutorepeat(); err != nil {
		return 0, err
	}
	if err := k.refresh(); err != nil {
		return 0, err
	}
	if _, _, ok := k.lookup(sym); !ok {
		if i := k.nextSpare(); i >= 0 {
			if wait := spareSettle - time.Since(k.spares[i].usedAt); wait > 0 {
				return wait, nil
			}
		}
	}
	return 0, k.typeSym(sym)
}
