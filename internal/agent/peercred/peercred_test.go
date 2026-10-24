//go:build linux

package peercred

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOfNamesBothEnds checks each end of a unix connection sees the other:
// the dialler sees the listener, the accepted end the dialler, and here
// both are this process.
func TestOfNamesBothEnds(t *testing.T) {
	ln, err := net.Listen("unix", filepath.Join(t.TempDir(), "sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dialled, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer dialled.Close()
	accepted, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	for name, c := range map[string]net.Conn{"dialled": dialled, "accepted": accepted} {
		cred, err := Of(c)
		if err != nil || int(cred.Uid) != os.Getuid() || int(cred.Pid) != os.Getpid() {
			t.Errorf("Of(%s) = %+v, %v; want uid %d, pid %d", name, cred, err, os.Getuid(), os.Getpid())
		}
	}

	closed, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	if _, err := Of(closed); err == nil {
		t.Error("Of on a closed connection succeeded")
	}
}

func TestOfWantsAUnixConnection(t *testing.T) {
	c, peer := net.Pipe()
	defer c.Close()
	defer peer.Close()
	if _, err := Of(c); err == nil || !strings.Contains(err.Error(), "not a unix connection") {
		t.Errorf("Of on a pipe: %v", err)
	}
	if _, err := Of(&net.UnixConn{}); err == nil {
		t.Error("Of on a unix connection with no socket succeeded")
	}
}
