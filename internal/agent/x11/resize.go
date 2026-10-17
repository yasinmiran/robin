//go:build linux

package x11

import (
	"errors"
	"fmt"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/xproto"
)

// screenRes is what Resize needs to know about the RandR configuration.
type screenRes struct {
	minW, minH, maxW, maxH int
	created                randr.Mode // the mode we made and are showing, or 0
}

func (s *screenRes) init(x *xgb.Conn, root xproto.Window) error {
	rng, err := randr.GetScreenSizeRange(x, root).Reply()
	if err != nil {
		return fmt.Errorf("RandR: %w", err)
	}
	s.minW, s.minH = int(rng.MinWidth), int(rng.MinHeight)
	s.maxW, s.maxH = int(rng.MaxWidth), int(rng.MaxHeight)
	return randr.SelectInputChecked(x, root, randr.NotifyMaskScreenChange).Check()
}

// Resize switches the screen to w x h, clamped to the range the server
// supports and rounded down to even dimensions (H.264 4:2:0 needs them). It
// returns the size it set.
func (c *Conn) Resize(w, h int) (gotW, gotH int, err error) {
	c.resizeMu.Lock()
	defer c.resizeMu.Unlock()
	s := &c.scr
	w = min(max(w, s.minW, 2), s.maxW) &^ 1
	h = min(max(h, s.minH, 2), s.maxH) &^ 1
	curW, curH := c.Size()
	if w == curW && h == curH {
		return w, h, nil
	}
	// Hold off captures so none asks for the old size of a screen that has
	// already shrunk.
	c.cap.mu.Lock()
	defer c.cap.mu.Unlock()
	if err := c.switchMode(w, h, curW, curH); err != nil {
		return curW, curH, fmt.Errorf("x11: resize to %dx%d: %w", w, h, err)
	}
	c.setSize(w, h)
	c.dirty.Store(true)
	return w, h, nil
}

func (c *Conn) switchMode(w, h, curW, curH int) error {
	x, root, s := c.x, c.root, &c.scr
	res, err := randr.GetScreenResourcesCurrent(x, root).Reply()
	if err != nil {
		return err
	}
	out, info, err := firstOutput(x, res)
	if err != nil {
		return err
	}
	mode, made, err := modeFor(x, root, res, info, w, h)
	if err != nil {
		return err
	}
	if made {
		if err := randr.AddOutputModeChecked(x, out, mode).Check(); err != nil {
			randr.DestroyMode(x, mode)
			return fmt.Errorf("add mode: %w", err)
		}
		// AddOutputMode bumps the config timestamp SetCrtcConfig checks.
		if res, err = randr.GetScreenResourcesCurrent(x, root).Reply(); err != nil {
			return err
		}
	}

	// The CRTC must fit inside the screen at every step: grow the screen to
	// cover both sizes, switch the CRTC, then shrink to the target.
	mm := func(px int) uint32 { return uint32(max(px*254/960, 1)) } // at 96 dpi
	bigW, bigH := max(w, curW), max(h, curH)
	if bigW != curW || bigH != curH {
		if err := randr.SetScreenSizeChecked(x, root, uint16(bigW), uint16(bigH), mm(bigW), mm(bigH)).Check(); err != nil {
			return fmt.Errorf("grow screen: %w", err)
		}
	}
	sc, err := randr.SetCrtcConfig(x, info.Crtc, xproto.TimeCurrentTime, res.ConfigTimestamp,
		0, 0, mode, randr.RotationRotate0, []randr.Output{out}).Reply()
	if err != nil {
		return fmt.Errorf("set CRTC: %w", err)
	}
	if sc.Status != randr.SetConfigSuccess {
		return fmt.Errorf("set CRTC: status %d", sc.Status)
	}
	if bigW != w || bigH != h {
		if err := randr.SetScreenSizeChecked(x, root, uint16(w), uint16(h), mm(w), mm(h)).Check(); err != nil {
			return fmt.Errorf("shrink screen: %w", err)
		}
	}

	if s.created != 0 && s.created != mode {
		randr.DeleteOutputMode(x, out, s.created)
		randr.DestroyMode(x, s.created)
		s.created = 0
	}
	if made {
		s.created = mode
	}
	return nil
}

// firstOutput returns the first output driven by a CRTC. Xvfb has exactly one.
func firstOutput(x *xgb.Conn, res *randr.GetScreenResourcesCurrentReply) (randr.Output, *randr.GetOutputInfoReply, error) {
	for _, out := range res.Outputs {
		info, err := randr.GetOutputInfo(x, out, res.ConfigTimestamp).Reply()
		if err != nil {
			return 0, nil, err
		}
		if info.Crtc != 0 {
			return out, info, nil
		}
	}
	return 0, nil, errors.New("no active output")
}

// modeFor finds the output's mode of exactly w x h, or creates one; made
// reports a new mode that still has to be added to the output.
func modeFor(x *xgb.Conn, root xproto.Window, res *randr.GetScreenResourcesCurrentReply,
	info *randr.GetOutputInfoReply, w, h int) (mode randr.Mode, made bool, err error) {
	for _, m := range res.Modes {
		if int(m.Width) != w || int(m.Height) != h {
			continue
		}
		for _, om := range info.Modes {
			if om == randr.Mode(m.Id) {
				return om, false, nil
			}
		}
	}
	name := fmt.Sprintf("%dx%d", w, h)
	mi := randr.ModeInfo{
		Width: uint16(w), Height: uint16(h), DotClock: uint32(w * h * 60),
		HsyncStart: uint16(w), HsyncEnd: uint16(w), Htotal: uint16(w),
		VsyncStart: uint16(h), VsyncEnd: uint16(h), Vtotal: uint16(h),
		NameLen: uint16(len(name)),
	}
	r, err := randr.CreateMode(x, root, mi, name).Reply()
	if err != nil {
		return 0, false, fmt.Errorf("create mode: %w", err)
	}
	return r.Mode, true, nil
}
