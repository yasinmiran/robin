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
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// These variables make the test binary a keeper, or an agent that runs one,
// for the tests that need real processes.
const (
	serveEnv  = "ROBIN_KEEPER_TEST_SERVE"  // a directory: serve, logging undos there
	muteEnv   = "ROBIN_KEEPER_TEST_MUTE"   // read steps and never answer
	agentEnv  = "ROBIN_KEEPER_TEST_AGENT"  // a directory: hold steps, then wait to be killed
	holdEnv   = "ROBIN_KEEPER_TEST_HOLD"   // the steps that agent holds, by id
	unlinkEnv = "ROBIN_KEEPER_TEST_UNLINK" // that agent removes its own executable first
)

func TestMain(m *testing.M) {
	if dir := os.Getenv(serveEnv); dir != "" {
		serveForTest(dir)
		return
	}
	if os.Getenv(muteEnv) != "" {
		io.Copy(io.Discard, os.Stdin)
		return
	}
	if dir := os.Getenv(agentEnv); dir != "" {
		agentForTest(dir)
		return
	}
	os.Exit(m.Run())
}

// serveForTest is a keeper whose undos append their data to dir/undone. It
// writes its pid to dir/pid first. A "slow" undo notes in dir/undoing that
// it began, and takes a second.
func serveForTest(dir string) {
	os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	note := func(_ context.Context, data json.RawMessage) error {
		f, err := os.OpenFile(filepath.Join(dir, "undone"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.Write(append(data, '\n'))
		return err
	}
	Main(Undo{
		"note": note,
		"slow": func(ctx context.Context, data json.RawMessage) error {
			os.WriteFile(filepath.Join(dir, "undoing"), nil, 0o600)
			time.Sleep(time.Second)
			return note(ctx, data)
		},
	}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
}

// agentForTest stands in for an agent: it holds the changes holdEnv names
// with a keeper, says so on stdout, and waits to be killed.
func agentForTest(dir string) {
	if os.Getenv(unlinkEnv) != "" {
		exe, _ := os.Executable()
		if err := os.Remove(exe); err != nil {
			os.Stdout.WriteString("error: " + err.Error() + "\n")
			os.Exit(1)
		}
	}
	c := New(keeperArgv(), []string{serveEnv + "=" + dir, agentEnv + "=", holdEnv + "=", unlinkEnv + "="})
	for _, id := range strings.Split(os.Getenv(holdEnv), ",") {
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
	var acks bytes.Buffer
	Serve(context.Background(), lines(
		note("a", "a1"), note("b", "b1"), note("c", "c1"),
		Step{ID: "b"},      // the agent undid b itself
		note("a", "a2"),    // a changed again, and keeps its place
		Step{ID: "absent"}, // dropping what is not held is harmless
	), &acks, record(&got), quiet())
	if want := []string{"c1", "a2"}; !slices.Equal(got, want) {
		t.Fatalf("undid %q, want %q", got, want)
	}
	if acks.Len() != 7 {
		t.Errorf("Serve wrote %d acks for 6 steps, want one to say it is ready and one per step", acks.Len())
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
	Serve(context.Background(), in, io.Discard, undo, quiet())
	if want := []string{"b1", "a1"}; !slices.Equal(got, want) {
		t.Fatalf("undid %q, want %q", got, want)
	}
}

// A signal ends the keeper even while the agent still holds its stdin
// open, after reading what the agent had just written. What comes once it
// is undoing gets no ack.
func TestServeUndoesOnSignalAfterDraining(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	ackR, ackW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer ackR.Close()
	defer ackW.Close()
	ctx, cancel := context.WithCancel(context.Background())
	var got []string
	done := make(chan struct{})
	go func() {
		Serve(ctx, r, ackW, record(&got), quiet())
		close(done)
	}()
	readAck := func(what string) {
		t.Helper()
		ackR.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := ackR.Read(make([]byte, 1)); err != nil {
			t.Fatalf("no ack %s: %v", what, err)
		}
	}
	readAck("of readiness")
	line, _ := json.Marshal(note("a", "a1"))
	w.Write(append(line, '\n'))
	readAck("of a")
	cancel()
	line, _ = json.Marshal(note("b", "b1"))
	w.Write(append(line, '\n'))
	readAck("of b, written as the agent died")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not end after its context did")
	}
	if want := []string{"b1", "a1"}; !slices.Equal(got, want) {
		t.Fatalf("undid %q, want %q", got, want)
	}
	line, _ = json.Marshal(note("c", "c1"))
	w.Write(append(line, '\n'))
	ackR.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := ackR.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a keeper done undoing acked a step (%v)", err)
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
	Serve(context.Background(), lines(note("a", "a1"), Step{ID: "h", Kind: "hang"}), io.Discard, undo, quiet())
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Serve took %v with a hung undo", d)
	}
	if !slices.Equal(got, []string{"a1"}) {
		t.Errorf("undid %q after a hung undo, want a1", got)
	}
}

func TestClientStartsNothingUntilItHolds(t *testing.T) {
	c, dir := newClient(t)
	c.Drop("a")
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
	c.Drop("b")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if got := undone(t, dir); !slices.Equal(got, []string{"a2"}) {
		t.Fatalf("undid %q, want a2", got)
	}
	checkExited(t, dir)
	if err := c.Close(); err != nil {
		t.Errorf("a second Close: %v", err)
	}
}

// A keeper that died is replaced by the next Hold, which hands the new one
// everything still held. The Hold right after the kill must reach the new
// keeper however soon after it comes: the dead one cannot ack it.
func TestClientReplacesADeadKeeper(t *testing.T) {
	c, dir := newClient(t)
	if err := c.Hold("a", "note", "a1"); err != nil {
		t.Fatal(err)
	}
	syscall.Kill(pidOf(t, dir), syscall.SIGKILL)
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

// A keeper that died with nothing more to hold is replaced all the same, so
// an agent killed later still has its changes undone.
func TestClientRevivesADeadKeeper(t *testing.T) {
	dir, agent := startAgent(t, "a", false)
	first := pidOf(t, dir)
	syscall.Kill(first, syscall.SIGKILL)
	second := first
	for deadline := time.Now().Add(5 * time.Second); second == first && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		second = pidOf(t, dir)
	}
	agent.Process.Kill()
	agent.Wait()
	if second == first {
		t.Fatal("no keeper took the place of the one that died")
	}
	waitExited(t, second)
	if got := undone(t, dir); !slices.Equal(got, []string{"undo", "a"}) {
		t.Fatalf("undid %q after the agent died, want a", got)
	}
}

// A keeper that cannot be started again is reported.
func TestClientSaysWhenItLosesItsKeeper(t *testing.T) {
	c, dir := newClient(t)
	c.reviveWait = time.Millisecond
	lost := make(chan error, 1)
	c.Lost = func(err error) { lost <- err }
	if err := c.Hold("a", "note", "a1"); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.argv = nil // no keeper to start any more
	c.mu.Unlock()
	syscall.Kill(pidOf(t, dir), syscall.SIGKILL)
	select {
	case err := <-lost:
		if !strings.Contains(err.Error(), "no keeper") {
			t.Errorf("lost the keeper: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the client did not say it lost its keeper")
	}
}

// Close undoes through a new keeper what one that died had held.
func TestClientCloseAfterTheKeeperDied(t *testing.T) {
	c, dir := newClient(t)
	if err := c.Hold("a", "note", "a1"); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	p := c.p
	c.mu.Unlock()
	syscall.Kill(pidOf(t, dir), syscall.SIGKILL)
	<-p.exited
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if got := undone(t, dir); !slices.Equal(got, []string{"a1"}) {
		t.Fatalf("undid %q, want a1", got)
	}
}

// A keeper killed while it undoes is followed by one that finishes.
func TestClientCloseWhenTheKeeperDiesUndoing(t *testing.T) {
	c, dir := newClient(t)
	if err := c.Hold("a", "slow", "a1"); err != nil {
		t.Fatal(err)
	}
	first := pidOf(t, dir)
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "undoing")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the keeper never began to undo")
		}
	}
	syscall.Kill(first, syscall.SIGKILL)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Close did not return")
	}
	if got := undone(t, dir); !slices.Equal(got, []string{"a1"}) {
		t.Fatalf("undid %q, want a1 once", got)
	}
}

