package sshmux

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
)

// listenerOf reports whether a TCP socket listens on addr, an IPv4 address
// and port, and whether process pid holds it. Forwards ask this on Windows
// only; Linux answers too, so the tests can run that path with a real ssh.
func listenerOf(addr string, pid int) (listening, ours bool, err error) {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil || !ap.Addr().Is4() {
		return false, false, fmt.Errorf("sshmux: not an IPv4 address and port: %q", addr)
	}
	inode, err := listeningInode(ap)
	if err != nil || inode == "" {
		return false, false, err
	}
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	fds, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return true, false, nil // pid has exited
	}
	if err != nil {
		return true, false, err
	}
	want := "socket:[" + inode + "]"
	for _, fd := range fds {
		if link, err := os.Readlink(filepath.Join(dir, fd.Name())); err == nil && link == want {
			return true, true, nil
		}
	}
	return true, false, nil
}

// listeningInode returns the inode of the socket listening on ap, from
// /proc/net/tcp, or "" when none does.
func listeningInode(ap netip.AddrPort) (string, error) {
	b, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return "", err
	}
	// The kernel prints the address as the word it stores, in host byte
	// order, and the port as a number; 0A is TCP_LISTEN.
	ip := ap.Addr().As4()
	local := fmt.Sprintf("%08X:%04X", binary.NativeEndian.Uint32(ip[:]), ap.Port())
	for _, line := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) > 9 && f[1] == local && f[3] == "0A" {
			return f[9], nil
		}
	}
	return "", nil
}
