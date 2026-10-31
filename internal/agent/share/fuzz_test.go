//go:build linux

package share

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/protocol"
)

// codeMark stands for the invitation's own code in a fuzzer's input,
// which it could never guess.
const codeMark = "{code}"

// FuzzRedeem presents codes for an invitation of uid 1001. Only its own
// code from its own uid redeems it, once; a refusal says nothing of the
// secret and leaves the invitation open until maxTries of them.
func FuzzRedeem(f *testing.F) {
	for _, s := range []string{codeMark, codeMark + "x", "s1-4242-" + strings.Repeat("a", 26), "", "s1-4242-", strings.ToUpper(codeMark)} {
		f.Add(s, 1001, uint8(1))
		f.Add(s, 1002, uint8(3))
	}
	f.Fuzz(func(t *testing.T, code string, uid int, times uint8) {
		in, _ := testInvites(map[string]int{"alice": 1001})
		inv, err := in.Invite("alice")
		if err != nil {
			t.Fatal(err)
		}
		secret := inv.Code[strings.LastIndexByte(inv.Code, '-')+1:]
		code = strings.ReplaceAll(code, codeMark, inv.Code)
		good := uid == 1001 && code == inv.Code
		for try := range int(times%5) + 1 {
			user, err := in.Redeem(uid, code)
			switch {
			case good && try == 0:
				if err != nil || user != "alice" {
					t.Fatalf("its own code: %q, %v", user, err)
				}
			case err == nil:
				t.Fatalf("try %d of %q from uid %d redeemed the invitation", try, code, uid)
			case !errors.Is(err, ErrBadCode) || strings.Contains(err.Error(), secret):
				t.Fatalf("refused with %v", err)
			}
			open := in.Invited(1001)
			wantOpen := !good && (uid != 1001 || try+1 < maxTries)
			if open != wantOpen {
				t.Fatalf("after try %d of %q from uid %d the invitation is open %v", try, code, uid, open)
			}
		}
	})
}

var doors atomic.Int64

// FuzzDoor writes anything to the guests' door as the invited uid, then
// stops writing. The door admits it only when its first message is a join
// in this protocol with the invitation's code, and spends the invitation
// then; anything else is answered with an error and leaves the
// invitation open.
func FuzzDoor(f *testing.F) {
	for _, s := range []string{
		`{"type":"join","join":{"version":2,"code":"` + codeMark + `"}}` + "\n",
		`{"type":"join","join":{"version":2,"code":"` + codeMark + `"}}`,
		`{"type":"ping"}` + "\r\n" + `{"type":"join","join":{"version":2,"code":"` + codeMark + `"}}` + "\n",
		`{"type":"join","join":{"version":3,"code":"` + codeMark + `"}}` + "\n",
		`{"type":"join","join":{"version":2,"code":"` + codeMark + `x"}}` + "\n",
		`{"type":"join"}` + "\n",
		`{"type":"start","start":{"version":2}}` + "\n",
		"\x1b]0;robin\x07\n",
		"",
	} {
		f.Add([]byte(s))
	}
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.Fuzz(func(t *testing.T, stream []byte) {
		in, _ := testInvites(map[string]int{"me": os.Getuid()})
		in.now = nil
		in.session = fmt.Sprintf("f%d%d", os.Getpid(), doors.Add(1))
		inv, err := in.Invite("me")
		if err != nil {
			t.Fatal(err)
		}
		stream = bytes.ReplaceAll(stream, []byte(codeMark), []byte(inv.Code))
		d, err := open(in, discard, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		c, err := net.Dial("unix", protocol.ShareSocket(in.session))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := c.Write(stream); err != nil {
			t.Fatal(err)
		}
		c.(*net.UnixConn).CloseWrite()

		want := admits(stream, inv.Code)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if want {
			g, err := d.Next(ctx)
			if err != nil {
				t.Fatalf("its own join was not admitted: %v", err)
			}
			g.Conn.Close()
			if in.Invited(os.Getuid()) {
				t.Fatal("the invitation is open after it admitted a guest")
			}
			return
		}
		// The door hangs up as soon as the stream ends, which can beat
		// the error it was about to send.
		m, err := readReply(c)
		if err == nil && m.Type != protocol.TypeError {
			t.Fatalf("refused with %+v", m)
		}
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
			t.Fatalf("refused with %v", err)
		}
		if strings.Contains(m.Error, inv.Code[strings.LastIndexByte(inv.Code, '-')+1:]) && !bytes.Contains(stream, []byte(inv.Code)) {
			t.Fatalf("the refusal tells the secret: %q", m.Error)
		}
		select {
		case g := <-d.arrivals:
			t.Fatalf("admitted %+v", g)
		default:
		}
		if !in.Invited(os.Getuid()) {
			t.Fatal("a refused connection withdrew the invitation")
		}
	})
}

// admits reports whether stream opens with a join the door should take:
// its first message other than a ping, as protocol.Conn reads it.
func admits(stream []byte, code string) bool {
	c := protocol.NewConn(bytes.NewReader(stream), io.Discard, 0, 0)
	defer c.Close()
	m, err := c.Recv(context.Background())
	return err == nil && m.Type == protocol.TypeJoin && m.Join != nil && m.Join.Version == protocol.Version && m.Join.Code == code
}
