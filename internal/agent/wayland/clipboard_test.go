//go:build linux

package wayland

import (
	"context"
	"log/slog"
	"syscall"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/yasinmiran/robin/internal/protocol"
)

// staleOwner is a portal whose clipboard owner offers only text, while the
// portal goes on listing the formats of the owner before it, as KDE's
// portal can. A read of the text answers; a read of any other format gets
// no answer at all.
type staleOwner struct {
	dbus.BusObject // methods besides Call and CallWithContext are not used
	text           string
}

func (p *staleOwner) Call(method string, flags dbus.Flags, args ...any) *dbus.Call {
	return p.CallWithContext(context.Background(), method, flags, args...)
}

func (p *staleOwner) CallWithContext(ctx context.Context, method string, _ dbus.Flags, args ...any) *dbus.Call {
	if method != ifaceClipboard+".SelectionRead" {
		return &dbus.Call{}
	}
	if args[1] != textMimes[0] {
		<-ctx.Done()
		return &dbus.Call{Err: ctx.Err()}
	}
	var fds [2]int
	if err := syscall.Pipe2(fds[:], syscall.O_CLOEXEC); err != nil {
		return &dbus.Call{Err: err}
	}
	syscall.Write(fds[1], []byte(p.text))
	syscall.Close(fds[1])
	return &dbus.Call{Body: []any{dbus.UnixFD(fds[0])}}
}

// TestFollowOutlivesAReadNobodyAnswers: a copy announced with formats its
// owner does not have must not stop the clipboard following the desktop.
func TestFollowOutlivesAReadNobodyAnswers(t *testing.T) {
	s := &portalSession{p: &portal{obj: &staleOwner{text: "from-session"}}, handle: "/session"}
	c := &clipboard{
		s: s, updates: make(chan string, 1), contents: make(chan protocol.Clip, 1),
		log: slog.New(slog.DiscardHandler),
	}
	owner := make(chan *dbus.Signal, 1)
	go c.follow(owner)
	defer close(owner)
	owner <- &dbus.Signal{Body: []any{s.handle, vardict{
		"mime_types": dbus.MakeVariant([]string{protocol.MIMEHTML, protocol.MIMEPNG, textMimes[0]}),
	}}}
	select {
	case got := <-c.contents:
		if got.Text != "from-session" || got.HTML != "" || got.PNG != nil {
			t.Errorf("read %q, %q and %d bytes of PNG; want the owner's text alone", got.Text, got.HTML, len(got.PNG))
		}
	case <-time.After(3 * clipboardTimeout):
		t.Fatal("the clipboard stopped following the desktop at a read nobody answers")
	}
}
