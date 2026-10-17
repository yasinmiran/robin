// Package wltest stands in for the person at a test host's screen, who has
// to answer the portal's consent dialog. On GNOME it clicks through mutter's
// own remote desktop interface, which asks nobody; on KDE Plasma it presses
// the dialog's button through AT-SPI. Only tests may import it: the agent
// always goes through the portal, where the user decides.
package wltest

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	mutterRD = "org.gnome.Mutter.RemoteDesktop"
	mutterSC = "org.gnome.Mutter.ScreenCast"
	btnLeft  = int32(0x110)
)

// Where xdg-desktop-portal-gnome puts its controls on a 1280x720 monitor:
// measured on 46, and still inside them on 49, whose dialog is a few pixels
// narrower.
const (
	toggleX, toggleY = 896, 266 // "Allow Remote Interaction"
	shareX, shareY   = 922, 132 // "Share"
)

// AnswerDialog waits for the consent dialog to come up, then shares the
// screen, allowing remote interaction if allowInput is set. KDE Plasma has
// no view-only choice; there it returns ErrNoViewOnly when allowInput is
// not set.
func AnswerDialog(allowInput bool) error {
	conn, err := sessionBus()
	if err != nil {
		return err
	}
	defer conn.Close()
	if isKDE(conn) {
		return answerKDE(conn, allowInput)
	}
	return answerGNOME(conn, allowInput)
}

func sessionBus() (*dbus.Conn, error) {
	return dbus.Connect("unix:path=" + os.Getenv("XDG_RUNTIME_DIR") + "/bus")
}

// answerGNOME clicks xdg-desktop-portal-gnome's dialog through mutter.
func answerGNOME(conn *dbus.Conn, allowInput bool) error {
	time.Sleep(3 * time.Second)
	var rdPath dbus.ObjectPath
	if err := conn.Object(mutterRD, "/org/gnome/Mutter/RemoteDesktop").Call(mutterRD+".CreateSession", 0).Store(&rdPath); err != nil {
		return fmt.Errorf("mutter RemoteDesktop: %w", err)
	}
	rd := conn.Object(mutterRD, rdPath)
	defer rd.Call(mutterRD+".Session.Stop", 0)
	id, err := rd.GetProperty(mutterRD + ".Session.SessionId")
	if err != nil {
		return err
	}
	var scPath, stream dbus.ObjectPath
	if err := conn.Object(mutterSC, "/org/gnome/Mutter/ScreenCast").Call(mutterSC+".CreateSession", 0,
		map[string]dbus.Variant{"remote-desktop-session-id": id}).Store(&scPath); err != nil {
		return fmt.Errorf("mutter ScreenCast: %w", err)
	}
	// Absolute pointer positions are relative to a stream.
	if err := conn.Object(mutterSC, scPath).Call(mutterSC+".Session.RecordMonitor", 0, "", map[string]dbus.Variant{}).Store(&stream); err != nil {
		return err
	}
	if err := conn.AddMatchSignal(dbus.WithMatchObjectPath(stream), dbus.WithMatchMember("PipeWireStreamAdded")); err != nil {
		return err
	}
	added := make(chan *dbus.Signal, 4)
	conn.Signal(added)
	if err := rd.Call(mutterRD+".Session.Start", 0).Err; err != nil {
		return err
	}
	select {
	case <-added:
	case <-time.After(5 * time.Second):
		return errors.New("mutter's stream never started")
	}

	move := func(x, y float64) error {
		return rd.Call(mutterRD+".Session.NotifyPointerMotionAbsolute", 0, string(stream), x, y).Err
	}
	click := func(x, y float64) error {
		if err := move(x, y); err != nil {
			return err
		}
		time.Sleep(200 * time.Millisecond)
		for _, down := range []bool{true, false} {
			if err := rd.Call(mutterRD+".Session.NotifyPointerButton", 0, btnLeft, down).Err; err != nil {
				return err
			}
		}
		time.Sleep(500 * time.Millisecond)
		return nil
	}
	// mutter drops a session's first event, so that one goes nowhere useful.
	move(100, 100)
	time.Sleep(300 * time.Millisecond)
	if allowInput {
		if err := click(toggleX, toggleY); err != nil {
			return err
		}
	}
	return click(shareX, shareY)
}
