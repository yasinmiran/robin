//go:build linux

package gstsink

import (
	"bufio"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// helperEnv makes the test binary act as an agent that starts a sink and
// waits to be killed; see TestChildDiesWithTheAgent.
const helperEnv = "ROBIN_GSTSINK_TEST_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != "" {
		s, err := Start(nil, Options{Name: "helper"})
		if err != nil {
			os.Stdout.WriteString("error: " + err.Error() + "\n")
			os.Exit(1)
		}
		os.Stdout.WriteString("ready " + strconv.Itoa(s.cur.cmd.Process.Pid) + "\n")
		select {}
	}
	os.Exit(m.Run())
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeGst puts a gst-launch-1.0 on PATH that runs script with sh, in a
// directory the script can use as $D.
func fakeGst(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	body := "#!/bin/sh\nD=" + dir + "\n" + script + "\n"
	if err := os.WriteFile(filepath.Join(dir, "gst-launch-1.0"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return dir
}

func fast(t *testing.T) {
	t.Helper()
	g, r, s := startGrace, restartDelay, stopGrace
	startGrace, restartDelay, stopGrace = 100*time.Millisecond, 20*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { startGrace, restartDelay, stopGrace = g, r, s })
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWriteReachesThePipeline(t *testing.T) {
	fast(t)
	dir := fakeGst(t, `echo "$@" >"$D/args"; echo "$EXTRA" >"$D/env"; exec cat >"$D/in"`)
	s, err := Start([]string{"fdsrc", "fd=0", "!", "fakesink"}, Options{Name: "test", Env: []string{"EXTRA=yes"}, Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"one ", "two"} {
		if err := s.Write([]byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	in, _ := os.ReadFile(filepath.Join(dir, "in"))
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	env, _ := os.ReadFile(filepath.Join(dir, "env"))
	if string(in) != "one two" || strings.TrimSpace(string(args)) != "fdsrc fd=0 ! fakesink" || strings.TrimSpace(string(env)) != "yes" {
		t.Errorf("the pipeline got %q, args %q, env %q", in, args, env)
	}
	if err := s.Write([]byte("late")); err != ErrClosed {
		t.Errorf("Write after Close = %v, want ErrClosed", err)
	}
	if s.Close() != nil {
		t.Error("a second Close failed")
	}
}

func TestStartReportsWhatGstSaid(t *testing.T) {
	fast(t)
	fakeGst(t, `echo 'WARNING: erroneous pipeline: no element "pipewiresink"' >&2; exit 1`)
	_, err := Start(nil, Options{Name: "camera", Log: quiet()})
	if err == nil || !strings.Contains(err.Error(), `no element "pipewiresink"`) || !strings.HasPrefix(err.Error(), "camera: ") {
		t.Fatalf("Start = %v, want gst's complaint", err)
	}
}

func TestStartWithoutGst(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Start(nil, Options{Name: "mic", Log: quiet()})
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("Start = %v, want gst-launch-1.0 not installed", err)
	}
}

// TestAChildThatExitsIsReplaced: pipewiresink quits when its last reader
// goes; the next child takes the writes that follow.
func TestAChildThatExitsIsReplaced(t *testing.T) {
	fast(t)
	dir := fakeGst(t, `n=$(cat "$D/runs" 2>/dev/null || echo 0); n=$((n+1)); echo $n >"$D/runs"
if [ $n = 1 ]; then head -c 4 >"$D/in1"; exit 0; fi
exec cat >"$D/in$n"`)
	s, err := Start(nil, Options{Name: "test", Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a second child", func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "runs"))
		return strings.TrimSpace(string(b)) == "2"
	})
	waitFor(t, "writes to reach the second child", func() bool { return s.Write([]byte("efgh")) == nil })
	s.Close()
	if b, _ := os.ReadFile(filepath.Join(dir, "in2")); !strings.HasPrefix(string(b), "efgh") {
		t.Errorf("the second child got %q", b)
	}
	if s.Err() != nil {
		t.Errorf("Err = %v after one restart", s.Err())
	}
}

func TestASinkThatKeepsDyingGivesUp(t *testing.T) {
	fast(t)
	fakeGst(t, `sleep 0.2; echo broken >&2; exit 3`)
	s, err := Start(nil, Options{Name: "test", Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	waitFor(t, "the sink to give up", func() bool { return s.Err() != nil })
	if err := s.Write([]byte("x")); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Errorf("Write = %v, want the reason it gave up", err)
	}
}

func TestARestartThatFailsEndsTheSink(t *testing.T) {
	fast(t)
	dir := fakeGst(t, `if [ -e "$D/ran" ]; then echo 'no PipeWire' >&2; exit 1; fi; touch "$D/ran"; sleep 0.2`)
	s, err := Start(nil, Options{Name: "test", Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	waitFor(t, "the sink to give up", func() bool { return s.Err() != nil })
	if !strings.Contains(s.Err().Error(), "no PipeWire") {
		t.Errorf("Err = %v", s.Err())
	}
	if _, err := os.Stat(filepath.Join(dir, "ran")); err != nil {
		t.Fatal(err)
	}
}

func TestCloseKillsAChildThatIgnoresTheEnd(t *testing.T) {
	fast(t)
	dir := fakeGst(t, `trap '' TERM; echo $$ >"$D/pid"; exec 0<&-; while :; do sleep 0.05; done`)
	s, err := Start(nil, Options{Name: "test", Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s.Close()
	if d := time.Since(start); d < stopGrace {
		t.Errorf("Close returned after %s, before the grace", d)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "pid"))
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid == 0 || syscall.Kill(pid, 0) == nil {
		t.Errorf("child %d outlived Close", pid)
	}
}

// TestCloseDuringARestart: a Close that lands while a child is being
// replaced stops the replacement too.
func TestCloseDuringARestart(t *testing.T) {
	fast(t)
	restartDelay = 200 * time.Millisecond
	dir := fakeGst(t, `n=$(cat "$D/runs" 2>/dev/null || echo 0); n=$((n+1)); echo $n >"$D/runs"; echo $$ >"$D/pid$n"
if [ $n = 1 ]; then sleep 0.15; exit 0; fi
exec cat >/dev/null`)
	s, err := Start(nil, Options{Name: "test", Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first child to exit", func() bool { return s.Write([]byte("x")) != nil })
	s.Close()
	if b, err := os.ReadFile(filepath.Join(dir, "pid2")); err == nil {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if syscall.Kill(pid, 0) == nil {
			t.Errorf("the replacement %d outlived Close", pid)
		}
	}
}

// TestChildDiesWithTheAgent kills an agent with SIGKILL, which runs no
// cleanup, and checks the kernel took its pipeline down with it.
func TestChildDiesWithTheAgent(t *testing.T) {
	dir := fakeGst(t, `exec sleep 60`)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"=1", "PATH="+dir+":"+os.Getenv("PATH"))
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		cmd.Process.Kill()
		t.Fatalf("helper: %v", err)
	}
	f := strings.Fields(line)
	if len(f) != 2 || f[0] != "ready" {
		cmd.Process.Kill()
		t.Fatalf("helper said %q", line)
	}
	pid, _ := strconv.Atoi(f[1])
	cmd.Process.Kill()
	cmd.Wait()
	waitFor(t, "the pipeline to die with the agent", func() bool {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		return err != nil || strings.Contains(string(b), ") Z ")
	})
}

func TestQuote(t *testing.T) {
	inner := "props,media.class=Audio/Source,node.description=" + Quote(`Robin "mic" \ 1`)
	got := "stream-properties=" + Quote(inner)
	want := `stream-properties="props,media.class=Audio/Source,node.description=\"Robin \\\"mic\\\" \\\\ 1\""`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}