func TestClientWithoutAKeeperRefusesToHold(t *testing.T) {
	var zero Client
	if err := zero.Hold("a", "note", "x"); err == nil {
		t.Fatal("the zero Client held a step")
	}
	zero.Drop("a")
	if err := zero.Close(); err != nil {
		t.Fatal(err)
	}
	c := New(nil, nil)
	if err := c.Hold("a", "note", "x"); err == nil {
		t.Fatal("Hold succeeded with no keeper to run")
	}
	c.Close()
	c, _ = newClient(t)
	if err := c.Hold("a", "note", make(chan int)); err == nil {
		t.Fatal("Hold took data it cannot send")
	}
}

// A keeper that never says it is ready holds nothing, however alive it is:
// the agent dying before it can catch the signal would kill it.
func TestClientRefusesAKeeperThatNeverAnswers(t *testing.T) {
	c := New(keeperArgv(), []string{muteEnv + "=1"})
	c.ackWait = 200 * time.Millisecond
	defer c.Close()
	start := time.Now()
	if err := c.Hold("a", "note", "x"); err == nil || !strings.Contains(err.Error(), "ready") {
		t.Fatalf("Hold with a keeper that never answers: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Hold took %v to give up", d)
	}
}

// startAgent runs agentForTest holding ids, from a copy of the test binary
// when unlink has it remove that copy first.
func startAgent(t *testing.T, ids string, unlink bool) (dir string, agent *exec.Cmd) {
	t.Helper()
	dir = t.TempDir()
	exe, _ := os.Executable()
	env := []string{agentEnv + "=" + dir, holdEnv + "=" + ids}
	if unlink {
		b, err := os.ReadFile(exe)
		if err != nil {
			t.Fatal(err)
		}
		exe = filepath.Join(t.TempDir(), "agent.test")
		if err := os.WriteFile(exe, b, 0o700); err != nil {
			t.Fatal(err)
		}
		env = append(env, unlinkEnv+"=1")
	}
	agent = exec.Command(exe, "-test.run=^$")
	agent.Env = append(os.Environ(), env...)
	agent.Stderr = os.Stderr
	out, err := agent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() {
			agent.Process.Kill()
			agent.Wait()
		})
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "held\n" {
		t.Fatalf("agent said %q, %v", line, err)
	}
	return dir, agent
}

