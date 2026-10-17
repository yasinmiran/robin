//go:build linux

package x11

import (
	"fmt"
	"sync"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/damage"
	"github.com/jezek/xgb/shm"
	"github.com/jezek/xgb/xproto"
	"golang.org/x/sys/unix"

	"github.com/yasinmiran/remotejump/internal/agent/desktop"
)

// Frame is a captured picture; see desktop.Frame.
type Frame = desktop.Frame

// capturer holds the shared memory segment the server copies frames into.
type capturer struct {
	mu    sync.Mutex
	x     *xgb.Conn
	root  xproto.Window
	noSHM bool // MIT-SHM is unavailable or failed; use plain GetImage
	seg   *segment
	frame Frame
}

type segment struct {
	id   shm.Seg
	data []byte
}

func (cp *capturer) init(x *xgb.Conn, root xproto.Window) {
	cp.x, cp.root = x, root
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
		return &cp.frame, false, nil
	}
	w, h := c.Size()
	// Subtract before copying: anything drawn during the copy then raises a
	// fresh DAMAGE event and the next Capture picks it up.
	damage.Subtract(c.x, c.damage, 0, 0)
	if err := cp.grab(w, h); err != nil {
		c.dirty.Store(true)
		c.sizeStale.Store(true) // the usual cause is a resize we have not seen yet
		return nil, false, fmt.Errorf("x11: capture %dx%d: %w", w, h, err)
	}
	return &cp.frame, true, nil
}

func (cp *capturer) grab(w, h int) error {
	size := w * h * 4
	if !cp.noSHM {
		if err := cp.ensureSegment(size); err != nil {
			cp.noSHM = true
		}
	}
	if cp.noSHM {
		r, err := xproto.GetImage(cp.x, xproto.ImageFormatZPixmap, xproto.Drawable(cp.root),
			0, 0, uint16(w), uint16(h), ^uint32(0)).Reply()
		if err != nil {
			return err
		}
		cp.frame = Frame{Width: w, Height: h, Stride: w * 4, Pix: r.Data}
		return nil
	}
	_, err := shm.GetImage(cp.x, xproto.Drawable(cp.root), 0, 0, uint16(w), uint16(h),
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
