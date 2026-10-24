//go:build linux

package display

import (
	"bytes"
	"context"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
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
	if err == nil || !strings.Contains(err.Error(), "pid "+strconv.Itoa(os.Getpid())) {
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
