//go:build linux

package wayland

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/yasinmiran/remotejump/internal/protocol"
)

const (
	portalBus      = "org.freedesktop.portal.Desktop"
	portalPath     = dbus.ObjectPath("/org/freedesktop/portal/desktop")
	ifaceRemote    = "org.freedesktop.portal.RemoteDesktop"
	ifaceScreen    = "org.freedesktop.portal.ScreenCast"
	ifaceClipboard = "org.freedesktop.portal.Clipboard"
	ifaceRequest   = "org.freedesktop.portal.Request"
	ifaceSession   = "org.freedesktop.portal.Session"
)

// Values from the portal specification.
const (
	deviceKeyboard      = uint32(1)
	devicePointer       = uint32(2)
	sourceMonitor       = uint32(1)
	cursorHidden        = uint32(1)
	cursorEmbedded      = uint32(2)
	persistUntilRevoked = uint32(2)

	responseCancelled = uint32(1)
)

// requestTimeout bounds the portal calls that never show a dialog.
const requestTimeout = 10 * time.Second

var (
	errDeclined = errors.New("the screen sharing request was declined on the host")
	errNoAnswer = errors.New("nobody answered the screen sharing request on the host's screen")
)

type vardict = map[string]dbus.Variant

// portal is the xdg-desktop-portal frontend on the session bus.
type portal struct {
	conn *dbus.Conn
	sigs *signals
	obj  dbus.BusObject
	// sender is the connection's unique name the way request object paths
	// spell it: ":1.42" becomes "1_42".
	sender     string
	cursorMode uint32
}

func newPortal(conn *dbus.Conn, sigs *signals) (*portal, error) {
	names := conn.Names()
	if len(names) == 0 {
		return nil, errors.New("session bus gave no unique name")
	}
	p := &portal{
		conn: conn, sigs: sigs, obj: conn.Object(portalBus, portalPath),
		sender: strings.ReplaceAll(strings.TrimPrefix(names[0], ":"), ".", "_"),
	}
	if _, err := p.obj.GetProperty(ifaceRemote + ".AvailableDeviceTypes"); err != nil {
		return nil, explain(err)
	}
	v, err := p.obj.GetProperty(ifaceScreen + ".AvailableCursorModes")
	if err != nil {
		return nil, explain(err)
	}
	// Embedded draws the pointer into the frames: cursor metadata travels in
	// PipeWire buffer headers, which gst-launch cannot hand over.
	modes, _ := v.Value().(uint32)
	switch {
	case modes&cursorEmbedded != 0:
		p.cursorMode = cursorEmbedded
	case modes&cursorHidden != 0:
		p.cursorMode = cursorHidden
	}
	return p, nil
}

// explain turns the bus errors a missing portal produces into advice.
func explain(err error) error {
	var de dbus.Error
	if !errors.As(err, &de) {
		return err
	}
	switch de.Name {
	case "org.freedesktop.DBus.Error.ServiceUnknown", "org.freedesktop.DBus.Error.NameHasNoOwner":
		return errors.New("xdg-desktop-portal is not running in the desktop session; " +
			"install xdg-desktop-portal and your desktop's backend (xdg-desktop-portal-gnome or xdg-desktop-portal-kde)")
	case "org.freedesktop.DBus.Error.InvalidArgs", "org.freedesktop.DBus.Error.UnknownInterface",
		"org.freedesktop.DBus.Error.UnknownProperty", "org.freedesktop.DBus.Error.UnknownMethod":
		return fmt.Errorf("this desktop's portal offers no remote desktop (GNOME and KDE Plasma do; "+
			"wlroots compositors such as Sway do not): %w", err)
	}
	return err
}

// pending is a portal call waiting for its Response signal.
type pending struct {
	p      *portal
	method string
	path   dbus.ObjectPath
	resp   <-chan *dbus.Signal
	stop   func()
}

