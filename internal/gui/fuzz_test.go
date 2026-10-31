package gui

import (
	"bytes"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pion/stun/v4"

	"github.com/yasinmiran/robin/internal/relay"
	"github.com/yasinmiran/robin/internal/runes"
)

// FuzzPrintable feeds printable the host's words on their way to the
// user's terminal: what it gives is UTF-8 with no control character but a
// newline or a tab and nothing that reorders text, and is printable
// already.
func FuzzPrintable(f *testing.F) {
	for _, s := range []string{
		"plain text, tabs\tand\nnewlines", "Grüße, 日本", "\x1b[2Jcleared", "bell\a back\b cr\r del\x7f",
		"c1 \u009b2J", "raw \x9b2J byte", "‮txet", "a⁦b⁩", "p q ", "\xe6\x97", `\x1b`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := printable(s)
		if !utf8.ValidString(out) {
			t.Fatalf("printable(%q) = %q is not UTF-8", s, out)
		}
		if strings.ContainsFunc(out, func(r rune) bool {
			return unicode.IsControl(r) && r != '\n' && r != '\t' || runes.ReordersText(r)
		}) {
			t.Fatalf("printable(%q) = %q", s, out)
		}
		if again := printable(out); again != out {
			t.Fatalf("printable is not idempotent on %q: %q", out, again)
		}
	})
}

// FuzzTunnelSplice connects to the tunnel with a check as the browser
// sends it, signed with the agent's password or another, one byte
// flipped, its RFC 4571 length off by some, and more bytes after. The
// tunnel splices the connection to the forward only when its first frame
// is a check relay.Credentials admit, and then passes every byte on as it
// came; the same check again is refused.
func FuzzTunnelSplice(f *testing.F) {
	f.Add("brws", true, uint16(0), byte(0), int8(0), []byte("rest"))
	f.Add("brws", false, uint16(0), byte(0), int8(0), []byte{})
	f.Add("brws", true, uint16(40), byte(1), int8(0), []byte{})
	f.Add("brws", true, uint16(0), byte(0), int8(-3), []byte{})
	f.Add("brws", true, uint16(0), byte(0), int8(5), bytes.Repeat([]byte{7}, 600))
	f.Add("brws", true, uint16(0), byte(0), int8(127), []byte{})
	creds := relay.Credentials{Ufrag: "aGeNtUfRaGaGeNtU", Pwd: "aGeNtPaSsWoRdaGeNtPaSsWo"}
	fwd, accepted := testForwardF(f)
	counted := &countingForward{dialer: fwd}
	tun := &tunnel{log: slog.New(slog.DiscardHandler), firstCheck: time.Second, fwd: counted, creds: creds}
	f.Fuzz(func(t *testing.T, peer string, signed bool, at uint16, flip byte, skew int8, rest []byte) {
		pwd := creds.Pwd
		if !signed {
			pwd = "guessedpasswordguessedpa"
		}
		m, err := stun.Build(stun.TransactionID, stun.BindingRequest, stun.NewUsername(creds.Ufrag+":"+peer), stun.NewShortTermIntegrity(pwd))
		if err != nil || len(m.Raw) > 0xffff {
			return
		}
		data := framed(m.Raw)
		if flip != 0 {
			data[2+int(at)%len(m.Raw)] ^= flip
		}
		binary.BigEndian.PutUint16(data, uint16(int(binary.BigEndian.Uint16(data))+int(skew)))
		data = append(data, rest...)

		var first []byte
		if n := int(binary.BigEndian.Uint16(data)); n <= maxCheckBytes && len(data) >= 2+n {
			first = data[:2+n]
		}
		want := first != nil && creds.Admits(first[2:])
		for try := range 2 {
			before := counted.dials.Load()
			splice(t, tun, data)
			dialed := counted.dials.Load() != before
			if dialed != (want && try == 0) {
				t.Fatalf("try %d: spliced %v, want %v, for % x", try, dialed, want && try == 0, data)
			}
			if !dialed {
				continue
			}
			u := <-accepted
			got, _ := io.ReadAll(u)
			u.Close()
			if !bytes.Equal(got, data) {
				t.Fatalf("the forward got % x of % x", got, data)
			}
		}
	})
}

// splice runs tun.splice on a connection that sends data and then closes.
func splice(t *testing.T, tun *tunnel, data []byte) {
	t.Helper()
	c, s := net.Pipe()
	go func() {
		c.Write(data)
		c.Close()
	}()
	done := make(chan struct{})
	go func() {
		tun.splice(s)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("splice never returned")
	}
}

// testForwardF is testForward for a fuzz target, whose forward serves
// every input.
func testForwardF(f *testing.F) (fakeForward, <-chan net.Conn) {
	f.Helper()
	sock := f.TempDir() + "/fwd.sock"
	u, err := net.Listen("unix", sock)
	if err != nil {
		f.Fatal(err)
	}
	accepted := make(chan net.Conn, 8)
	go func() {
		for {
			c, err := u.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	f.Cleanup(func() { u.Close() })
	return fakeForward{sock}, accepted
}
