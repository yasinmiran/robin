package gui

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/probe"
	"github.com/yasinmiran/robin/internal/protocol"
	"github.com/yasinmiran/robin/internal/tail"
)

func keptReport(sessions ...protocol.Session) *probe.Report {
	return &probe.Report{Kept: sessions, Commands: map[string]string{}}
}

func spawned(id, agent string) protocol.Session {
	return protocol.Session{ID: id, Agent: agent, Build: "v1", Mode: protocol.ModeSpawn, Display: ":7", Keep: 28800, Started: time.Now()}
}

func TestPickSession(t *testing.T) {
	const ours = "robin-agent-0123456789abcdef"
	one := keptReport(spawned("ab12", ours))
	two := keptReport(spawned("ab12", ours), spawned("cd34", ours))
	attached := keptReport(protocol.Session{ID: "at01", Agent: ours, Mode: protocol.ModeAttach})
	other := keptReport(spawned("ab12", "robin-agent-fedcba9876543210"))
	for _, tc := range []struct {
		name string
		o    Options
		r    *probe.Report
		want string
		err  string
	}{
		{"nothing kept", Options{}, keptReport(), "", ""},
		{"one kept", Options{}, one, "ab12", ""},
		{"one kept, new asked", Options{New: true}, one, "", ""},
		{"two kept", Options{}, two, "", "keeps 2 sessions"},
		{"two kept, id given", Options{Resume: "cd34"}, two, "cd34", ""},
		{"id not kept", Options{Resume: "zz99"}, two, "", "no kept session zz99"},
		{"attach never resumes", Options{Attach: ":0"}, one, "", ""},
		{"attached session is not a candidate", Options{}, attached, "", ""},
		{"attached session by id", Options{Resume: "at01"}, attached, "at01", ""},
		{"other agent", Options{}, other, "", "runs agent robin-agent-fedcba9876543210"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.o.Dest = "box"
			got, err := pickSession(tc.o, tc.r, ours)
			if got != tc.want || (err != nil) != (tc.err != "") || (err != nil && !strings.Contains(err.Error(), tc.err)) {
				t.Errorf("pickSession = %q, %v; want %q, %q", got, err, tc.want, tc.err)
			}
		})
	}
}

func TestKeepWindow(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    Options
		want time.Duration
	}{
		{"spawned by default", Options{}, protocol.DefaultKeep},
		{"attached by default", Options{Attach: ":0"}, 0},
		{"asked for", Options{Keep: time.Hour, Attach: ":0"}, time.Hour},
		{"none asked", Options{Keep: KeepNone}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := keepWindow(tc.o); got != tc.want {
				t.Errorf("keepWindow = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestChooseToEnd(t *testing.T) {
	a, b := spawned("ab12", "x"), spawned("cd34", "x")
	if s, err := chooseToEnd("box", []protocol.Session{a}, ""); err != nil || s.ID != "ab12" {
		t.Errorf("one kept: %+v, %v", s, err)
	}
	if s, err := chooseToEnd("box", []protocol.Session{a, b}, "cd34"); err != nil || s.ID != "cd34" {
		t.Errorf("by id: %+v, %v", s, err)
	}
	for name, tc := range map[string]struct {
		kept []protocol.Session
		id   string
		want string
	}{
		"none":    {nil, "", "no kept sessions"},
		"two":     {[]protocol.Session{a, b}, "", "say which"},
		"unknown": {[]protocol.Session{a}, "zz", "no kept session zz"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := chooseToEnd("box", tc.kept, tc.id); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%v, want %q", err, tc.want)
			}
		})
	}
}

func TestServeArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		a    serveArgs
		want string
	}{
		{"bare", serveArgs{}, "/a serve"},
		{"kept under setsid", serveArgs{verbose: true, keep: 8 * time.Hour, detach: protocol.DetachSetsid}, "/a serve -v -keep 8h0m0s"},
		{"kept under systemd-run", serveArgs{keep: time.Minute, detach: protocol.DetachSystemdRun}, "/a serve -keep 1m0s -detach systemd-run"},
		{"resume", serveArgs{verbose: true, keep: time.Hour, resume: "ab12"}, "/a serve -v -resume ab12"},
		{"no keep needs no detach", serveArgs{detach: protocol.DetachSystemdRun}, "/a serve"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Join(tc.a.list("/a"), " "); got != tc.want {
				t.Errorf("%+v: %q, want %q", tc.a, got, tc.want)
			}
		})
	}
}