// The point of the keeper: an agent killed with SIGKILL, which runs none of
// its own cleanup, still has its changes undone.
func TestKeeperUndoesAfterTheAgentIsKilled(t *testing.T) {
	dir, agent := startAgent(t, "a,b", false)
	pid := pidOf(t, dir)
	agent.Process.Kill()
	agent.Wait()
	waitExited(t, pid)
	if got := undone(t, dir); !slices.Equal(got, []string{"undo", "b", "undo", "a"}) {
		t.Fatalf("undid %q after the agent died, want b then a", got)
	}
}

// A deploy of another version removes the agent's file while it runs; the
// keeper it starts after that is still the agent's own executable.
func TestKeeperStartsAfterTheAgentsFileIsGone(t *testing.T) {
	dir, agent := startAgent(t, "a", true)
	pid := pidOf(t, dir)
	agent.Process.Kill()
	agent.Wait()
	waitExited(t, pid)
	if got := undone(t, dir); !slices.Equal(got, []string{"undo", "a"}) {
		t.Fatalf("undid %q after the agent died, want a", got)
	}
}

func pidOf(t *testing.T, dir string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(filepath.Join(dir, "pid"))
		if pid, err := strconv.Atoi(string(b)); err == nil && pid > 0 {
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatal("the keeper never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitExited waits for pid to be gone, or to be a zombie its parent has
// yet to reap. It says nothing of whether a Client has seen it go.
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