// send calls a method that answers through an org.freedesktop.portal.Request
// object. The Response match goes in before the call, on the path the portal
// derives from our unique name and handle_token, so the answer cannot be
// missed however fast it comes. The last argument must be the options dict.
func (p *portal) send(ctx context.Context, method string, args ...any) (*pending, error) {
	token := randomToken()
	path := portalPath + dbus.ObjectPath("/request/"+p.sender+"/"+token)
	resp, stop, err := p.sigs.watch(path, ifaceRequest, "Response")
	if err != nil {
		return nil, err
	}
	args[len(args)-1].(vardict)["handle_token"] = dbus.MakeVariant(token)
	var handle dbus.ObjectPath
	if err := p.obj.CallWithContext(ctx, method, 0, args...).Store(&handle); err != nil {
		stop()
		return nil, fmt.Errorf("%s: %w", shortName(method), explain(err))
	}
	if handle != path {
		stop()
		return nil, fmt.Errorf("%s: portal answered on %s, not %s; this portal is too old", shortName(method), handle, path)
	}
	return &pending{p: p, method: method, path: path, resp: resp, stop: stop}, nil
}

// wait returns the request's results. If ctx ends first it closes the
// request, which also takes down any dialog it put on screen. onSlow, if
// set, runs once when slow fires before the answer.
func (r *pending) wait(ctx context.Context, slow <-chan time.Time, onSlow func()) (vardict, error) {
	defer r.stop()
	for {
		select {
		case sig, ok := <-r.resp:
			if !ok {
				return nil, fmt.Errorf("%s: session bus closed", shortName(r.method))
			}
			// Anything on the user's bus can send a signal; a Response
			// that is not (u, a{sv}) is not the portal's.
			if len(sig.Body) != 2 {
				continue
			}
			code, codeOK := sig.Body[0].(uint32)
			results, resultsOK := sig.Body[1].(vardict)
			if !codeOK || !resultsOK {
				continue
			}
			switch code {
			case 0:
				return results, nil
			case responseCancelled:
				return nil, errDeclined
			default:
				return nil, fmt.Errorf("%s failed (portal response %d)", shortName(r.method), code)
			}
		case <-slow:
			slow = nil
			onSlow()
		case <-ctx.Done():
			r.p.conn.Object(portalBus, r.path).Call(ifaceRequest+".Close", 0)
			return nil, ctx.Err()
		}
	}
}

func (p *portal) request(method string, args ...any) (vardict, error) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	r, err := p.send(ctx, method, args...)
	if err != nil {
		return nil, err
	}
	return r.wait(ctx, nil, nil)
}

// portalSession is one RemoteDesktop session with a monitor screencast, as
// Start granted it.
type portalSession struct {
	p          *portal
	handle     dbus.ObjectPath
	closed     <-chan *dbus.Signal
	stopClosed func()

	node uint32
	// width and height are the stream's size in compositor coordinates, the
	// space pointer positions are given in; 0 when the portal did not say.
	width, height int
	devices       uint32
	clipboard     bool
	token         string
	// remote is a PipeWire connection that can only see this stream.
	remote *os.File
}

func (s *portalSession) viewOnly() bool {
	both := deviceKeyboard | devicePointer
	return s.devices&both != both
}

// close ends the portal session; the compositor stops the stream and drops
// the virtual input devices. The user's own session is untouched.
func (s *portalSession) close() {
	s.stopClosed()
	s.p.conn.Object(portalBus, s.handle).Call(ifaceSession+".Close", 0)
	if s.remote != nil {
		s.remote.Close()
	}
}

// consent says how Start waits for the user.
type consent struct {
	wait   time.Duration
	notify func(protocol.Notice)
}

