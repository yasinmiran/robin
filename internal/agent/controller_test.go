//go:build linux

package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yasinmiran/robin/internal/protocol"
)

func TestStdioYieldsOneController(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	cs := Stdio(r, w)
	ctx := context.Background()
	conn, err := cs.Next(ctx)
	if err != nil || conn == nil {
		t.Fatalf("first Next: %v, %v", conn, err)
	}
	conn.Close()
	if _, err := cs.Next(ctx); !errors.Is(err, ErrNoControllers) {
		t.Fatalf("second Next: %v, want ErrNoControllers", err)
	}
	cs.Changed(protocol.Session{}) // nothing to record
}

// pair is a controller connection and the CLI's end of it.
func pair(t *testing.T) (agent, cli *protocol.Conn) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	agent = protocol.NewConn(inR, outW, 0, 0)
	cli = protocol.NewConn(outR, inW, 0, 0)
	t.Cleanup(func() {
		inW.Close()
		outW.Close()
	})
	return agent, cli
}

func TestAwaitResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name string
		m    protocol.Message
		want string
	}{
		{"start instead", protocol.Message{Type: protocol.TypeStart, Start: &protocol.Start{Version: protocol.Version}}, `expected resume, got "start"`},
		{"no body", protocol.Message{Type: protocol.TypeResume}, `expected resume`},
		{"wrong version", protocol.Message{Type: protocol.TypeResume, Resume: &protocol.Resume{Version: protocol.Version + 1, ID: "ab12"}},
			"the CLI speaks protocol"},
		{"wrong id", protocol.Message{Type: protocol.TypeResume, Resume: &protocol.Resume{Version: protocol.Version, ID: "other"}}, `no session "other" here`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent, cli := pair(t)
			go cli.Send(tc.m)
			err := awaitResume(ctx, agent, "ab12")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			// The CLI hears why.
			if m, err := cli.Recv(ctx); err != nil || m.Type != protocol.TypeError || !strings.Contains(m.Error, tc.want) {
				t.Fatalf("CLI got %+v, %v", m, err)
			}
		})
	}
	agent, cli := pair(t)
	go cli.Send(protocol.Message{Type: protocol.TypeResume, Resume: &protocol.Resume{Version: protocol.Version, ID: "ab12"}})
	if err := awaitResume(ctx, agent, "ab12"); err != nil {
		t.Fatal(err)
	}
}

func TestAwaitResumeGivesUpOnAClosedController(t *testing.T) {
	inR, inW := io.Pipe()
	_, outW := io.Pipe()
	agent := protocol.NewConn(inR, outW, 0, 0)
	inW.Close()
	if err := awaitResume(context.Background(), agent, "x"); !errors.Is(err, io.EOF) {
		t.Fatalf("got %v, want EOF", err)
	}
}

// deadPID is a pid no process has: a child that has already been reaped.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func TestSweepRunRootRemovesOnlyTheDead(t *testing.T) {
	root := t.TempDir()
	dead := deadPID(t)
	mk := func(name string) string {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	deadByName := mk(strconv.Itoa(dead))
	liveByName := mk(strconv.Itoa(os.Getpid()))
	deadByFile := mk("ab12")
	WriteSession(deadByFile, protocol.Session{ID: "ab12", PID: dead})
	liveByFile := mk("cd34")
	WriteSession(liveByFile, protocol.Session{ID: "cd34", PID: os.Getppid()})
	unknown := mk("ef56") // no pid anywhere: a daemon still starting
	keep := mk("0011")
	WriteSession(keep, protocol.Session{ID: "0011", PID: dead})
	os.WriteFile(filepath.Join(root, "stray"), nil, 0o600)

	sweepRunRoot(root, keep)

	for _, gone := range []string{deadByName, deadByFile} {
		if _, err := os.Lstat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the sweep", filepath.Base(gone))
		}
	}
	for _, kept := range []string{liveByName, liveByFile, unknown, keep, filepath.Join(root, "stray")} {
		if _, err := os.Lstat(kept); err != nil {
			t.Errorf("%s was swept: %v", filepath.Base(kept), err)
		}
	}
}

func TestPrepareRunDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "run")
	dir, err := prepareRunDir(Options{RunRoot: root})
	if err != nil || dir != filepath.Join(root, strconv.Itoa(os.Getpid())) {
		t.Fatalf("by pid: %q, %v", dir, err)
	}
	if fi, _ := os.Lstat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("mode %o", fi.Mode().Perm())
	}

	own := filepath.Join(root, "ab12")
	os.Mkdir(own, 0o700)
	stale := filepath.Join(root, strconv.Itoa(deadPID(t)))
	os.Mkdir(stale, 0o700)
	got, err := prepareRunDir(Options{RunRoot: root, RunDir: own})
	if err != nil || got != own {
		t.Fatalf("own dir: %q, %v", got, err)
	}
	if _, err := os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Error("a dead session's directory survived")
	}
	if _, err := os.Lstat(own); err != nil {
		t.Error("the caller's own directory was swept")
	}
}

func TestSessionFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	want := protocol.Session{
		ID: "ab12", PID: 7, Agent: "robin-agent-0123", Build: "v1", Mode: protocol.ModeSpawn, Display: ":7",
		Desktop: "openbox + xterm", Keep: 3600, Started: until.Add(-time.Hour), Until: &until,
	}
	if err := WriteSession(dir, want); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, SessionFile))
	if strings.Count(strings.TrimSpace(string(b)), "\n") != 0 {
		t.Errorf("session.json is not one line:\n%s", b)
	}
	got, err := ReadSession(dir)
	if err != nil || got.ID != want.ID || got.Agent != want.Agent || !got.Until.Equal(until) || !got.Started.Equal(want.Started) {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
	if _, err := ReadSession(t.TempDir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ReadSession of nothing: %v", err)
	}
	if sessionPID(t.TempDir()) != 0 {
		t.Error("sessionPID of nothing is not 0")
	}
}
