//go:build linux

package keeper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These variables make the test binary a keeper, or an agent that runs one,
// for the tests that need real processes.
const (
	serveEnv = "ROBIN_KEEPER_TEST_SERVE" // a directory: serve, logging undos there
	agentEnv = "ROBIN_KEEPER_TEST_AGENT" // a directory: hold steps, then wait to be killed
)

func TestMain(m *testing.M) {
	if dir := os.Getenv(serveEnv); dir != "" {
		serveForTest(dir)
		return
	}
	if dir := os.Getenv(agentEnv); dir != "" {
		agentForTest(dir)
		return
	}
	os.Exit(m.Run())
}

// serveForTest is a keeper whose one kind of undo appends its data to
// dir/undone. It writes its pid to dir/pid first.
func serveForTest(dir string) {
	signal.Ignore(syscall.SIGPIPE)
	os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	Serve(ctx, os.Stdin, Undo{"note": func(_ context.Context, data json.RawMessage) error {
		f, err := os.OpenFile(filepath.Join(dir, "undone"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.Write(append(data, '\n'))
		return err
	}}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
}

// agentForTest stands in for an agent: it holds two changes with a keeper,
// says so on stdout, and waits to be killed.
func agentForTest(dir string) {
	c := New(keeperArgv(), []string{serveEnv + "=" + dir, agentEnv + "="})
	for _, id := range []string{"a", "b"} {
		if err := c.Hold(id, "note", "undo "+id); err != nil {
			os.Stdout.WriteString("error: " + err.Error() + "\n")
			os.Exit(1)
		}
	}
	os.Stdout.WriteString("held\n")
	select {}
}

func keeperArgv() []string {
	exe, _ := os.Executable()
	return []string{exe, "-test.run=^$"}
}

func newClient(t *testing.T) (*Client, string) {
	t.Helper()
	dir := t.TempDir()
	c := New(keeperArgv(), []string{serveEnv + "=" + dir})
	t.Cleanup(func() { c.Close() })
	return c, dir
}

func undone(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "undone"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(strings.ReplaceAll(string(b), `"`, " "))
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// record is an Undo that notes what it undid, in order.
func record(got *[]string) Undo {
	return Undo{"note": func(_ context.Context, data json.RawMessage) error {
		var s string
		json.Unmarshal(data, &s)
		*got = append(*got, s)
		return nil
	}}
}

func lines(steps ...Step) io.Reader {
	var b bytes.Buffer
	for _, s := range steps {
		line, _ := json.Marshal(s)
		b.Write(append(line, '\n'))
	}
	return &b
}

func note(id, data string) Step {
	raw, _ := json.Marshal(data)
	return Step{ID: id, Kind: "note", Data: raw}
}

func TestServeUndoesWhatIsStillHeldNewestFirst(t *testing.T) {
	var got []string
	Serve(context.Background(), lines(
		note("a", "a1"), note("b", "b1"), note("c", "c1"),
		Step{ID: "b"},      // the agent undid b itself
		note("a", "a2"),    // a changed again, and keeps its place
		Step{ID: "absent"}, // dropping what is not held is harmless
	), record(&got), quiet())
	if want := []string{"c1", "a2"}; !slices.Equal(got, want) {
		t.Fatalf("undid %q, want %q", got, want)
	}
}

func TestServeGoesOnPastBadSteps(t *testing.T) {
	var got []string
	undo := record(&got)
	undo["broken"] = func(context.Context, json.RawMessage) error { return errors.New("no X server") }
	in := io.MultiReader(
		strings.NewReader("not json\n{\"kind\":\"note\"}\n"),
		lines(note("a", "a1"), Step{ID: "x", Kind: "unknown"}, Step{ID: "y", Kind: "broken"}, note("b", "b1")),
	)
	Serve(context.Background(), in, undo, quiet())
	if want := []string{"b1", "a1"}; !slices.Equal(got, want) {
		t.Fatalf("undid %q, want %q", got, want)
	}
}

// A signal ends the keeper even while the agent still holds its stdin
// open, after reading what the agent had just written.
func TestServeUndoesOnSignalAfterDraining(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	var got []string
	done := make(chan struct{})
	go func() {
		Serve(ctx, r, record(&got), quiet())
		close(done)
	}()
	line, _ := json.Marshal(note("a", "a1"))
	w.Write(append(line, '\n'))
	cancel()
	line, _ = json.Marshal(note("b", "b1"))
	w.Write(append(line, '\n'))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not end after its context did")
	}
	if want := []string{"b1", "a1"}; !slices.Equal(got, want) {
		t.Fatalf("undid %q, want %q", got, want)
	}
}

func TestServeBoundsEachUndo(t *testing.T) {
	defer func(d time.Duration) { undoTimeout = d }(undoTimeout)
	undoTimeout = 50 * time.Millisecond
	var got []string
	undo := record(&got)
	undo["hang"] = func(ctx context.Context, _ json.RawMessage) error {
		<-ctx.Done()
		return ctx.Err()
	}
	start := time.Now()
	Serve(context.Background(), lines(note("a", "a1"), Step{ID: "h", Kind: "hang"}), undo, quiet())
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Serve took %v with a hung undo", d)
	}
	if !slices.Equal(got, []string{"a1"}) {
		t.Errorf("undid %q after a hung undo, want a1", got)
	}
}

func TestClientStartsNothingUntilItHolds(t *testing.T) {
	c, dir := newClient(t)
	if err := c.Drop("a"); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pid")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a keeper ran for a session that held nothing (%v)", err)
	}
	if err := c.Hold("a", "note", "x"); err == nil {
		t.Error("Hold after Close succeeded")
	}
}

