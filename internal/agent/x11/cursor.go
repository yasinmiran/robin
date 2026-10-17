//go:build linux

package x11

import (
	"bytes"
	"image"
	"image/png"
	"sync"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xfixes"
	"github.com/jezek/xgb/xproto"

	"github.com/yasinmiran/remotejump/internal/protocol"
)

// maxCursorSide bounds the images forwarded; X cursors are rarely over 64 px
// and a hostile client cannot make the agent ship megapixel PNGs.
const maxCursorSide = 256

// cursorTracker follows the pointer image through XFIXES. Each distinct image
// is encoded once; switching back to it later sends only its serial.
type cursorTracker struct {
	x       *xgb.Conn
	updates chan protocol.Cursor

	mu sync.Mutex
	// sent holds the last maxSentCursors serials published with an image;
	// X mints a serial per cursor object, so it must not grow forever. An
	// evicted serial is simply encoded again.
	sent  map[uint32]bool
	order []uint32
}

const maxSentCursors = 64

func (t *cursorTracker) init(x *xgb.Conn, root xproto.Window) error {
	t.x = x
	t.updates = make(chan protocol.Cursor, 1)
	t.sent = map[uint32]bool{}
	if err := xfixes.SelectCursorInputChecked(x, root, xfixes.CursorNotifyMaskDisplayCursor).Check(); err != nil {
		return err
	}
	t.refresh(0)
	return nil
}

func (t *cursorTracker) onNotify(e xfixes.CursorNotifyEvent) { t.refresh(e.CursorSerial) }

// refresh publishes the current cursor. A serial already sent needs no image.
func (t *cursorTracker) refresh(serial uint32) {
	t.mu.Lock()
	known := serial != 0 && t.sent[serial]
	t.mu.Unlock()
	if known {
		t.publish(protocol.Cursor{ID: serial})
		return
	}
	img, err := xfixes.GetCursorImage(t.x).Reply()
	if err != nil || img.Width == 0 || img.Height == 0 || img.Width > maxCursorSide || img.Height > maxCursorSide {
		return
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, cursorImage(int(img.Width), int(img.Height), img.CursorImage)); err != nil {
		return
	}
	t.mu.Lock()
	if !t.sent[img.CursorSerial] {
		t.sent[img.CursorSerial] = true
		t.order = append(t.order, img.CursorSerial)
		if len(t.order) > maxSentCursors {
			delete(t.sent, t.order[0])
			t.order = t.order[1:]
		}
	}
	t.mu.Unlock()
	t.publish(protocol.Cursor{ID: img.CursorSerial, PNG: buf.Bytes(), HotX: int(img.Xhot), HotY: int(img.Yhot)})
}

// publish keeps only the newest update when nobody is reading.
func (t *cursorTracker) publish(c protocol.Cursor) {
	select {
	case <-t.updates:
	default:
	}
	select {
	case t.updates <- c:
	default:
	}
}

// cursorImage converts XFIXES pixels, premultiplied ARGB in native words, to
// straight-alpha RGBA.
func cursorImage(w, h int, argb []uint32) *image.NRGBA {
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i, p := range argb[:min(len(argb), w*h)] {
		a := uint8(p >> 24)
		if a == 0 {
			continue
		}
		un := func(c uint32) uint8 { return uint8(min((c&0xff)*255/uint32(a), 255)) }
		m.Pix[4*i], m.Pix[4*i+1], m.Pix[4*i+2], m.Pix[4*i+3] = un(p>>16), un(p>>8), un(p), a
	}
	return m
}

// Cursor delivers pointer image changes. Only the newest is kept if the reader
// falls behind, and the channel is closed when the connection ends.
func (c *Conn) Cursor() <-chan protocol.Cursor { return c.cursor.updates }
