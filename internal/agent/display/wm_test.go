//go:build linux

package display

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestWaitForWindowRefusesAStrangersSocket claims the abstract socket of a
// display nobody runs, as any local user can, and checks that waitForWindow
// hands the display's cookie only to the server Spawn started.
func TestWaitForWindowRefusesAStrangersSocket(t *testing.T) {
	num, ln := listenAbstract(t)
	received := make(chan []byte, 8)
	go func() {
		defer close(received)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			b, _ := io.ReadAll(c)
			c.Close()
			received <- b
		}
	}()
	cookie := bytes.Repeat([]byte{0x5a}, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// The listener is this process; say the server is another.
	err := waitForWindow(ctx, num, cookie, os.Getpid()+1)
	var stranger *notServerError
	if !errors.As(err, &stranger) || stranger.pid != os.Getpid() || !strings.Contains(stranger.Error(), "X"+num) {
		t.Errorf("waitForWindow on a stranger's socket: %v, want a refusal naming its pid", err)
	}
	select {
	case b := <-received:
		if len(b) > 0 {
			t.Errorf("the stranger's socket received %d bytes", len(b))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitForWindow never connected")
	}

	// The same socket, when it is the server's, gets the cookie.
	if err := waitForWindow(ctx, num, cookie, os.Getpid()); err == nil {
		t.Error("waitForWindow found a window on a socket that is no X server")
	}
	select {
	case b := <-received:
		if !bytes.Contains(b, cookie) {
			t.Errorf("the server's socket received %q, not the cookie", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitForWindow never connected to the server's socket")
	}
	ln.Close()
}

// TestWaitForWindowTrustsTheSpawnedXvfb checks the other side: the Xvfb
// Spawn started passes the check, on whichever socket it listens.
func TestWaitForWindowTrustsTheSpawnedXvfb(t *testing.T) {
	d, _ := spawn(t, "xterm")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cookie := readEntries(t, d.XAuthority)[0].data
	if err := waitForWindow(ctx, strings.TrimPrefix(d.Name, ":"), cookie, d.xvfb.cmd.Process.Pid); err != nil {
		t.Fatalf("waitForWindow on the spawned display: %v", err)
	}
}

// listenAbstract listens on the abstract X socket of a display number that
// has no filesystem socket, so a dial falls through to it.
func listenAbstract(t *testing.T) (string, net.Listener) {
	t.Helper()
	for range 100 {
		num := strconv.Itoa(40000 + rand.IntN(20000))
		if _, err := os.Lstat("/tmp/.X11-unix/X" + num); err == nil {
			continue
		}
		if ln, err := net.Listen("unix", "@/tmp/.X11-unix/X"+num); err == nil {
			return num, ln
		}
	}
	t.Fatal("no free display number")
	return "", nil
}

// TestServedByWantsXvfbsUser: SO_PEERCRED names the pid that called listen,
// so another user's socket handed to a process that lives on carries a pid
// Xvfb may be given later. Both checks of a display's sockets take a
// listener for Xvfb only when its uid is this user's too.
func TestServedByWantsXvfbsUser(t *testing.T) {
	const xvfb = 4242
	me := uint32(os.Getuid())
	for _, tc := range []struct {
		name string
		cred unix.Ucred
		ok   bool
	}{
		{"Xvfb", unix.Ucred{Pid: xvfb, Uid: me}, true},
		{"another process of this user's", unix.Ucred{Pid: xvfb + 1, Uid: me}, false},
		{"another user's, at Xvfb's pid", unix.Ucred{Pid: xvfb, Uid: me + 1}, false},
		{"root's, at Xvfb's pid", unix.Ucred{Pid: xvfb, Uid: 0}, me == 0},
	} {
		err := servedBy("@/tmp/.X11-unix/X99", &tc.cred, xvfb)
		if tc.ok {
			if err != nil {
				t.Errorf("%s: %v, want it taken for Xvfb", tc.name, err)
			}
			continue
		}
		var stranger *notServerError
		if !errors.As(err, &stranger) || stranger.pid != int(tc.cred.Pid) || stranger.uid != tc.cred.Uid {
			t.Errorf("%s: %v, want a refusal naming pid %d and uid %d", tc.name, err, tc.cred.Pid, tc.cred.Uid)
		}
	}
}
