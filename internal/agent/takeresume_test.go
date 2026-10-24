//go:build linux

package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/protocol"
)

func TestTakeResumeRefusesDevicesTheSessionLacks(t *testing.T) {
	withMic := &session{start: protocol.Start{Mic: true}}
	if err := withMic.takeResume(protocol.Resume{ID: "ab12", Mic: true}); err != nil {
		t.Fatalf("a session with a microphone refused --mic: %v", err)
	}
	for _, tc := range []struct {
		name string
		s    *session
		r    protocol.Resume
		want []string
	}{
		{"camera", withMic, protocol.Resume{ID: "ab12", Mic: true, Camera: true},
			[]string{"session ab12 started without a camera", "resume it without --camera,"}},
		{"both", &session{}, protocol.Resume{ID: "cd34", Mic: true, Camera: true},
			[]string{"without a microphone or a camera", "resume it without --mic and --camera,", "robin end"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.s.takeResume(tc.r)
			for _, want := range tc.want {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %v, want it to say %q", err, want)
				}
			}
		})
	}
}

func TestTakeResumeRefusesPrivateOnASpawnedDesktop(t *testing.T) {
	spawned := &session{}
	if err := spawned.takeResume(protocol.Resume{ID: "ab12"}); err != nil {
		t.Fatalf("a plain resume: %v", err)
	}
	for _, r := range []protocol.Resume{{Curtain: true}, {Silent: true}} {
		if err := spawned.takeResume(r); err == nil || !strings.Contains(err.Error(), "for a desktop you attach to") {
			t.Errorf("resume %+v of a spawned desktop: %v", r, err)
		}
	}
}

func TestPrivateRequireTurnsOnWhatTheResumeAsks(t *testing.T) {
	r, err := newPrivate(t, protocol.Start{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.p.require(true, false); err != nil {
		t.Fatal(err)
	}
	if st := r.p.snapshot(); !st.Curtain.On || st.Silent.On || !r.curtain.isDrawn() {
		t.Fatalf("after --curtain: state %+v, drawn %v; want the curtain alone", st, r.curtain.isDrawn())
	}
	// Asked again with the silent room, the curtain stays as it is.
	if err := r.p.require(true, true); err != nil {
		t.Fatal(err)
	}
	if st := r.p.snapshot(); !st.Curtain.On || !st.Silent.On || r.capture.followed() != "fake.monitor" {
		t.Fatalf("after --curtain --silent: state %+v, capture followed %q", st, r.capture.followed())
	}
	// What the resume turned on is the page's to turn off.
	for len(r.sent) > 0 {
		<-r.sent
	}
	if st := r.set(protocol.ControlCurtain, false); st.Curtain.On || r.curtain.isDrawn() {
		t.Fatalf("the page lifted the curtain: state %+v, drawn %v", st, r.curtain.isDrawn())
	}
}

func TestPrivateRequireRefusesAndPutsBack(t *testing.T) {
	r, err := newPrivate(t, protocol.Start{})
	if err != nil {
		t.Fatal(err)
	}
	r.rooms.err = errors.New("no null sink")
	err = r.p.require(true, true)
	if err == nil || err.Error() != "--silent: no null sink" {
		t.Fatalf("a silent room that cannot be: %v", err)
	}
	if st := r.p.snapshot(); st.Curtain.On || st.Silent.On || r.curtain.isDrawn() {
		t.Fatalf("after the refusal: state %+v, drawn %v; want both off again", st, r.curtain.isDrawn())
	}

	unsupported, err := newPrivate(t, protocol.Start{}, func(d *openedDesktop, _ *Options) { d.openCurtain = nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := unsupported.p.require(true, false); err == nil || err.Error() != "--curtain: this desktop cannot be darkened" {
		t.Fatalf("a desktop without a curtain: %v", err)
	}

	r.p.close()
	if err := r.p.require(true, false); err == nil || !strings.Contains(err.Error(), "--curtain") {
		t.Fatalf("a closed private session: %v", err)
	}
}

// TestLingerGivesTheResumeWhatItAsks: a kept attached desktop resumed with
// --mic it never had is refused and goes on waiting; resumed with
// --curtain, it draws the curtain before Ready, which says so.
func TestLingerGivesTheResumeWhatItAsks(t *testing.T) {
	rig, err := newPrivate(t, protocol.Start{})
	if err != nil {
		t.Fatal(err)
	}
	s := &session{log: slog.New(slog.NewTextHandler(io.Discard, nil)), private: rig.p, conn: newController(nil)}
	f := &feed{conns: make(chan *protocol.Conn, 2), changes: make(chan protocol.Session, 4)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ready := &protocol.Ready{Version: protocol.Version, ID: "ab12", Mode: protocol.ModeAttach}

	refused, refusedCLI := pair(t)
	resumed, resumedCLI := pair(t)
	f.conns <- refused
	f.conns <- resumed
	got := make(chan *protocol.Conn, 1)
	go func() {
		c, err := s.linger(ctx, f, ready, nil, time.Now().Add(time.Minute))
		if err != nil {
			t.Errorf("linger: %v", err)
		}
		got <- c
	}()

	go refusedCLI.Send(protocol.Message{Type: protocol.TypeResume, Resume: &protocol.Resume{Version: protocol.Version, ID: "ab12", Mic: true}})
	if m, err := refusedCLI.Recv(ctx); err != nil || m.Type != protocol.TypeError || !strings.Contains(m.Error, "--mic") {
		t.Fatalf("the CLI that asked for --mic got %+v, %v", m, err)
	}
	go resumedCLI.Send(protocol.Message{Type: protocol.TypeResume, Resume: &protocol.Resume{Version: protocol.Version, ID: "ab12", Curtain: true}})
	m, err := resumedCLI.Recv(ctx)
	if err != nil || m.Type != protocol.TypeReady || !m.Ready.Resumed || m.Ready.Private == nil || !m.Ready.Private.Curtain.On {
		t.Fatalf("the CLI that asked for --curtain got %+v, %v", m, err)
	}
	if !rig.curtain.isDrawn() {
		t.Error("Ready says the curtain is on, and it is not drawn")
	}
	if c := <-got; c != resumed {
		t.Errorf("linger resumed %p, want the second controller %p", c, resumed)
	}
}
