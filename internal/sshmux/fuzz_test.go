package sshmux

import (
	"net/netip"
	"strings"
	"testing"
	"unicode"
)

// FuzzCheckTarget feeds the forward target the host names. What passes
// is a loopback address with no zone and a port of 1 to 65535, written
// exactly as netip writes it, with nothing ssh would read as more than a
// target in its -L or -W spec: no expansion, quoting or escape, no white
// space, no leading dash. Every loopback IPv4 address and port the agent
// could listen on passes, and so does [::1].
func FuzzCheckTarget(f *testing.F) {
	for _, s := range []string{
		"127.0.0.1:41234", "127.0.0.1:1", "127.0.0.1:65535", "[::1]:22",
		"", "a:b:c", "/run/x:1", "[::1]:22:3", "127.0.0.1:0", "127.0.0.1:70000", "127.0.0.1:022",
		"[127.0.0.1]:1", "127.0.0.1:", ":22", "relative/x.sock", `/run/back\slash`, "[fe80::1%eth0]:22",
		"[::1%lo]:22", "-oProxyCommand=x:22", "host name:22", `h\ost:22`, "127.0.0.1:22 ", "[::1]:+22",
		"/tmp/${HOME}", "/run/x${PATH}y", "127.0.0.1:${PORT}", "localhost:4000", "10.0.0.5:4000",
		"/run/user/1000/robin/ice.sock", "[::ffff:127.0.0.1]:22", "127.1:22",
	} {
		f.Add(s, byte(0), byte(0), byte(1), uint16(4000))
	}
	f.Fuzz(func(t *testing.T, remote string, b1, b2, b3 byte, port uint16) {
		if checkTarget(remote) == nil {
			ap, err := netip.ParseAddrPort(remote)
			if err != nil || ap.String() != remote || !ap.Addr().IsLoopback() || ap.Addr().Zone() != "" || ap.Port() == 0 {
				t.Fatalf("took %q", remote)
			}
			if strings.ContainsAny(remote, "${}\\%") || strings.ContainsFunc(remote, unicode.IsSpace) || strings.HasPrefix(remote, "-") {
				t.Fatalf("took %q, which ssh would read as more than a target", remote)
			}
		}
		if port == 0 {
			return
		}
		for _, a := range []netip.Addr{netip.AddrFrom4([4]byte{127, b1, b2, b3}), netip.IPv6Loopback()} {
			if target := netip.AddrPortFrom(a, port).String(); checkTarget(target) != nil {
				t.Fatalf("refused the agent's own %q", target)
			}
		}
	})
}