func TestClientCloseUndoesWhatIsHeld(t *testing.T) {
	c, dir := newClient(t)
	for _, step := range []struct{ id, data string }{{"a", "a1"}, {"b", "b1"}, {"a", "a2"}} {
		if err := c.Hold(step.id, "note", step.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Drop("b"); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if got := undone(t, dir); !slices.Equal(got, []string{"a2"}) {
		t.Fatalf("undid %q, want a2", got)
	}
	checkExited(t, dir)
}

// A keeper that died is started again with everything still held.
func TestClientReplacesADeadKeeper(t *testing.T) {
	c, dir := newClient(t)
	if err := c.Hold("a", "note", "a1"); err != nil {
		t.Fatal(err)
	}
	first := pidOf(t, dir)
	syscall.Kill(first, syscall.SIGKILL)
	waitExited(t, first)
	os.Remove(filepath.Join(dir, "pid"))
	if err := c.Hold("b", "note", "b1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if got := undone(t, dir); !slices.Equal(got, []string{"b1", "a1"}) {
		t.Fatalf("the second keeper undid %q, want b1 then a1", got)
	}
}

func TestClientWithoutAKeeperRefusesToHold(t *testing.T) {
	c := New(nil, nil)
	if err := c.Hold("a", "note", "x"); err == nil {
		t.Fatal("Hold succeeded with no keeper to run")
	}
	c.Close()
	c = New([]string{filepath.Join(t.TempDir(), "missing")}, nil)
	if err := c.Hold("a", "note", "x"); err == nil {
		t.Fatal("Hold succeeded with a keeper that cannot start")
	}
	if err := c.Hold("a", "note", make(chan int)); err == nil {
		t.Fatal("Hold took data it cannot send")
	}
}

// The point of the keeper: an agent killed with SIGKILL, which runs none of
// its own cleanup, still has its changes undone.
func TestKeeperUndoesAfterTheAgentIsKilled(t *testing.T) {
	dir := t.TempDir()
	exe, _ := os.Executable()
	agent := exec.Command(exe, "-test.run=^$")
	agent.Env = append(os.Environ(), agentEnv+"="+dir)
	agent.Stderr = os.Stderr
	out, err := agent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	defer agent.Process.Kill()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "held\n" {
		t.Fatalf("agent said %q, %v", line, err)
	}
	pid := pidOf(t, dir)
	agent.Process.Kill()
	agent.Wait()
	waitExited(t, pid)
	if got := undone(t, dir); !slices.Equal(got, []string{"undo", "b", "undo", "a"}) {
		t.Fatalf("undid %q after the agent died, want b then a", got)
	}
}

func pidOf(t *testing.T, dir string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(filepath.Join(dir, "pid"))
		if err == nil {
			pid, _ := strconv.Atoi(string(b))
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatal("the keeper never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitExited waits for pid to be gone; a keeper whose parent died is reaped
// by init, one of ours by its Client.
func waitExited(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if err != nil || strings.Contains(string(b), ") Z ") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("keeper %d still running", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func checkExited(t *testing.T, dir string) {
	t.Helper()
	waitExited(t, pidOf(t, dir))
}
