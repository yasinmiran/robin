//go:build windows

package tcpowner

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// tcpTableOwnerPIDConnections is TCP_TABLE_OWNER_PID_CONNECTIONS from the
// Windows SDK's iprtrmib.h: established and closing sockets, with the pid
// of each one's process.
const tcpTableOwnerPIDConnections = 4

var getExtendedTCPTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

// PeerUID is ErrUnavailable: Windows has no uids. PeerIsSelf asks what it
// can answer.
func PeerUID(net.Conn) (uint32, error) {
	return 0, ErrUnavailable
}

// PeerIsSelf reports whether the other end of c, a TCP connection over IPv4
// loopback, is held by a process of this process's account. Windows lists
// each socket with its process, and a process can read the account of
// another of its own account's; one it cannot open is another user's, or
// has gone, and either way is not this user's.
func PeerIsSelf(c net.Conn) (bool, error) {
	local, ok1 := c.LocalAddr().(*net.TCPAddr)
	remote, ok2 := c.RemoteAddr().(*net.TCPAddr)
	if !ok1 || !ok2 {
		return false, errors.New("not a TCP connection")
	}
	table, err := tcpTable()
	if err != nil {
		return false, fmt.Errorf("%w: GetExtendedTcpTable: %w", ErrUnavailable, err)
	}
	pid, err := ownerPID(table, remote, local)
	if err != nil {
		return false, err
	}
	me, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false, fmt.Errorf("%w: this process's account: %w", ErrUnavailable, err)
	}
	peer, err := processUser(pid)
	if err != nil {
		return false, fmt.Errorf("the account of process %d: %w", pid, err)
	}
	return peer.User.Sid.Equals(me.User.Sid), nil
}

// processUser is the account process pid runs as.
func processUser(pid uint32) (*windows.Tokenuser, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h)
	var token windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &token); err != nil {
		return nil, err
	}
	defer token.Close()
	return token.GetTokenUser()
}

// tcpTable is the host's MIB_TCPTABLE_OWNER_PID of IPv4 sockets. The table
// can grow between asking its size and filling it, so a buffer too small
// is grown and tried again a few times.
func tcpTable() ([]byte, error) {
	if err := getExtendedTCPTable.Find(); err != nil {
		return nil, err
	}
	size := uint32(4 + 64*sizeofOwnerPIDRow)
	for range 4 {
		buf := make([]byte, size)
		r, _, _ := getExtendedTCPTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0,
			windows.AF_INET, tcpTableOwnerPIDConnections, 0)
		switch syscall.Errno(r) {
		case 0:
			return buf, nil
		case windows.ERROR_INSUFFICIENT_BUFFER:
			size += 16 * sizeofOwnerPIDRow
		default:
			return nil, syscall.Errno(r)
		}
	}
	return nil, errors.New("the table kept growing")
}
