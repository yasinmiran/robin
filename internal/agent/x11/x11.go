//go:build linux

// Package x11 drives one X display over a single connection: it captures the
// screen (MIT-SHM, with DAMAGE to skip idle frames), injects input (XTEST),
// resizes the screen (RandR) and syncs the CLIPBOARD selection (XFIXES).
//
// A Conn's methods are safe for concurrent use. One goroutine owns the
// event queue; it only sets flags and runs the selection protocol.
package x11

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/damage"
	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/xfixes"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
)

// Conn is a connection to one X display.
type Conn struct {
	x      *xgb.Conn
	root   xproto.Window
	maxReq int // largest request body in bytes, a multiple of 4

	sizeMu    sync.Mutex
	w, h      int
	sizeStale atomic.Bool // the screen changed under us; re-read on next use

	damage damage.Damage
	dirty  atomic.Bool
	cap    capturer

	resizeMu sync.Mutex
	scr      screenRes

	kb     keyboard
	clip   clipboard
	cursor cursorTracker

	loopDone  chan struct{}
	closeOnce sync.Once
}

// Open connects to display (for example ":7") with the MIT-MAGIC-COOKIE-1
// from the xauthority file. An empty xauthority falls back to $XAUTHORITY.
func Open(display, xauthority string) (*Conn, error) {
	x, err := dial(display, xauthority)
	if err != nil {
		return nil, fmt.Errorf("x11: connect %s: %w", display, err)
	}
	c := &Conn{x: x, loopDone: make(chan struct{})}
	if err := c.init(); err != nil {
		x.Close()
		return nil, fmt.Errorf("x11: %s: %w", display, err)
	}
	go c.loop()
	return c, nil
}

func (c *Conn) init() error {
	setup := xproto.Setup(c.x)
	screen := setup.DefaultScreen(c.x)
	c.root = screen.Root
	c.maxReq = (int(setup.MaximumRequestLength)*4 - 32) &^ 3
	if err := checkPixmapFormat(setup, screen); err != nil {
		return err
	}
	if err := initExtensions(c.x); err != nil {
		return err
	}

	var err error
	if c.damage, err = damage.NewDamageId(c.x); err != nil {
		return fmt.Errorf("DAMAGE: %w", err)
	}
	if err := damage.CreateChecked(c.x, c.damage, xproto.Drawable(c.root), damage.ReportLevelNonEmpty).Check(); err != nil {
		return fmt.Errorf("DAMAGE: %w", err)
	}
	c.dirty.Store(true)

	if err := c.scr.init(c.x, c.root); err != nil {
		return err
	}
	geo, err := xproto.GetGeometry(c.x, xproto.Drawable(c.root)).Reply()
	if err != nil {
		return err
	}
	c.w, c.h = int(geo.Width), int(geo.Height)

	if err := c.kb.init(c.x, setup); err != nil {
		return err
	}
	if err := c.clip.init(c.x, c.root, c.maxReq, c.loopDone); err != nil {
		return fmt.Errorf("clipboard: %w", err)
	}
	c.cap.init(c.x, c.root)
	if err := c.cursor.init(c.x, c.root); err != nil {
		return fmt.Errorf("cursor: %w", err)
	}
	return nil
}

func initExtensions(x *xgb.Conn) error {
	if err := xfixes.Init(x); err != nil {
		return fmt.Errorf("XFIXES: %w", err)
	}
	if _, err := xfixes.QueryVersion(x, 5, 0).Reply(); err != nil {
		return fmt.Errorf("XFIXES: %w", err)
	}
	if err := damage.Init(x); err != nil {
		return fmt.Errorf("DAMAGE: %w", err)
	}
	if _, err := damage.QueryVersion(x, 1, 1).Reply(); err != nil {
		return fmt.Errorf("DAMAGE: %w", err)
	}
	if err := xtest.Init(x); err != nil {
		return fmt.Errorf("XTEST: %w", err)
	}
	if _, err := xtest.GetVersion(x, 2, 2).Reply(); err != nil {
		return fmt.Errorf("XTEST: %w", err)
	}
	if err := randr.Init(x); err != nil {
		return fmt.Errorf("RandR: %w", err)
	}
	if _, err := randr.QueryVersion(x, 1, 3).Reply(); err != nil {
		return fmt.Errorf("RandR: %w", err)
	}
	return nil
}

// checkPixmapFormat makes sure captures come back as 32-bit little-endian
// BGRX, the only layout Frame describes.
func checkPixmapFormat(setup *xproto.SetupInfo, screen *xproto.ScreenInfo) error {
	if screen.RootDepth != 24 && screen.RootDepth != 32 {
		return fmt.Errorf("unsupported root depth %d", screen.RootDepth)
	}
	if setup.ImageByteOrder != xproto.ImageOrderLSBFirst {
		return errors.New("unsupported big-endian image byte order")
	}
	for _, f := range setup.PixmapFormats {
		if f.Depth == screen.RootDepth {
			if f.BitsPerPixel != 32 {
				return fmt.Errorf("unsupported %d bits per pixel", f.BitsPerPixel)
			}
			return nil
		}
	}
	return fmt.Errorf("no pixmap format for depth %d", screen.RootDepth)
}

