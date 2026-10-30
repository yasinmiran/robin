//go:build linux

// Package wayland streams and drives the user's running Wayland desktop
// through xdg-desktop-portal: RemoteDesktop for input, ScreenCast for the
// picture and Clipboard for the clipboard. Frames come from PipeWire through
// a gst-launch-1.0 subprocess, so the agent links no GStreamer or PipeWire
// library.
//
// The first session needs someone at the host's screen to approve it. The
// portal then hands out a restore token, kept in a file only the user can
// read, that lets later sessions start without asking until the grant is
// revoked on the host.
package wayland

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/yasinmiran/robin/internal/agent/desktop"
	"github.com/yasinmiran/robin/internal/protocol"
)

// DefaultConsentWait is how long Open waits for someone to answer the
// consent dialog when Options.ConsentWait is zero.
const DefaultConsentWait = 2 * time.Minute

// restoreGrace is how long a restore token gets to answer before the user is
// told a dialog may be up after all, as when the grant was revoked.
const restoreGrace = 2 * time.Second

// Options configure Open.
type Options struct {
	// TokenFile keeps the portal's restore token between sessions. Empty
	// asks for consent every time.
	TokenFile string
	// MaxWidth and MaxHeight bound the frame size; a larger screen is scaled
	// down to fit, keeping its aspect ratio.
	MaxWidth, MaxHeight int
	FPS                 int
	// ConsentWait bounds the wait for an answer to the consent dialog.
	ConsentWait time.Duration
	// Notify hears, before Open returns, what the user has to do or know:
	// that a dialog waits on the host's screen, or that only viewing was
	// allowed.
	Notify func(protocol.Notice)
	Log    *slog.Logger
}

// Desktop is the user's Wayland session as one portal session grants it.
type Desktop struct {
	conn   *dbus.Conn
	sess   *portalSession
	frames *frames
	in     *input
	clip   *clipboard
	cursor chan protocol.Cursor
	done   chan error
	// anyKeysym says the compositor types a keysym its layout lacks, as
	// KWin does, so text needs no paste.
	anyKeysym bool
	// kb follows the layout under mutter, which types only what that
	// layout has; nil elsewhere, and under a view-only grant.
	kb *keyboard

	closing   chan struct{}
	closeOnce sync.Once
}

var (
	_ desktop.Desktop  = (*Desktop)(nil)
	_ desktop.ViewOnly = (*Desktop)(nil)
)

// Open asks the portal for the screen, keyboard, pointer and clipboard and
// starts reading frames. ctx bounds the opening only, consent included.
func Open(ctx context.Context, o Options) (_ *Desktop, err error) {
	if o.ConsentWait <= 0 {
		o.ConsentWait = DefaultConsentWait
	}
	if o.FPS <= 0 {
		o.FPS = 30
	}
	if o.Notify == nil {
		o.Notify = func(protocol.Notice) {}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	conn, err := sessionBus()
	if err != nil {
		return nil, err
	}
	d := &Desktop{conn: conn, done: make(chan error, 1), closing: make(chan struct{})}
	defer func() {
		if err != nil {
			d.Close()
		}
	}()
	p, err := newPortal(conn, newSignals(conn))
	if err != nil {
		return nil, err
	}
	if d.sess, err = grant(ctx, p, o); err != nil {
		return nil, err
	}
	o.Log.Info("portal session", "node", d.sess.node, "size", fmt.Sprintf("%dx%d", d.sess.width, d.sess.height),
		"devices", d.sess.devices, "clipboard", d.sess.clipboard, "cursor", p.cursorMode)

	w, h := fit(d.sess.width, d.sess.height, o.MaxWidth, o.MaxHeight)
	if d.frames, err = startFrames(d.sess.remote, d.sess.node, w, h, o.FPS); err != nil {
		return nil, err
	}
	// gst holds its own copy of the PipeWire connection now.
	d.sess.remote.Close()
	d.sess.remote = nil

	d.in = newInput(d.sess, d.frames.width, d.frames.height)
	if err := d.in.warmUp(); err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}
	switch sessionCompositor(ctx, conn.BusObject()) {
	case compositorKWin:
		d.anyKeysym = true
	case compositorMutter:
		if !d.in.viewOnly {
			d.kb = followKeyboard(p.sigs, o.Log)
			d.in.kb = d.kb
		}
	}
	if d.sess.clipboard {
		if d.clip, err = newClipboard(d.sess, o.Log); err != nil {
			return nil, err
		}
	}
	if p.cursorMode == cursorEmbedded {
		// The pointer is in the picture, so the page must not draw its own.
		d.cursor = make(chan protocol.Cursor, 1)
		d.cursor <- protocol.Cursor{ID: 1, Hidden: true}
	}
	go d.watch()
	return d, nil
}

// grant gets a portal session, from the stored token when there is one.
// A token is spent by any Start, so it is replaced or removed every time.
func grant(ctx context.Context, p *portal, o Options) (*portalSession, error) {
	c := consent{wait: o.ConsentWait, notify: o.Notify}
	token := readToken(o.TokenFile)
	for {
		s, err := p.start(ctx, token, c)
		if err != nil {
			removeToken(o.TokenFile)
			// A token can fail where a fresh request would not, as when the
			// monitor it names is gone. Declining and timeouts are answers.
			if token != "" && ctx.Err() == nil && !errors.Is(err, errDeclined) && !errors.Is(err, errNoAnswer) {
				o.Log.Warn("restoring the screen sharing grant failed; asking again", "err", err)
				token = ""
				continue
			}
			return nil, err
		}
		if !s.viewOnly() {
			if err := saveToken(o.TokenFile, s.token); err != nil {
				o.Log.Warn("cannot keep the restore token; the next session asks again", "err", err)
			}
			return s, nil
		}
		// A view-only token would make every later session view-only without
		// asking, so it is never kept.
		removeToken(o.TokenFile)
		if token != "" {
			s.close()
			token = ""
			continue
		}
		o.Notify(protocol.Notice{Kind: protocol.NoticeViewOnly})
		return s, nil
	}
}