// start opens a session and asks for keyboard, pointer, one monitor and the
// clipboard. With a restore token the portal can grant it without a dialog.
// The request is the same every time, since KDE refuses a token for a
// different one.
func (p *portal) start(ctx context.Context, token string, c consent) (*portalSession, error) {
	out, err := p.request(ifaceRemote+".CreateSession", vardict{"session_handle_token": dbus.MakeVariant(randomToken())})
	if err != nil {
		return nil, err
	}
	s := &portalSession{p: p}
	switch h := out["session_handle"].Value().(type) {
	case string:
		s.handle = dbus.ObjectPath(h)
	case dbus.ObjectPath:
		s.handle = h
	default:
		return nil, fmt.Errorf("CreateSession: no session handle in %v", out)
	}
	if s.closed, s.stopClosed, err = p.sigs.watch(s.handle, ifaceSession, "Closed"); err != nil {
		p.conn.Object(portalBus, s.handle).Call(ifaceSession+".Close", 0)
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			s.close()
		}
	}()

	// A portal without the Clipboard interface still shares the screen.
	p.obj.Call(ifaceClipboard+".RequestClipboard", 0, s.handle, vardict{})

	devices := vardict{
		"types":        dbus.MakeVariant(deviceKeyboard | devicePointer),
		"persist_mode": dbus.MakeVariant(persistUntilRevoked),
	}
	if token != "" {
		devices["restore_token"] = dbus.MakeVariant(token)
	}
	if _, err := p.request(ifaceRemote+".SelectDevices", s.handle, devices); err != nil {
		return nil, err
	}
	sources := vardict{"types": dbus.MakeVariant(sourceMonitor), "multiple": dbus.MakeVariant(false)}
	if p.cursorMode != 0 {
		sources["cursor_mode"] = dbus.MakeVariant(p.cursorMode)
	}
	if _, err := p.request(ifaceScreen+".SelectSources", s.handle, sources); err != nil {
		return nil, err
	}

	startCtx, cancel := context.WithTimeout(ctx, c.wait)
	defer cancel()
	deadline := time.Now().Add(c.wait)
	tell := func() {
		c.notify(protocol.Notice{Kind: protocol.NoticeConsent, Wait: int(time.Until(deadline).Round(time.Second).Seconds())})
	}
	// A restore answers at once. Only when it does not is a dialog likely up.
	var slow <-chan time.Time
	if token == "" {
		tell()
	} else {
		t := time.NewTimer(restoreGrace)
		defer t.Stop()
		slow = t.C
	}
	r, err := p.send(startCtx, ifaceRemote+".Start", s.handle, "", vardict{})
	if err != nil {
		return nil, err
	}
	out, err = r.wait(startCtx, slow, tell)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, fmt.Errorf("%w within %s", errNoAnswer, c.wait)
		}
		return nil, err
	}
	if err := s.parseStart(out); err != nil {
		return nil, err
	}

	var fd dbus.UnixFD
	if err := p.obj.Call(ifaceScreen+".OpenPipeWireRemote", 0, s.handle, vardict{}).Store(&fd); err != nil {
		return nil, fmt.Errorf("OpenPipeWireRemote: %w", err)
	}
	s.remote = os.NewFile(uintptr(fd), "pipewire-remote")
	ok = true
	return s, nil
}

func (s *portalSession) parseStart(out vardict) error {
	s.devices = deviceKeyboard | devicePointer
	if v, ok := out["devices"].Value().(uint32); ok {
		s.devices = v
	}
	s.clipboard, _ = out["clipboard_enabled"].Value().(bool)
	s.token, _ = out["restore_token"].Value().(string)
	var streams []struct {
		Node  uint32
		Props vardict
	}
	if v, ok := out["streams"]; !ok || dbus.Store([]any{v.Value()}, &streams) != nil || len(streams) == 0 {
		return fmt.Errorf("Start granted no stream: %v", out)
	}
	s.node = streams[0].Node
	if size, ok := streams[0].Props["size"].Value().([]any); ok && len(size) == 2 {
		w, _ := size[0].(int32)
		h, _ := size[1].(int32)
		s.width, s.height = int(w), int(h)
	}
	return nil
}

// call invokes a RemoteDesktop method on the session; args follow the
// session handle and an empty options dict.
func (s *portalSession) call(method string, args ...any) error {
	return s.p.obj.Call(ifaceRemote+"."+method, 0, append([]any{s.handle, vardict{}}, args...)...).Err
}

// randomToken returns a fresh handle token, valid as an object path element.
func randomToken() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "rj" + hex.EncodeToString(b)
}

func shortName(method string) string {
	return method[strings.LastIndexByte(method, '.')+1:]
}
