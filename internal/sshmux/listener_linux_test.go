package sshmux

import (
	"net"
	"os"
	"testing"
)

func TestListenerOf(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if listening, ours, err := listenerOf(addr, os.Getpid()); err != nil || !listening || !ours {
		t.Errorf("our own listener: listening %v, ours %v, %v", listening, ours, err)
	}
	if listening, ours, err := listenerOf(addr, os.Getppid()); err != nil || !listening || ours {
		t.Errorf("a listener of another process: listening %v, ours %v, %v", listening, ours, err)
	}
	if listening, _, err := listenerOf(addr, 1<<30); err != nil || !listening {
		t.Errorf("a process that does not exist: listening %v, %v", listening, err)
	}
	// A connection to the port is not a listener on it.
	c, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ln.Close()
	if listening, _, err := listenerOf(addr, os.Getpid()); err != nil || listening {
		t.Errorf("a closed listener: listening %v, %v", listening, err)
	}
	if listening, _, err := listenerOf(c.LocalAddr().String(), os.Getpid()); err != nil || listening {
		t.Errorf("a connected socket: listening %v, %v", listening, err)
	}
	for _, bad := range []string{"localhost:1", "[::1]:1", "127.0.0.1"} {
		if _, _, err := listenerOf(bad, os.Getpid()); err == nil {
			t.Errorf("listenerOf(%q) took it", bad)
		}
	}
}
