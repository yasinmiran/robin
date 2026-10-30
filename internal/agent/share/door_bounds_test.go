//go:build linux

package share

import (
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/protocol"
)

// Any account on the host may knock, so knocking must not cost the owner
// more than a bounded log: a loop of connections from an uninvited uid
// leaves the first refusal, with its uid, and no line per knock.
func TestKnocksFromAnUninvitedUIDLogBounded(t *testing.T) {
	d, logged := openDoor(t, map[string]int{"other": 4243})
	if _, err := d.Invites().Invite("other"); err != nil {
		t.Fatal(err)
	}
	const knocks = 500
	for range knocks {
		c, err := net.Dial("unix", protocol.ShareSocket(d.inv.session))
		if err != nil {
			t.Fatal(err)
		}
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		c.Read(make([]byte, 1)) // the door hangs up
		c.Close()
	}
	waitFor(t, func() bool { return strings.Contains(logged.String(), "no invitation") })
	l := logged.String()
	if !strings.Contains(l, "uid="+strconv.Itoa(os.Getuid())) {
		t.Errorf("the first refusal does not name its uid:\n%s", l)
	}
	if n := strings.Count(l, "\n"); n > 10 || len(l) > 4<<10 {
		t.Errorf("%d knocks logged %d lines, %d bytes; want a bounded few", knocks, n, len(l))
	}
}