func TestDetachModeAndKeepNote(t *testing.T) {
	plain := &probe.Report{Commands: map[string]string{}}
	kills := &probe.Report{KillUserProcesses: "yes", Commands: map[string]string{"systemd-run": "/usr/bin/systemd-run"}}
	killsNoSystemd := &probe.Report{KillUserProcesses: "yes", Commands: map[string]string{}}
	lingers := &probe.Report{KillUserProcesses: "yes", Linger: true, Commands: map[string]string{"systemd-run": "/usr/bin/systemd-run"}}
	if detachMode(plain) != protocol.DetachSetsid || detachMode(killsNoSystemd) != protocol.DetachSetsid || detachMode(kills) != protocol.DetachSystemdRun || detachMode(lingers) != protocol.DetachSystemdRun {
		t.Error("detachMode picks wrongly")
	}
	if keepNote(plain) != "" || keepNote(lingers) != "" {
		t.Error("keepNote warns where nothing is wrong")
	}
	if n := keepNote(kills); !strings.Contains(n, "enable-linger") {
		t.Errorf("keepNote without linger: %q", n)
	}
	if n := keepNote(killsNoSystemd); !strings.Contains(n, "systemd-run is missing") {
		t.Errorf("keepNote without systemd-run: %q", n)
	}
}

func TestSessionNotes(t *testing.T) {
	r := &protocol.Ready{Display: ":7", Mode: protocol.ModeSpawn, ID: "ab12", Keep: 28800}
	if n := sessionNote("box", r); n != ":7 on box; kept for 8h0m0s if the connection drops" {
		t.Errorf("spawned: %q", n)
	}
	r.Resumed = true
	if n := sessionNote("box", r); !strings.HasPrefix(n, "resumed session ab12, :7 on box") {
		t.Errorf("resumed: %q", n)
	}
	a := &protocol.Ready{Display: ":0", Mode: protocol.ModeAttach}
	if n := sessionNote("box", a); n != "attached to :0 on box" {
		t.Errorf("attached: %q", n)
	}
	until := time.Date(2026, 10, 24, 18, 0, 0, 0, time.Local)
	s := protocol.Session{ID: "ab12", Mode: "spawn", Display: ":7", Desktop: "openbox + xterm", Started: until.Add(-8 * time.Hour), Until: &until}
	if d := describeSession(s); !strings.Contains(d, "ab12  spawn  :7 (openbox + xterm)  started Oct 24 10:00  kept until 6:00PM") {
		t.Errorf("describeSession: %q", d)
	}
	s.Connected, s.Until = true, nil
	if d := describeSession(s); !strings.HasSuffix(d, "connected") {
		t.Errorf("connected: %q", d)
	}
	if l := keptList([]protocol.Session{s}); !strings.HasPrefix(l, "\n  ab12") {
		t.Errorf("keptList: %q", l)
	}
}

// fakeHostAgent is the agent's end of the proxy's pipes. exit stands for
// the proxy process going away.
type fakeHostAgent struct {
	conn  *protocol.Conn
	outW  io.Closer
	exit  func()
	calls chan protocol.Message
}

func agentOverPipes(t *testing.T) (*agentProc, *fakeHostAgent) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	exited := make(chan struct{})
	var exitOnce sync.Once
	exit := func() { exitOnce.Do(func() { close(exited) }) }
	a := &agentProc{
		conn:  protocol.NewConn(outR, inW, 0, 0),
		stdin: inW, exited: exited, log: &tail.Buffer{Max: 1024},
	}
	f := &fakeHostAgent{conn: protocol.NewConn(inR, outW, 0, 0), outW: outW, exit: exit, calls: make(chan protocol.Message, 8)}
	go func() {
		for {
			m, err := f.conn.Recv(context.Background())
			if err != nil {
				return
			}
			f.calls <- m
		}
	}()
	t.Cleanup(func() {
		inW.Close()
		outW.Close()
		exit()
	})
	return a, f
}

func TestAgentResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name  string
		reply protocol.Message
		final bool
		err   string
	}{
		{"resumed", protocol.Message{Type: protocol.TypeReady, Ready: &protocol.Ready{Version: protocol.Version, ID: "ab12", Resumed: true}}, false, ""},
		{"wrong session", protocol.Message{Type: protocol.TypeReady, Ready: &protocol.Ready{Version: protocol.Version, ID: "zz99", Resumed: true}}, true, `resumed session "zz99"`},
		{"gone", protocol.Message{Type: protocol.TypeError, Error: `no session "ab12" here`}, true, "no session"},
		{"busy", protocol.Message{Type: protocol.TypeError, Error: "another CLI is connected to this session", Code: protocol.CodeBusy}, false, "another CLI"},
		{"busy by text alone is final", protocol.Message{Type: protocol.TypeError, Error: "another CLI is connected to this session"}, true, "another CLI"},
		{"other protocol", protocol.Message{Type: protocol.TypeReady, Ready: &protocol.Ready{Version: protocol.Version + 1}}, true, "speaks protocol"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, f := agentOverPipes(t)
			go func() {
				if m := <-f.calls; m.Type == protocol.TypeResume && m.Resume.ID == "ab12" && m.Resume.Version == protocol.Version {
					f.conn.Send(tc.reply)
				}
			}()
			ready, err := a.resume(ctx, protocol.Resume{ID: "ab12"})
			var final *finalError
			if errors.As(err, &final) != tc.final || (err != nil) != (tc.err != "") || (err != nil && !strings.Contains(err.Error(), tc.err)) {
				t.Fatalf("resume: %+v, %v (final %v); want err %q final %v", ready, err, errors.As(err, &final), tc.err, tc.final)
			}
		})
	}
}

