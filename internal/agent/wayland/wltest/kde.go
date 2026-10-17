package wltest

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/godbus/dbus/v5"
)

// ErrNoViewOnly is what AnswerDialog returns when asked for a view-only
// grant on KDE Plasma: its dialog grants the devices the request names, or
// nothing.
var ErrNoViewOnly = errors.New("KDE's consent dialog has no view-only choice")

const (
	atspiAccessible = "org.a11y.atspi.Accessible"
	atspiAction     = "org.a11y.atspi.Action"
	kdePortalApp    = "xdg-desktop-portal-kde"
)

// KDE reports whether the session's compositor is KWin.
func KDE() bool {
	conn, err := sessionBus()
	if err != nil {
		return false
	}
	defer conn.Close()
	return isKDE(conn)
}

func isKDE(conn *dbus.Conn) bool {
	var owned bool
	err := conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, "org.kde.KWin").Store(&owned)
	return err == nil && owned
}

// answerKDE presses Approve in xdg-desktop-portal-kde's dialog through
// AT-SPI. Qt shows its widgets there only when
// QT_LINUX_ACCESSIBILITY_ALWAYS_ON is set, as testdata/session.sh does.
func answerKDE(session *dbus.Conn, allowInput bool) error {
	if !allowInput {
		return ErrNoViewOnly
	}
	var addr string
	if err := session.Object("org.a11y.Bus", "/org/a11y/bus").Call("org.a11y.Bus.GetAddress", 0).Store(&addr); err != nil {
		return fmt.Errorf("accessibility bus: %w", err)
	}
	a11y, err := dbus.Connect(addr)
	if err != nil {
		return fmt.Errorf("accessibility bus: %w", err)
	}
	defer a11y.Close()

	deadline := time.Now().Add(30 * time.Second)
	for {
		if button, ok := findButton(a11y, "Approve"); ok {
			return press(a11y, button)
		}
		if time.Now().After(deadline) {
			return errors.New("no Approve button from xdg-desktop-portal-kde within 30 s")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// node is an accessible object: the bus name of its application and its path.
type node struct {
	Name string
	Path dbus.ObjectPath
}

func children(conn *dbus.Conn, n node) []node {
	var kids []node
	if conn.Object(n.Name, n.Path).Call(atspiAccessible+".GetChildren", 0).Store(&kids) != nil {
		return nil
	}
	return kids
}

func accessibleName(conn *dbus.Conn, n node) string {
	v, err := conn.Object(n.Name, n.Path).GetProperty(atspiAccessible + ".Name")
	if err != nil {
		return ""
	}
	s, _ := v.Value().(string)
	return s
}

// findButton looks through the KDE portal's windows for a widget named name
// that has a Press action.
func findButton(conn *dbus.Conn, name string) (node, bool) {
	root := node{"org.a11y.atspi.Registry", "/org/a11y/atspi/accessible/root"}
	for _, app := range children(conn, root) {
		if accessibleName(conn, app) != kdePortalApp {
			continue
		}
		queue := children(conn, app)
		for len(queue) > 0 {
			n := queue[0]
			queue = queue[1:]
			if accessibleName(conn, n) == name && pressIndex(conn, n) >= 0 {
				return n, true
			}
			queue = append(queue, children(conn, n)...)
		}
	}
	return node{}, false
}

func pressIndex(conn *dbus.Conn, n node) int32 {
	var actions []struct{ Name, Description, KeyBinding string }
	if conn.Object(n.Name, n.Path).Call(atspiAction+".GetActions", 0).Store(&actions) != nil {
		return -1
	}
	return int32(slices.IndexFunc(actions, func(a struct{ Name, Description, KeyBinding string }) bool {
		return a.Name == "Press"
	}))
}

func press(conn *dbus.Conn, n node) error {
	var done bool
	if err := conn.Object(n.Name, n.Path).Call(atspiAction+".DoAction", 0, pressIndex(conn, n)).Store(&done); err != nil {
		return fmt.Errorf("press: %w", err)
	}
	if !done {
		return errors.New("press: the button refused the action")
	}
	return nil
}