// compositor is the session's compositor, as far as typing tells them
// apart.
type compositor int

const (
	compositorOther compositor = iota
	// compositorMutter types a keysym only if its layout has it, and
	// takes the layout GNOME Shell applies from GNOME's input sources.
	compositorMutter
	// compositorKWin types any keysym, its layout's or not.
	compositorKWin
)

// compositorNames are the bus names the compositors own, by which the
// agent tells them apart. Measured on KDE Plasma 6.7, GNOME 46 and GNOME 49.
var compositorNames = []struct {
	name string
	c    compositor
}{
	{"org.kde.KWin", compositorKWin},
	{"org.gnome.Mutter.RemoteDesktop", compositorMutter},
}

// sessionCompositor names the compositor by the names it owns on bus, the
// session bus's own object.
func sessionCompositor(ctx context.Context, bus dbus.BusObject) compositor {
	for _, n := range compositorNames {
		var owned bool
		if err := bus.CallWithContext(ctx, "org.freedesktop.DBus.NameHasOwner", 0, n.name).Store(&owned); err == nil && owned {
			return n.c
		}
	}
	return compositorOther
}

// watch reports the desktop gone when the portal ends the session, as the
// host's "stop sharing" button does, or when the frame reader dies.
func (d *Desktop) watch() {
	stopped := func(ok bool) error {
		if !ok {
			return errors.New("lost the desktop session's bus")
		}
		return errors.New("screen sharing was stopped on the host")
	}
	var err error
	select {
	case <-d.closing:
		return
	case _, ok := <-d.sess.closed:
		err = stopped(ok)
	case <-d.frames.exited:
		err = d.frames.reason()
		// A share stopped on the host takes the stream down too, and the
		// portal's Closed, which says why, can come just after.
		select {
		case _, ok := <-d.sess.closed:
			err = stopped(ok)
		case <-time.After(time.Second):
		case <-d.closing:
			return
		}
	}
	select {
	case <-d.closing:
	case d.done <- err:
	}
}

// Done fires once if the desktop goes away underneath the session.
func (d *Desktop) Done() <-chan error { return d.done }

// Size returns the frame size, which is also the space Input positions are in.
func (d *Desktop) Size() (w, h int) { return d.frames.width, d.frames.height }

// Resize reports the current size: the screen belongs to the user.
func (d *Desktop) Resize(int, int) (int, int, error) {
	w, h := d.Size()
	return w, h, desktop.ErrFixedSize
}

// Capture returns the newest frame, in I420.
func (d *Desktop) Capture(bool) (*desktop.Frame, bool, error) { return d.frames.capture() }

// Input applies a key, pointer, wheel or release message.
func (d *Desktop) Input(m protocol.Control) error { return d.in.apply(m) }

// ViewOnly reports whether the host granted the screen without keyboard
// and pointer.
func (d *Desktop) ViewOnly() bool { return d.sess.viewOnly() }

// SetClipboard offers text as the desktop's clipboard.
func (d *Desktop) SetClipboard(text string) error {
	return d.SetClipboardContent(protocol.Clip{Text: text})
}

// SetClipboardContent offers c as the desktop's clipboard, in the MIME
// types of each format it holds.
func (d *Desktop) SetClipboardContent(c protocol.Clip) error {
	if d.clip == nil {
		return errors.New("the host did not allow clipboard access")
	}
	return d.clip.set(c)
}

// Clipboard delivers the text of what is copied on the desktop; nil when
// the host did not allow clipboard access.
func (d *Desktop) Clipboard() <-chan string {
	if d.clip == nil {
		return nil
	}
	return d.clip.updates
}

// ClipboardContent delivers what is copied on the desktop, as text, HTML
// and a PNG image as far as the program copying offers them; nil when the
// host did not allow clipboard access.
func (d *Desktop) ClipboardContent() <-chan protocol.Clip {
	if d.clip == nil {
		return nil
	}
	return d.clip.contents
}

// Cursor reports once that the pointer is drawn into the frames, so the page
// hides its own; nil when the portal cannot draw it.
func (d *Desktop) Cursor() <-chan protocol.Cursor { return d.cursor }

// Close lets go of held keys and buttons, stops the stream and ends the
// portal session. The user's session carries on as it was.
func (d *Desktop) Close() error {
	d.closeOnce.Do(func() {
		close(d.closing)
		if d.in != nil {
			d.in.close()
		}
		if d.frames != nil {
			d.frames.stop()
		}
		if d.clip != nil {
			if err := d.clip.putBack(); err != nil {
				d.clip.log.Warn("put the clipboard back", "err", err)
			}
			d.clip.close()
		}
		if d.kb != nil {
			d.kb.close()
		}
		if d.sess != nil {
			d.sess.close()
		}
		d.conn.Close()
	})
	return nil
}

// fit scales w x h down to fit inside maxW x maxH, keeping the aspect ratio,
// and rounds to even dimensions. Unknown sizes stay unknown.
func fit(w, h, maxW, maxH int) (int, int) {
	if w <= 0 || h <= 0 {
		return 0, 0
	}
	if maxW > 0 && maxH > 0 && (w > maxW || h > maxH) {
		scale := min(float64(maxW)/float64(w), float64(maxH)/float64(h))
		w, h = int(float64(w)*scale), int(float64(h)*scale)
	}
	return max(w&^1, 2), max(h&^1, 2)
}
