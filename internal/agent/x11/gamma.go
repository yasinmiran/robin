//go:build linux

package x11

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/xproto"

	"github.com/yasinmiran/robin/internal/agent/gamma"
)

// GammaUndo puts a display's screens back as a curtain found them: the
// display, the authority file the agent got in with ("" for host-based
// access) and the ramp of every CRTC it darkened.
type GammaUndo struct {
	Display    string       `json:"display"`
	XAuthority string       `json:"xauthority,omitempty"`
	Ramps      []gamma.Ramp `json:"ramps"`
}

// GammaStep is the keeper's kind for a GammaUndo.
const GammaStep = "x11-gamma"

// ErrNoGamma means the display drives no screen at all, as with an X server
// that has no CRTCs.
var ErrNoGamma = errors.New("x11: no screen on this display has a gamma ramp a client can set")

// OpenCurtain readies a curtain over display's screens, through RandR on a
// connection of its own, made the way the agent's own Conn was: with the
// cookie in xauthority, or "" for host-based access. Open that Conn first;
// see useRandR. Before every change to the ramps the curtain hands hold a
// GammaUndo for GammaStep, and makes no change hold refuses. OpenCurtain
// returns ErrNoGamma, or gamma.Uncovered, for a display it cannot black out.
func OpenCurtain(display, xauthority string, hold func(kind string, undo any) error, log *slog.Logger) (*gamma.Curtain, error) {
	s, err := openScreens(context.Background(), display, xauthority)
	if err != nil {
		return nil, err
	}
	c, err := gamma.Open(s, func(ramps []gamma.Ramp) error {
		return hold(GammaStep, GammaUndo{Display: display, XAuthority: xauthority, Ramps: ramps})
	}, log)
	if err != nil {
		s.Close()
		if errors.Is(err, gamma.ErrNoScreen) {
			return nil, ErrNoGamma
		}
		return nil, err
	}
	return c, nil
}

// UndoGamma is the keeper's undo for a GammaUndo: it connects to the display
// on its own and puts the ramps back.
func UndoGamma(ctx context.Context, data json.RawMessage) error {
	var u GammaUndo
	if err := json.Unmarshal(data, &u); err != nil {
		return err
	}
	s, err := openScreens(ctx, u.Display, u.XAuthority)
	if err != nil {
		return err
	}
	defer s.Close()
	return gamma.Restore(s, u.Ramps)
}

// screens are an X display's CRTCs, reached through RandR.
type screens struct {
	x    *xgb.Conn
	root xproto.Window
}

// openScreens connects to display, with ctx's deadline on the connection.
func openScreens(ctx context.Context, display, xauthority string) (*screens, error) {
	x, err := dialAs(ctx, display, xauthority)
	if err != nil {
		return nil, fmt.Errorf("x11: connect %s: %w", display, err)
	}
	if err := useRandR(x); err != nil {
		x.Close()
		return nil, fmt.Errorf("x11: %s: RandR: %w", display, err)
	}
	return &screens{x: x, root: xproto.Setup(x).DefaultScreen(x).Root}, nil
}

// CRTCs lists the CRTCs in use that drive an output.
func (s *screens) CRTCs() ([]gamma.CRTC, error) {
	res, err := randr.GetScreenResourcesCurrent(s.x, s.root).Reply()
	if err != nil {
		return nil, fmt.Errorf("x11: list CRTCs: %w", err)
	}
	var out []gamma.CRTC
	for _, crtc := range res.Crtcs {
		info, err := randr.GetCrtcInfo(s.x, crtc, res.ConfigTimestamp).Reply()
		if err != nil {
			return nil, fmt.Errorf("x11: read CRTC %d: %w", crtc, err)
		}
		if info.Mode == 0 || len(info.Outputs) == 0 {
			continue
		}
		var names []string
		for _, o := range info.Outputs {
			if oi, err := randr.GetOutputInfo(s.x, o, res.ConfigTimestamp).Reply(); err == nil {
				names = append(names, string(oi.Name))
			}
		}
		out = append(out, gamma.CRTC{ID: uint32(crtc), Output: strings.Join(names, "+")})
	}
	return out, nil
}

// Gamma reads crtc's ramp; a CRTC whose ramp has no entries has none.
func (s *screens) Gamma(crtc uint32) (gamma.Ramp, error) {
	r := gamma.Ramp{CRTC: crtc}
	size, err := randr.GetCrtcGammaSize(s.x, randr.Crtc(crtc)).Reply()
	if err != nil {
		return r, fmt.Errorf("x11: read the gamma size of CRTC %d: %w", crtc, err)
	}
	if size.Size == 0 {
		return r, nil
	}
	g, err := randr.GetCrtcGamma(s.x, randr.Crtc(crtc)).Reply()
	if err != nil {
		return r, fmt.Errorf("x11: read the gamma of CRTC %d: %w", crtc, err)
	}
	r.Red, r.Green, r.Blue = g.Red, g.Green, g.Blue
	return r, nil
}

func (s *screens) SetGamma(r gamma.Ramp) error {
	if err := randr.SetCrtcGammaChecked(s.x, randr.Crtc(r.CRTC), uint16(len(r.Red)), r.Red, r.Green, r.Blue).Check(); err != nil {
		return fmt.Errorf("x11: set the gamma of CRTC %d: %w", r.CRTC, err)
	}
	return nil
}

func (s *screens) Close() error {
	s.x.Close()
	return nil
}

// dialAs connects to display as Attach did: with the cookie from xauthority,
// or with none for host-based access. ctx's deadline, if any, bounds the
// connection for its whole life.
func dialAs(ctx context.Context, display, xauthority string) (*xgb.Conn, error) {
	num, err := displayNumber(display)
	if err != nil {
		return nil, err
	}
	cookie := noCookie
	if xauthority != "" {
		if cookie, err = readCookie(xauthority, num); err != nil {
			return nil, err
		}
	}
	nc, err := dialSocket(num)
	if err != nil {
		return nil, err
	}
	if d, ok := ctx.Deadline(); ok {
		nc.SetDeadline(d)
	}
	x, err := xgb.NewConnNetWithCookieHex(nc, hex.EncodeToString(cookie))
	if err != nil {
		nc.Close()
		return nil, err
	}
	return x, nil
}

// useRandR readies x for RandR requests. randr.Init also writes xgb's global
// event and error tables, which every other connection's reader reads, so
// once a connection to this server has filled them (the agent's Conn), x
// only learns the extension's opcode.
func useRandR(x *xgb.Conn) error {
	ext, err := xproto.QueryExtension(x, uint16(len("RANDR")), "RANDR").Reply()
	if err != nil {
		return err
	}
	if !ext.Present {
		return errors.New("the server has no RandR")
	}
	if _, known := xgb.NewEventFuncs[int(ext.FirstEvent)]; !known {
		if err := randr.Init(x); err != nil {
			return err
		}
	} else {
		x.ExtLock.Lock()
		x.Extensions["RANDR"] = ext.MajorOpcode
		x.ExtLock.Unlock()
	}
	v, err := randr.QueryVersion(x, 1, 3).Reply()
	if err != nil {
		return err
	}
	if v.MajorVersion == 1 && v.MinorVersion < 3 {
		return fmt.Errorf("RandR %d.%d is too old for gamma ramps, want 1.3", v.MajorVersion, v.MinorVersion)
	}
	return nil
}
