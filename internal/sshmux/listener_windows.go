package sshmux

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetExtendedTcpTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

const (
	tcpTableOwnerPIDListener = 3  // TCP_TABLE_OWNER_PID_LISTENER
	tcpRowOwnerPIDSize       = 24 // MIB_TCPROW_OWNER_PID, six DWORDs
)

// listenerOf reports whether a TCP socket listens on addr, an IPv4 address
// and port, and whether process pid holds it, from the system's table of
// listeners and their owners.
func listenerOf(addr string, pid int) (listening, ours bool, err error) {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil || !ap.Addr().Is4() {
		return false, false, fmt.Errorf("sshmux: not an IPv4 address and port: %q", addr)
	}
	table, err := tcpListeners()
	if err != nil {
		return false, false, err
	}
	n := binary.LittleEndian.Uint32(table)
	if uint64(len(table)) < 4+uint64(n)*tcpRowOwnerPIDSize {
		return false, false, fmt.Errorf("sshmux: listener table of %d rows in %d bytes", n, len(table))
	}
	want := ap.Addr().As4()
	for i := range n {
		row := table[4+i*tcpRowOwnerPIDSize:][:tcpRowOwnerPIDSize]
		// dwState, then dwLocalAddr and dwLocalPort, both in network byte
		// order, then the remote end, then dwOwningPid.
		if [4]byte(row[4:8]) == want && binary.BigEndian.Uint16(row[8:10]) == ap.Port() {
			return true, binary.LittleEndian.Uint32(row[20:24]) == uint32(pid), nil
		}
	}
	return false, false, nil
}

// tcpListeners returns the MIB_TCPTABLE_OWNER_PID of the IPv4 listeners.
func tcpListeners() ([]byte, error) {
	size := uint32(4096)
	for {
		buf := make([]byte, size)
		r, _, _ := procGetExtendedTcpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)),
			0, windows.AF_INET, tcpTableOwnerPIDListener, 0)
		switch err := windows.Errno(r); err {
		case 0:
			return buf, nil
		case windows.ERROR_INSUFFICIENT_BUFFER:
			// size now holds what the table needs.
		default:
			return nil, fmt.Errorf("sshmux: list TCP listeners: %w", err)
		}
	}
}
