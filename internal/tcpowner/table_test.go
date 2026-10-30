package tcpowner

import (
	"encoding/binary"
	"net"
	"testing"
)

// row is a MIB_TCPROW_OWNER_PID as Windows lays it out.
func row(state uint32, local, remote *net.TCPAddr, pid uint32) []byte {
	r := make([]byte, sizeofOwnerPIDRow)
	binary.LittleEndian.PutUint32(r[0:], state)
	copy(r[4:8], local.IP.To4())
	binary.BigEndian.PutUint16(r[8:], uint16(local.Port))
	copy(r[12:16], remote.IP.To4())
	binary.BigEndian.PutUint16(r[16:], uint16(remote.Port))
	binary.LittleEndian.PutUint32(r[20:], pid)
	return r
}

func table(rows ...[]byte) []byte {
	t := binary.LittleEndian.AppendUint32(nil, uint32(len(rows)))
	for _, r := range rows {
		t = append(t, r...)
	}
	return t
}

// TestOwnerPIDFindsTheDialer: of the rows Windows lists, only the
// established one from the dialer's end to the listener's names the pid
// to ask about; one the other way round, a closing one or none is not it.
func TestOwnerPIDFindsTheDialer(t *testing.T) {
	lo := net.IPv4(127, 0, 0, 1)
	dialer := &net.TCPAddr{IP: lo, Port: 50123}
	listener := &net.TCPAddr{IP: lo, Port: 9059}
	other := &net.TCPAddr{IP: lo, Port: 50124}
	full := table(
		row(mibTCPStateEstab, listener, dialer, 111), // the accepted end, this process's
		row(8, dialer, listener, 222),                // CLOSE_WAIT, of a port reused since
		row(mibTCPStateEstab, other, listener, 333),
		row(mibTCPStateEstab, dialer, listener, 444),
	)
	if pid, err := ownerPID(full, dialer, listener); err != nil || pid != 444 {
		t.Errorf("ownerPID = %d, %v; want 444", pid, err)
	}
	if pid, err := ownerPID(table(row(8, dialer, listener, 222)), dialer, listener); err == nil {
		t.Errorf("ownerPID of a closing socket = %d, want an error", pid)
	}
	if _, err := ownerPID(full[:len(full)-1], dialer, listener); err == nil {
		t.Error("ownerPID of a table shorter than its count succeeded")
	}
	if _, err := ownerPID(nil, dialer, listener); err == nil {
		t.Error("ownerPID of no table succeeded")
	}
	v6 := &net.TCPAddr{IP: net.IPv6loopback, Port: 50123}
	if _, err := ownerPID(full, v6, listener); err == nil {
		t.Error("ownerPID of an IPv6 end succeeded")
	}
}