// Size returns the current screen size in pixels.
func (c *Conn) Size() (w, h int) {
	c.sizeMu.Lock()
	defer c.sizeMu.Unlock()
	if c.sizeStale.Swap(false) {
		if geo, err := xproto.GetGeometry(c.x, xproto.Drawable(c.root)).Reply(); err == nil {
			c.w, c.h = int(geo.Width), int(geo.Height)
		}
	}
	return c.w, c.h
}

func (c *Conn) setSize(w, h int) {
	c.sizeMu.Lock()
	c.w, c.h = w, h
	c.sizeMu.Unlock()
}

// Close releases every key and button still held, puts back the keymap
// entries and autorepeat it changed, and disconnects. It is safe to call
// more than once.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		c.kb.close()
		c.x.Close()
		<-c.loopDone
		c.cap.close()
	})
	return nil
}

// loop owns the event queue until the connection closes.
func (c *Conn) loop() {
	defer close(c.loopDone)
	defer c.clip.closeUpdates()
	defer close(c.cursor.updates)
	for {
		ev, xerr := c.x.WaitForEvent()
		if ev == nil && xerr == nil {
			return
		}
		if xerr != nil {
			continue // a failed unchecked request; its caller has moved on
		}
		switch e := ev.(type) {
		case damage.NotifyEvent:
			c.dirty.Store(true)
		case randr.ScreenChangeNotifyEvent:
			c.sizeStale.Store(true)
			c.dirty.Store(true)
		case xproto.MappingNotifyEvent:
			if e.Request != xproto.MappingPointer {
				c.kb.stale.Store(true)
			}
		case xproto.PropertyNotifyEvent:
			c.clip.onProperty(e)
		case xproto.SelectionRequestEvent:
			c.clip.onRequest(e)
		case xproto.SelectionClearEvent:
			c.clip.onClear(e)
		case xproto.SelectionNotifyEvent:
			c.clip.onNotify(e)
		case xfixes.SelectionNotifyEvent:
			c.clip.onOwner(e)
		case xfixes.CursorNotifyEvent:
			c.cursor.onNotify(e)
		}
	}
}

// dial connects to the local display socket and authenticates with the
// cookie from xauthority, without touching the process environment.
func dial(display, xauthority string) (*xgb.Conn, error) {
	if xauthority == "" {
		return xgb.NewConnDisplay(display)
	}
	num, err := displayNumber(display)
	if err != nil {
		return nil, err
	}
	cookie, err := readCookie(xauthority, num)
	if err != nil {
		return nil, err
	}
	sock := "/tmp/.X11-unix/X" + num
	nc, err := net.Dial("unix", sock)
	if err != nil {
		// Servers that cannot write /tmp/.X11-unix still listen on the
		// abstract socket of the same name.
		var aerr error
		if nc, aerr = net.Dial("unix", "@"+sock); aerr != nil {
			return nil, err
		}
	}
	x, err := xgb.NewConnNetWithCookieHex(nc, hex.EncodeToString(cookie))
	if err != nil {
		nc.Close()
		return nil, err
	}
	return x, nil
}

// displayNumber extracts "7" from ":7", ":7.0" or "unix:7".
func displayNumber(display string) (string, error) {
	host, rest, ok := strings.Cut(display, ":")
	if !ok || (host != "" && host != "unix") {
		return "", fmt.Errorf("unsupported display %q: only local displays", display)
	}
	num, _, _ := strings.Cut(rest, ".")
	if num == "" || strings.Trim(num, "0123456789") != "" {
		return "", fmt.Errorf("bad display %q", display)
	}
	return num, nil
}

// readCookie finds the MIT-MAGIC-COOKIE-1 for display number num, matching
// entries the way libXau does for a local connection.
func readCookie(path, num string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	r := bytes.NewReader(data)
	for {
		var family uint16
		if err := binary.Read(r, binary.BigEndian, &family); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("%s: no cookie for display %s", path, num)
			}
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		var f [4][]byte
		for i := range f {
			var n uint16
			if err := binary.Read(r, binary.BigEndian, &n); err != nil {
				return nil, fmt.Errorf("%s: truncated", path)
			}
			f[i] = make([]byte, n)
			if _, err := io.ReadFull(r, f[i]); err != nil {
				return nil, fmt.Errorf("%s: truncated", path)
			}
		}
		addr, dnum, name, cookie := string(f[0]), string(f[1]), string(f[2]), f[3]
		const familyLocal, familyWild = 256, 65535
		hostOK := family == familyWild || (family == familyLocal && addr == host)
		if hostOK && (dnum == "" || dnum == num) && name == "MIT-MAGIC-COOKIE-1" && len(cookie) == 16 {
			return cookie, nil
		}
	}
}
