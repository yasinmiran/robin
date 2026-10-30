package tcpowner

import (
	"encoding/binary"
	"errors"
	"net"
)

// From the Windows SDK's tcpmib.h: MIB_TCP_STATE_ESTAB, and the size of
// one MIB_TCPROW_OWNER_PID, six DWORDs.
const (
	mibTCPStateEstab  = 5
	sizeofOwnerPIDRow = 24
)

// ownerPID finds, in a MIB_TCPTABLE_OWNER_PID as GetExtendedTcpTable
// fills it, the established socket from local to remote and returns the
// pid that holds it. The table's DWORDs are little-endian, its addresses
// in network order, and each port is the first two bytes of its DWORD,
// big-endian. It is apart from the Windows call so it can be tested
// anywhere.
func ownerPID(table []byte, local, remote *net.TCPAddr) (uint32, error) {
	le := binary.LittleEndian
	if len(table) < 4 {
		return 0, errors.New("tcp table: short")
	}
	n := uint64(le.Uint32(table))
	rows := table[4:]
	if n*sizeofOwnerPIDRow > uint64(len(rows)) {
		return 0, errors.New("tcp table: shorter than its count")
	}
	la, ra := local.IP.To4(), remote.IP.To4()
	if la == nil || ra == nil {
		return 0, errors.New("not a TCP connection over IPv4")
	}
	for i := range n {
		r := rows[i*sizeofOwnerPIDRow:]
		if le.Uint32(r[0:]) != mibTCPStateEstab ||
			string(r[4:8]) != string(la) || int(binary.BigEndian.Uint16(r[8:])) != local.Port ||
			string(r[12:16]) != string(ra) || int(binary.BigEndian.Uint16(r[16:])) != remote.Port {
			continue
		}
		return le.Uint32(r[20:]), nil
	}
	return 0, errors.New("the other end is not a socket of this host")
}
