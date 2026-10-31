//go:build linux

package agent

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/agent/peer"
	"github.com/yasinmiran/robin/internal/agent/share"
	"github.com/yasinmiran/robin/internal/protocol"
)

// FuzzGuestControl feeds a guest's page's control messages to the
// session, as its peer decodes them. A guest without control changes
// nothing; one with control reaches the desktop only with input and the
// clipboard, and never with what only the owner's page sends: the size,
// the monitor shown, the stream's keyframes and tiles, the curtain and
// the silent room, invitations, and which guest has control.
func FuzzGuestControl(f *testing.F) {
	door, err := share.Open(share.NewInvites(fmt.Sprintf("f%d%d", os.Getpid(), time.Now().UnixNano()), os.Getuid()), slog.New(slog.DiscardHandler))
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { door.Close() })
	for _, s := range []string{
		`{"t":"key","down":true,"key":"a","code":"KeyA"}`,
		`{"t":"move","x":3,"y":4}`,
		`{"t":"button","button":2,"down":true}`,
		`{"t":"wheel","dy":120}`,
		`{"t":"release"}`,
		`{"t":"resize","w":2560,"h":1440}`,
		`{"t":"monitor","monitor":"HDMI-1"}`,
		`{"t":"clipboard","text":"hi"}`,
		`{"t":"text","text":"typed"}`,
		`{"t":"tiles","on":true}`,
		`{"t":"curtain","on":true}`,
		`{"t":"silent","on":true}`,
		`{"t":"invite","invite":{"user":"eve"}}`,
		`{"t":"invite","invite":{"user":"root"}}`,
		`{"t":"guest","guest":{"id":"g1","action":"grant"}}`,
		`{"t":"guest","guest":{"id":"g2","action":"grant"}}`,
		`{"t":"guest","guest":{"id":"g1","action":"revoke"}}`,
		`{"t":"file-fetch","file":{"id":"1"}}`,
		`{"t":"file-decline","file":{"id":"1"}}`,
		`{"t":"heartbeat"}`,
		`{"t":"from-a-newer-page"}`,
	} {
		f.Add([]byte(s), false)
		f.Add([]byte(s), true)
	}
	input := []protocol.ControlType{protocol.ControlKey, protocol.ControlMove, protocol.ControlButton, protocol.ControlWheel, protocol.ControlRelease}
	f.Fuzz(func(t *testing.T, data []byte, control bool) {
		var m protocol.Control
		if len(data) > peer.MaxControlBytes || json.Unmarshal(data, &m) != nil {
			return
		}
		desk := &fakeMonitors{}
		s := testSession(desk)
		s.shared.door = door
		door.Invites().Clear()
		s.private = &private{kick: make(chan struct{}, 1)}
		g, other := &guest{id: "g1", user: "bob"}, &guest{id: "g2", user: "carol"}
		g.control.Store(control)
		s.shared.guests = []*guest{g, other}
		s.controlAs(&viewer{guest: g}, m)
		s.typing.discard()

		if g.control.Load() != control || other.control.Load() || len(s.shared.guests) != 2 {
			t.Fatalf("a guest's %q changed who has control", m.T)
		}
		if inv := door.Invites().Open(); len(inv) > 0 {
			t.Fatalf("a guest's %q invited %q", m.T, inv[0].User)
		}
		s.private.mu.Lock()
		curtain, silent := s.private.wantCurtain, s.private.wantSilent
		s.private.mu.Unlock()
		if curtain || silent || s.stream.tilesWanted.Load() != nil {
			t.Fatalf("a guest's %q asked for the curtain %v, the silent room %v or tiles", m.T, curtain, silent)
		}
		desk.mu.Lock()
		defer desk.mu.Unlock()
		if len(desk.sizes) > 0 || len(desk.shown) > 0 || s.stream.keyframe.Load() {
			t.Fatalf("a guest's %q resized the desktop to %v, showed %v or asked for a keyframe", m.T, desk.sizes, desk.shown)
		}
		if !control && (len(desk.inputs) > 0 || len(desk.clipboard) > 0) {
			t.Fatalf("a guest without control sent %q to the desktop", m.T)
		}
		for _, in := range desk.inputs {
			if !slices.Contains(input, in.T) || in.T != m.T && in.T != protocol.ControlRelease {
				t.Fatalf("a guest's %q became the input %q", m.T, in.T)
			}
		}
		if control && slices.Contains(input, m.T) && len(desk.inputs) == 0 {
			t.Fatalf("a guest with control sent %q, which never reached the desktop", m.T)
		}
		if len(desk.clipboard) > 0 && m.T != protocol.ControlClipboard {
			t.Fatalf("a guest's %q set the clipboard", m.T)
		}
	})
}

// Only input, the clipboard, typed text and answers to the host's file
// offers reach the session from a guest with control. Every other
// control message is the owner's page's alone.
func TestGuestsSendOnlyInputAndAnswers(t *testing.T) {
	guests := []protocol.ControlType{
		protocol.ControlKey, protocol.ControlMove, protocol.ControlButton, protocol.ControlWheel, protocol.ControlRelease,
		protocol.ControlClipboard, protocol.ControlText, protocol.ControlFileFetch, protocol.ControlFileDecline,
	}
	for _, ct := range []protocol.ControlType{
		protocol.ControlResize, protocol.ControlMonitor, protocol.ControlTiles, protocol.ControlInvite,
		protocol.ControlGuest, protocol.ControlCurtain, protocol.ControlSilent,
	} {
		if h, ok := controlHandlers[ct]; !ok || !h.owner {
			t.Errorf("%q is not the owner's alone", ct)
		}
	}
	for ct, h := range controlHandlers {
		if !h.owner && !slices.Contains(guests, ct) {
			t.Errorf("a guest with control may send %q", ct)
		}
	}
}

// FuzzRanges turns any set of tile indexes into a manifest's Gone: half-open
// ranges, every number greater than the one before, covering exactly the
// set, as the page's checkGone takes them.
func FuzzRanges(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 3})
	f.Add([]byte{0, 5, 0, 1, 0, 2, 0, 3, 0, 9, 0, 8})
	f.Add([]byte{0, 2, 0, 2, 255, 255, 0, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		var idx []int
		for i := 0; i+1 < len(b); i += 2 {
			idx = append(idx, int(binary.BigEndian.Uint16(b[i:])))
		}
		gone := ranges(idx)
		if len(gone)%2 != 0 || !slices.IsSorted(gone) {
			t.Fatalf("ranges(%v) = %v", idx, gone)
		}
		for i := 1; i < len(gone); i++ {
			if gone[i] <= gone[i-1] {
				t.Fatalf("ranges(%v) = %v repeats %d", idx, gone, gone[i])
			}
		}
		var covered []int
		for k := 0; k < len(gone); k += 2 {
			for i := gone[k]; i < gone[k+1]; i++ {
				covered = append(covered, i)
			}
		}
		want := slices.Compact(slices.Sorted(slices.Values(idx)))
		if !slices.Equal(covered, want) {
			t.Fatalf("ranges(%v) = %v covers %v", idx, gone, covered)
		}
	})
}
