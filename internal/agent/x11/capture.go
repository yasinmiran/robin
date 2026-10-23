//go:build linux

package x11

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/damage"
	"github.com/jezek/xgb/shm"
	"github.com/jezek/xgb/xproto"
	"golang.org/x/sys/unix"

	"github.com/yasinmiran/robin/internal/agent/desktop"
)

// Frame is a captured picture; see desktop.Frame.
type Frame = desktop.Frame

// damageCheck is how long a screen DAMAGE calls idle goes before Capture
// grabs it anyway. Some compositors draw in ways DAMAGE on the root window
// never hears about; this is how such a screen is noticed.
const damageCheck = time.Second

// capturer holds the shared memory segment the server copies frames into.
type capturer struct {
	mu    sync.Mutex
	x     *xgb.Conn
	root  xproto.Window
	noSHM bool // MIT-SHM is unavailable or failed; use plain GetImage
	seg   *segment
	frame Frame

	grabbed    time.Time // when frame was taken
	checkEvery time.Duration
	sum        uint64 // hash of frame, once summed is set
	summed     bool
	// polling means DAMAGE was caught missing a change: every Capture then
	// grabs the screen and compares it with the last frame.
	polling bool
}

type segment struct {
	id   shm.Seg
	data []byte
}

func (cp *capturer) init(x *xgb.Conn, root xproto.Window) {
	cp.x, cp.root = x, root
	cp.checkEvery = damageCheck
	if err := shm.Init(x); err != nil {
		cp.noSHM = true
		return
	}
	if _, err := shm.QueryVersion(x).Reply(); err != nil {
		cp.noSHM = true
	}
}

// Capture grabs the screen. Unless force is set, it skips the copy and
// returns changed=false when nothing was drawn since the previous capture;
// the returned frame is then the previous one, or nil if there is none.
//
// The Frame and its Pix are reused: they are valid until the next Capture or
// Close, so convert or copy the pixels before capturing again.
func (c *Conn) Capture(force bool) (f *Frame, changed bool, err error) {
	cp := &c.cap
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if !c.dirty.Swap(false) && !force {
		if cp.frame.Pix == nil {
			return nil, false, nil
		}
		if !cp.polling && time.Since(cp.grabbed) < cp.checkEvery {
			cp.hash() // the screen just went idle: remember what it shows
			return &cp.frame, false, nil
		}
		return c.recheck()
	}
	if err := c.grabScreen(); err != nil {
		return nil, false, err
	}
	if cp.polling {
		cp.hash()
	}
	return &cp.frame, true, nil
}

// recheck grabs a screen DAMAGE calls idle and reports whether it changed
// all the same. The first change it finds stops Capture trusting DAMAGE.
func (c *Conn) recheck() (*Frame, bool, error) {
	cp := &c.cap
	cp.hash()
	before := cp.sum
	if err := c.grabScreen(); err != nil {
		return nil, false, err
	}
	cp.hash()
	if cp.sum == before {
		return &cp.frame, false, nil
	}
	if !cp.polling {
		cp.polling = true
		slog.Warn("x11: DAMAGE missed a change on screen; comparing every frame from now on")
	}
	return &cp.frame, true, nil
}

func (c *Conn) grabScreen() error {
	cp := &c.cap
	r := c.viewport()
	// Subtract before copying: anything drawn during the copy then raises a
	// fresh DAMAGE event and the next Capture picks it up.
	damage.Subtract(c.x, c.damage, 0, 0)
	if err := cp.grab(r); err != nil {
		c.dirty.Store(true)
		c.sizeStale.Store(true) // the usual cause is a resize we have not seen yet
		return fmt.Errorf("x11: capture %dx%d+%d+%d: %w", r.W, r.H, r.X, r.Y, err)
	}
	cp.grabbed, cp.summed = time.Now(), false
	return nil
}

// hash sums the current frame unless that is already done.
func (cp *capturer) hash() {
	if !cp.summed {
		cp.sum, cp.summed = pixelHash(cp.frame.Pix[:cp.frame.Stride*cp.frame.Height]), true
	}
}

// grab copies rectangle r of the root into the frame.
func (cp *capturer) grab(r Rect) error {
	w, h := r.W, r.H
	size := w * h * 4
	if !cp.noSHM {
		if err := cp.ensureSegment(size); err != nil {
			cp.noSHM = true
		}
	}
	if cp.noSHM {
		img, err := xproto.GetImage(cp.x, xproto.ImageFormatZPixmap, xproto.Drawable(cp.root),
			int16(r.X), int16(r.Y), uint16(w), uint16(h), ^uint32(0)).Reply()
		if err != nil {
			return err
		}
		cp.frame = Frame{Width: w, Height: h, Stride: w * 4, Pix: img.Data}
		return nil
	}
	_, err := shm.GetImage(cp.x, xproto.Drawable(cp.root), int16(r.X), int16(r.Y), uint16(w), uint16(h),
		^uint32(0), xproto.ImageFormatZPixmap, cp.seg.id, 0).Reply()
	if err != nil {
		return err
	}
	cp.frame = Frame{Width: w, Height: h, Stride: w * 4, Pix: cp.seg.data[:size]}
	return nil
}

// ensureSegment makes sure the segment holds at least size bytes. A new
// segment is marked for removal as soon as both sides are attached, so the
// kernel frees it when we and the server let go, however we exit.
func (cp *capturer) ensureSegment(size int) error {
	if cp.seg != nil && len(cp.seg.data) >= size {
		return nil
	}
	cp.release()
	id, err := unix.SysvShmGet(unix.IPC_PRIVATE, size, unix.IPC_CREAT|0o600)
	if err != nil {
		return err
	}
	defer unix.SysvShmCtl(id, unix.IPC_RMID, nil)
	data, err := unix.SysvShmAttach(id, 0, 0)
	if err != nil {
		return err
	}
	seg, err := shm.NewSegId(cp.x)
	if err == nil {
		err = shm.AttachChecked(cp.x, seg, uint32(id), false).Check()
	}
	if err != nil {
		unix.SysvShmDetach(data)
		return err
	}
	cp.seg = &segment{id: seg, data: data}
	return nil
}

func (cp *capturer) release() {
	if cp.seg == nil {
		return
	}
	shm.Detach(cp.x, cp.seg.id)
	unix.SysvShmDetach(cp.seg.data)
	cp.seg = nil
	cp.frame = Frame{}
}

func (cp *capturer) close() {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.release()
}

// pixelHash sums the colour bytes of BGRX pixels. The fourth byte of each
// pixel is padding the server leaves undefined, so it is masked out: a
// driver that writes noise there must not look like a screen that changes.
func pixelHash(pix []byte) uint64 {
	const colour = 0x00ffffff00ffffff
	h := uint64(14695981039346656037)
	i := 0
	for ; i+8 <= len(pix); i += 8 {
		h = (h ^ binary.LittleEndian.Uint64(pix[i:])&colour) * 1099511628211
	}
	for ; i < len(pix); i++ {
		if i%4 != 3 {
			h = (h ^ uint64(pix[i])) * 1099511628211
		}
	}
	return h
}
