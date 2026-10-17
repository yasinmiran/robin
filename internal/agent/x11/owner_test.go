//go:build linux

package x11

import (
	"bytes"
	"encoding/binary"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestAttachRefusesAnotherUsersServer claims the abstract socket of a display
// nobody runs, as any local user can, and checks that Attach gives whoever
// listens there neither the user's cookie nor a session.
func TestAttachRefusesAnotherUsersServer(t *testing.T) {
	num, ln := listenAbstract(t)
	// The listener is ours; make it a stranger's.
	saved := trustedServerUIDs
	trustedServerUIDs = []uint32{uint32(os.Getuid()) + 1}
	t.Cleanup(func() { trustedServerUIDs = saved })

	cookie := bytes.Repeat([]byte{0x5a}, 16)
	xauth := filepath.Join(t.TempDir(), "Xauthority")
	writeWildCookie(t, xauth, num, cookie)

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

	_, _, err := Attach(":"+num, []string{xauth})
	if err == nil || !strings.Contains(err.Error(), "uid") {
		t.Errorf("Attach to a stranger's server: err %v, want a refusal naming its owner", err)
	}
	ln.Close()
	for b := range received {
		if bytes.Contains(b, cookie) {
			t.Error("the stranger's server received the user's cookie")
		} else if len(b) > 0 {
			t.Errorf("the stranger's server received %d bytes", len(b))
		}
	}
}

func TestServerUIDIsTheListener(t *testing.T) {
	ln, err := net.Listen("unix", filepath.Join(t.TempDir(), "sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	nc, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	uid, err := serverUID(nc)
	if err != nil || uid != uint32(os.Getuid()) {
		t.Errorf("serverUID = %d, %v; want %d", uid, err, os.Getuid())
	}
}

// listenAbstract listens on the abstract X socket of a display number that
// has no filesystem socket, so dialSocket falls through to it.
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

// writeWildCookie writes an Xauthority file holding cookie for display num
// on any host.
func writeWildCookie(t *testing.T, path, num string, cookie []byte) {
	t.Helper()
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint16(65535))
	for _, f := range [][]byte{nil, []byte(num), []byte("MIT-MAGIC-COOKIE-1"), cookie} {
		binary.Write(&b, binary.BigEndian, uint16(len(f)))
		b.Write(f)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}
