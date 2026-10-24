//go:build linux

package files

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yasinmiran/robin/internal/agent/agenttest"
)

// strangerOffer is the stranger's side: it offers its own executable at
// sock under a name that holds escapes, and prints what came of it for the
// test.
func strangerOffer(sock string) {
	c, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: sock, Net: "unixpacket"})
	if err != nil {
		fmt.Printf("stranger: dial eacces=%v: %v\n", errors.Is(err, syscall.EACCES), err)
		return
	}
	defer c.Close()
	f, err := os.Open("/proc/self/exe")
	if err != nil {
		fmt.Printf("stranger: %v\n", err)
		return
	}
	defer f.Close()
	names := `{"names":["\u001b]0;robin\u0007\u001b[2Jreport.pdf"]}`
	if _, _, err := c.WriteMsgUnix([]byte(names), unix.UnixRights(int(f.Fd())), nil); err != nil {
		fmt.Printf("stranger: send: %v\n", err)
		return
	}
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, err := c.Read(make([]byte, 4096))
	fmt.Printf("stranger: read %d bytes, hung up %v: %v\n", n, agenttest.HungUp(err), err)
}

// A second account on the host is kept from the offer socket by the
// session's 0700 run directory, and where that directory was opened up,
// by the peer check: no offer reaches the session, nothing is answered,
// and the log says only that a uid was refused, never the names it sent.
func TestAnotherUserCannotOfferFiles(t *testing.T) {
	if sock := agenttest.StrangerSocket(); sock != "" {
		strangerOffer(sock)
		return
	}
	s := agenttest.NewStranger(t)
	dir := filepath.Join(agenttest.OpenDir(t), "s1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, SocketName)
	var logged agenttest.LockedBuffer
	l, err := Listen(sock, slog.New(slog.NewTextHandler(&logged, nil)))
	if err != nil {
		t.Fatal(err)
	}
	reqs := make(chan *Request, 1)
	served := make(chan struct{})
	go func() { defer close(served); l.Serve(func(r *Request) { reqs <- r }) }()
	defer func() {
		l.Close()
		<-served
	}()
	offer := func() string { return s.RunTest(t, t.Name(), sock) }

	if out := offer(); !strings.Contains(out, "stranger: dial eacces=true") {
		t.Fatalf("through the 0700 run directory:\n%s", out)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sock, 0o666); err != nil {
		t.Fatal(err)
	}
	if out := offer(); !strings.Contains(out, "stranger: read 0 bytes, hung up true") {
		t.Fatalf("through an open run directory:\n%s", out)
	}
	select {
	case r := <-reqs:
		t.Fatalf("the stranger's offer reached the session: %+v", r.Files)
	default:
	}
	want := fmt.Sprintf("offer from another user: uid %d", s.UID)
	if l := logged.String(); !strings.Contains(l, want) || strings.Count(l, "\n") != 1 || strings.Contains(l, "report.pdf") {
		t.Fatalf("the log, want one line saying %q:\n%s", want, l)
	}
	agenttest.TerminalSafe(t, "the log", logged.String())
}
