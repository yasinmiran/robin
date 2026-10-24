//go:build linux

package wayland

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/godbus/dbus/v5"

	"github.com/yasinmiran/robin/internal/agent/gamma"
)

// Mutter's own display configuration interface, which still carries the
// gamma calls gnome-settings-daemon used for night light. No portal offers
// anything like them.
const (
	displayConfigName  = "org.gnome.Mutter.DisplayConfig"
	displayConfigPath  = "/org/gnome/Mutter/DisplayConfig"
	displayConfigIface = displayConfigName
)

// MutterStep is the keeper's kind for a MutterUndo.
const MutterStep = "mutter-gamma"

// MutterUndo puts a GNOME session's screens back as a curtain found them.
type MutterUndo struct {
	Ramps []gamma.Ramp `json:"ramps"`
}

var (
	// ErrNoDisplayConfig means the compositor offers no way to set the
	// screens' gamma, as on KDE Plasma.
	ErrNoDisplayConfig = errors.New("wayland: the desktop offers no way to darken its screens: only GNOME's mutter lets a program set their gamma")
	// ErrNoGamma means mutter lists no screen, as in a headless session.
	ErrNoGamma = errors.New("wayland: mutter lists no screen whose gamma a program can set")
)

// OpenCurtain readies a curtain over the user's GNOME Wayland session,
// through mutter's DisplayConfig on the session bus. Before every change to
// the ramps the curtain hands hold a MutterUndo for MutterStep, and makes no
// change hold refuses. OpenCurtain returns ErrNoDisplayConfig, ErrNoGamma or
// gamma.Uncovered for a session it cannot black out.
func OpenCurtain(hold func(kind string, undo any) error, log *slog.Logger) (*gamma.Curtain, error) {
	s, err := openScreens(context.Background())
	if err != nil {
		return nil, err
	}
	var owned bool
	if err := s.conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, displayConfigName).Store(&owned); err != nil {
		s.Close()
		return nil, fmt.Errorf("wayland: ask the session bus for mutter: %w", err)
	}
	if !owned {
		s.Close()
		return nil, ErrNoDisplayConfig
	}
	c, err := gamma.Open(s, func(ramps []gamma.Ramp) error { return hold(MutterStep, MutterUndo{Ramps: ramps}) }, log)
	if err != nil {
		s.Close()
		if errors.Is(err, gamma.ErrNoScreen) {
			return nil, ErrNoGamma
		}
		return nil, err
	}
	return c, nil
}

// UndoMutter is the keeper's undo for a MutterUndo.
func UndoMutter(ctx context.Context, data json.RawMessage) error {
	var u MutterUndo
	if err := json.Unmarshal(data, &u); err != nil {
		return err
	}
	s, err := openScreens(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	return gamma.Restore(s, u.Ramps)
}

// screens are a GNOME session's CRTCs, reached through mutter. Its gamma
// calls must quote the serial of the resources the CRTCs came from, and
// mutter refuses one that a monitor change has since made stale.
type screens struct {
	ctx    context.Context
	conn   *dbus.Conn
	obj    dbus.BusObject
	serial uint32
}

// openScreens connects to the session bus; ctx bounds the connection and
// every call on it.
func openScreens(ctx context.Context) (*screens, error) {
	conn, err := sessionBus(dbus.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	return &screens{ctx: ctx, conn: conn, obj: conn.Object(displayConfigName, displayConfigPath)}, nil
}

// CRTCs lists the CRTCs that drive a screen.
func (s *screens) CRTCs() ([]gamma.CRTC, error) {
	var (
		crtcs      []mutterCrtc
		outputs    []mutterOutput
		modes      []mutterMode
		maxW, maxH int32
	)
	if err := s.obj.CallWithContext(s.ctx, displayConfigIface+".GetResources", 0).Store(&s.serial, &crtcs, &outputs, &modes, &maxW, &maxH); err != nil {
		return nil, fmt.Errorf("wayland: ask mutter for its screens: %w", err)
	}
	var out []gamma.CRTC
	for _, c := range crtcs {
		if c.Mode < 0 {
			continue
		}
		var names []string
		for _, o := range outputs {
			if o.CRTC == int32(c.ID) {
				names = append(names, o.Name)
			}
		}
		out = append(out, gamma.CRTC{ID: c.ID, Output: strings.Join(names, "+")})
	}
	return out, nil
}

func (s *screens) Gamma(crtc uint32) (gamma.Ramp, error) {
	r := gamma.Ramp{CRTC: crtc}
	if err := s.obj.CallWithContext(s.ctx, displayConfigIface+".GetCrtcGamma", 0, s.serial, crtc).Store(&r.Red, &r.Green, &r.Blue); err != nil {
		return r, fmt.Errorf("wayland: read the gamma of CRTC %d: %w", crtc, err)
	}
	return r, nil
}

func (s *screens) SetGamma(r gamma.Ramp) error {
	if err := s.obj.CallWithContext(s.ctx, displayConfigIface+".SetCrtcGamma", 0, s.serial, r.CRTC, r.Red, r.Green, r.Blue).Err; err != nil {
		return fmt.Errorf("wayland: set the gamma of CRTC %d: %w", r.CRTC, err)
	}
	return nil
}

func (s *screens) Close() error { return s.conn.Close() }

// The parts of GetResources' reply, in its D-Bus signature
// (ua(uxiiiiiuaua{sv})a(uxiausauaua{sv})a(uxuudu)ii).
type (
	mutterCrtc struct {
		ID                  uint32
		WinsysID            int64
		X, Y, Width, Height int32
		Mode                int32 // -1 when the CRTC drives nothing
		Transform           uint32
		Transforms          []uint32
		Properties          map[string]dbus.Variant
	}
	mutterOutput struct {
		ID         uint32
		WinsysID   int64
		CRTC       int32 // -1 when no CRTC drives it
		CRTCs      []uint32
		Name       string
		Modes      []uint32
		Clones     []uint32
		Properties map[string]dbus.Variant
	}
	mutterMode struct {
		ID            uint32
		WinsysID      int64
		Width, Height uint32
		Rate          float64
		Flags         uint32
	}
)