// TestResumeAsksWhatTheUserNames: a new robin gui's Resume carries the
// switches and devices it names, and a redial's carries none of them.
func TestResumeAsksWhatTheUserNames(t *testing.T) {
	o := Options{Curtain: true, Silent: true, Mic: true, Camera: true}
	if got, want := resumeMessage(o, "ab12"), (protocol.Resume{ID: "ab12", Curtain: true, Silent: true, Mic: true, Camera: true}); got != want {
		t.Errorf("resume message %+v, want %+v", got, want)
	}
	if got := resumeMessage(Options{}, "ab12"); got != (protocol.Resume{ID: "ab12"}) {
		t.Errorf("resume message %+v asks for what nobody named", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, f := agentOverPipes(t)
	sent := make(chan protocol.Resume, 1)
	go func() {
		m := <-f.calls
		sent <- *m.Resume
		f.conn.Send(protocol.Message{Type: protocol.TypeError, Error: "--curtain: this desktop cannot be darkened"})
	}()
	_, err := a.resume(ctx, protocol.Resume{ID: "ab12", Curtain: true})
	var final *finalError
	if !errors.As(err, &final) || !strings.Contains(err.Error(), "--curtain: this desktop cannot be darkened") {
		t.Fatalf("a refused resume: %v, want the agent's reason, final", err)
	}
	if got := <-sent; got != (protocol.Resume{Version: protocol.Version, ID: "ab12", Curtain: true}) {
		t.Errorf("the agent was sent %+v", got)
	}
}

// TestCheckSpawn: a host that lacks what spawning takes refuses a new
// desktop, not an attach and not a resume by id, which may be attached.
func TestCheckSpawn(t *testing.T) {
	bare := &probe.Report{}
	if err := checkSpawn(Options{Dest: "box"}, bare); err == nil || !strings.Contains(err.Error(), "box cannot run a headless desktop") {
		t.Errorf("a new desktop on a bare host: %v", err)
	}
	for _, o := range []Options{{Attach: ":0"}, {Resume: "ab12"}} {
		if err := checkSpawn(o, bare); err != nil {
			t.Errorf("%+v on a bare host: %v", o, err)
		}
	}
}

func TestAgentBye(t *testing.T) {
	a, f := agentOverPipes(t)
	go func() {
		if m := <-f.calls; m.Type == protocol.TypeBye {
			f.conn.Send(protocol.Message{Type: protocol.TypeEnded, Error: "the CLI ended the session"})
			f.outW.Close()
		}
	}()
	if !a.bye() {
		t.Fatal("bye was not confirmed")
	}

	// A proxy already gone confirms nothing.
	dead, deadHost := agentOverPipes(t)
	deadHost.outW.Close()
	dead.stdin.Close()
	if dead.bye() {
		t.Fatal("bye confirmed on a dead connection")
	}
	// A proxy whose pipe took the bye but which then dies is not waited
	// out: the wait ends with the process.
	dying, dyingHost := agentOverPipes(t)
	go func() {
		<-dyingHost.calls
		dyingHost.exit()
	}()
	start := time.Now()
	if dying.bye() || time.Since(start) > 3*time.Second {
		t.Fatalf("bye on a dying proxy: confirmed or waited %s", time.Since(start))
	}
	var none *agentProc
	if none.bye() {
		t.Fatal("a missing agent confirmed bye")
	}
	none.stop()
}

func TestFinalError(t *testing.T) {
	inner := errors.New("gone")
	err := error(&finalError{inner})
	if !errors.Is(err, inner) || err.Error() != "gone" {
		t.Errorf("finalError: %v", err)
	}
	if (&agentError{text: "x"}).Error() != "agent: x" {
		t.Error("agentError text")
	}
	if keepDuration(90) != "1m30s" {
		t.Error("keepDuration")
	}
}
