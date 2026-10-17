package gui

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/yasinmiran/remotejump/internal/probe"
	"github.com/yasinmiran/remotejump/internal/protocol"
	"github.com/yasinmiran/remotejump/internal/tail"
)

func TestPickAttachTakesTheActiveWaylandSession(t *testing.T) {
	desktop := func(sessions ...probe.Session) *probe.Report {
		return &probe.Report{
			Sessions: sessions, Wayland: []string{"/run/user/1000/wayland-0"}, SessionBus: "/run/user/1000/bus",
			X11: []probe.X11Display{{Display: ":0", UID: 1000}}, // Xwayland
		}
	}
	ssh := probe.Session{ID: "7", Type: "tty", Active: true, Remote: true}
	for _, tc := range []struct {
		name string
		r    *probe.Report
		want string
	}{
		{"GNOME on Wayland", desktop(ssh, probe.Session{ID: "2", Type: "wayland", Active: true}), protocol.AttachWayland},
		{"no logind", desktop(), protocol.AttachWayland},
		{"X11 session", desktop(ssh, probe.Session{ID: "2", Type: "x11", Active: true, Display: ":0"}), ":0"},
		{"nobody logged in", &probe.Report{Sessions: []probe.Session{ssh}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pickAttach(tc.r)
			if got != tc.want || (err == nil) != (tc.want != "") {
				t.Errorf("pickAttach = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestCheckWaylandSaysWhatToInstall(t *testing.T) {
	r := &probe.Report{
		Distro: probe.Distro{ID: "ubuntu"}, UID: 1000, Commands: map[string]string{},
		Wayland: []string{"/run/user/1000/wayland-0"}, SessionBus: "/run/user/1000/bus",
		GstDirs: []string{"/usr/lib/x86_64-linux-gnu/gstreamer-1.0"}, GstPlugins: []string{"pipewire", "videoconvertscale", "y4menc"},
	}
	err := checkWayland("box", r)
	if err == nil || !strings.Contains(err.Error(), "missing gst-launch-1.0") || !strings.Contains(err.Error(), "sudo apt install gstreamer1.0-tools") {
		t.Errorf("got %v", err)
	}
	r.Commands["gst-launch-1.0"] = "/usr/bin/gst-launch-1.0"
	if err := checkWayland("box", r); err != nil {
		t.Errorf("all there, but %v", err)
	}
}

// TestAwaitReadyShowsTheAgentError checks what the user reads when the agent
// gives up before Ready: its message once, under one "agent:", then its log.
func TestAwaitReadyShowsTheAgentError(t *testing.T) {
	agentR, cliW := io.Pipe()
	cliR, agentW := io.Pipe()
	agent := protocol.NewConn(agentR, agentW, 0, 0)
	defer agent.Close()
	log := &tail.Buffer{Max: 1024}
	log.Write([]byte("level=ERROR msg=\"session ended\"\n"))
	a := &agentProc{conn: protocol.NewConn(cliR, cliW, 0, 0), log: log}
	defer a.conn.Close()
	go agent.Send(protocol.Message{Type: protocol.TypeError, Error: `expected start, got "offer"`})

	_, err := a.awaitReady(context.Background(), "box", io.Discard)
	want := "agent: expected start, got \"offer\"\nagent log:\nlevel=ERROR msg=\"session ended\""
	if err == nil || err.Error() != want {
		t.Errorf("error %q, want %q", err, want)
	}
}

// TestAwaitReadyTellsTheUserAboutConsent plays an agent that waits on the
// consent dialog, then grants view only.
func TestAwaitReadyTellsTheUserAboutConsent(t *testing.T) {
	agentR, cliW := io.Pipe()
	cliR, agentW := io.Pipe()
	agent := protocol.NewConn(agentR, agentW, 0, 0)
	defer agent.Close()
	a := &agentProc{conn: protocol.NewConn(cliR, cliW, 0, 0), log: &tail.Buffer{Max: 1024}}
	defer a.conn.Close()
	go func() {
		agent.Send(protocol.Message{Type: protocol.TypeNotice, Notice: &protocol.Notice{Kind: protocol.NoticeConsent, Wait: 120}})
		agent.Send(protocol.Message{Type: protocol.TypeNotice, Notice: &protocol.Notice{Kind: "something-new"}})
		agent.Send(protocol.Message{Type: protocol.TypeNotice, Notice: &protocol.Notice{Kind: protocol.NoticeViewOnly}})
		agent.Send(protocol.Message{Type: protocol.TypeReady, Ready: &protocol.Ready{Version: protocol.Version, Mode: protocol.ModeAttach}})
	}()

	var stderr strings.Builder
	ready, err := a.awaitReady(context.Background(), hostOf("alice@build-box"), &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Mode != protocol.ModeAttach {
		t.Errorf("ready %+v", ready)
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != 2 ||
		!strings.Contains(lines[0], "approve the screen sharing request on build-box's screen") || !strings.Contains(lines[0], "2m0s") ||
		!strings.HasPrefix(lines[1], "remotejump: view only. build-box shared its screen") {
		t.Errorf("told the user:\n%s", stderr.String())
	}
}
