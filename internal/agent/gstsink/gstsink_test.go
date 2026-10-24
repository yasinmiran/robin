//go:build linux

package gstsink

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeGst is a gst-launch-1.0 stand-in. FAKE_GST picks what it does:
//
//	cat     record its arguments and XDG_RUNTIME_DIR, then append stdin
//	        to out
//	fail    complain like a pipeline that cannot be built, and exit
//	short   append FAKE_GST_BYTES of stdin to out, then exit, as gst does
//	        when the program reading its node goes away
//	brief   outlive startGrace a little, then fail the same way
//	deaf    ignore stdin closing and SIGTERM, and wait to be killed, in a
//	        child of its own so Close has a group to kill
//	lone    the same as one process, as gst-launch-1.0 is
const fakeGst = `#!/bin/sh
echo $$ >>"$FAKE_GST_DIR/pids"
case $FAKE_GST in
cat) printf '%s\n' "$@" >"$FAKE_GST_DIR/args"; printf %s "$XDG_RUNTIME_DIR" >"$FAKE_GST_DIR/runtime"; exec cat >>"$FAKE_GST_DIR/out" ;;
fail) echo 'WARNING: erroneous pipeline: no element "nosuchsink"' >&2; exit 1 ;;
short) head -c "$FAKE_GST_BYTES" >>"$FAKE_GST_DIR/out"; echo 'ERROR: PipeWire link to remote node was destroyed' >&2; exit 1 ;;
brief) sleep 0.15; echo 'ERROR: PipeWire link to remote node was destroyed' >&2; exit 1 ;;
deaf) trap '' TERM; exec 0</dev/null; sleep 1000 ;;
lone) trap '' TERM; exec sleep 1000 </dev/null ;;
esac
`

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != "" {
		runHelper()
	}
	startGrace = 100 * time.Millisecond
	restartDelay = 10 * time.Millisecond
	stopGrace = 300 * time.Millisecond
	stableAfter = time.Second
	maxQuickExits = 5
	os.Exit(m.Run())
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fake puts the stand-in first on PATH in mode and returns its directory.
func fake(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gst-launch-1.0"), []byte(fakeGst), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_GST", mode)
	t.Setenv("FAKE_GST_DIR", dir)
	return dir
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// eventually polls cond for up to two seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 2 s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func alive(t *testing.T, pid string) bool {
	t.Helper()
	p, err := strconv.Atoi(pid)
	if err != nil {
		t.Fatalf("pid %q: %v", pid, err)
	}
	return syscall.Kill(p, 0) == nil
}

func pids(t *testing.T, dir string) []string {
	return strings.Fields(read(t, filepath.Join(dir, "pids")))
}

func TestWritesReachThePipeline(t *testing.T) {
	dir := fake(t, "cat")
	s, err := Start([]string{"fdsrc", "fd=0", "!", "fakesink"}, Options{Name: "test", Env: []string{"FAKE_EXTRA=1"}, Log: quiet()})
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
	if got := read(t, filepath.Join(dir, "out")); got != "one two" {
		t.Errorf("the pipeline got %q", got)
	}
	if got := read(t, filepath.Join(dir, "args")); got != "fdsrc\nfd=0\n!\nfakesink\n" {
		t.Errorf("the pipeline ran with %q", got)
	}
	if err := s.Write([]byte("late")); err != ErrClosed {
		t.Errorf("Write after Close = %v, want ErrClosed", err)
	}
	s.Close()
	for _, pid := range pids(t, dir) {
		if alive(t, pid) {
			t.Errorf("child %s outlived Close", pid)
		}
	}
}

// TestTheChildFindsTheUsersRuntimeDir: an agent started over SSH may have
// no XDG_RUNTIME_DIR, and its pipeline still needs the user's to reach
// PipeWire. Options.Env can name another.
func TestTheChildFindsTheUsersRuntimeDir(t *testing.T) {
	for _, tc := range []struct {
		name, agent string
		env         []string
		want        string
	}{
		{"unset", "", nil, fmt.Sprintf("/run/user/%d", os.Getuid())},
		{"set", "/run/user/elsewhere", nil, "/run/user/elsewhere"},
		{"named in Env", "/run/user/elsewhere", []string{"XDG_RUNTIME_DIR=/tmp/robin-test"}, "/tmp/robin-test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_RUNTIME_DIR", tc.agent)
			if tc.agent == "" {
				os.Unsetenv("XDG_RUNTIME_DIR")
			}
			dir := fake(t, "cat")
			s, err := Start(nil, Options{Name: "test", Env: tc.env, Log: quiet()})
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
			if got := read(t, filepath.Join(dir, "runtime")); got != tc.want {
				t.Errorf("the child's XDG_RUNTIME_DIR is %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAPipelineThatCannotBeBuiltFailsStart(t *testing.T) {
	fake(t, "fail")
	_, err := Start([]string{"nosuchsink"}, Options{Name: "camera", Log: quiet()})
	if err == nil || !strings.Contains(err.Error(), "camera: gst-launch-1.0 exit status 1") || !strings.Contains(err.Error(), `no element "nosuchsink"`) {
		t.Fatalf("Start = %v, want gst's own complaint", err)
	}
}

func TestNoGStreamer(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Start(nil, Options{Name: "microphone", Log: quiet()})
	if err == nil || !strings.Contains(err.Error(), "microphone: gst-launch-1.0 is not installed") {
		t.Fatalf("Start = %v", err)
	}
}

// TestAChildThatExitsIsReplaced: gst quits when the reader of its node
// goes; the next frame finds a new child, and the sink keeps going.
func TestAChildThatExitsIsReplaced(t *testing.T) {
	dir := fake(t, "short")
	t.Setenv("FAKE_GST_BYTES", "4")
	s, err := Start(nil, Options{Name: "test", Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a second child", func() bool { return len(pids(t, dir)) == 2 && s.Write([]byte("efgh")) == nil })
	eventually(t, "the second child's frame", func() bool { return read(t, filepath.Join(dir, "out")) == "abcdefgh" })
	if err := s.Err(); err != nil {
		t.Errorf("Err = %v", err)
	}
}

// TestASinkThatKeepsDyingGivesUp: children that die young, one after
// another, mean the pipeline cannot work; Write says why.
func TestASinkThatKeepsDyingGivesUp(t *testing.T) {
	dir := fake(t, "brief")
	s, err := Start(nil, Options{Name: "test", Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	eventually(t, "giving up", func() bool { return s.Err() != nil })
	if n := len(pids(t, dir)); n != maxQuickExits {
		t.Errorf("%d children started, want %d", n, maxQuickExits)
	}
	err = s.Write([]byte("x"))
	if err == nil || !strings.Contains(err.Error(), "exited 5 times in a row") || !strings.Contains(err.Error(), "link to remote node was destroyed") {
		t.Errorf("Write = %v", err)
	}
}

// TestARestartThatFailsGivesUp: a pipeline that built once and will not
// build again has lost something it needs.
func TestARestartThatFailsGivesUp(t *testing.T) {
	dir := fake(t, "short")
	t.Setenv("FAKE_GST_BYTES", "1")
	s, err := Start(nil, Options{Name: "test", Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t.Setenv("FAKE_GST", "fail")
	s.Write([]byte("x"))
	eventually(t, "giving up", func() bool { return s.Err() != nil })
	if err := s.Err(); !strings.Contains(err.Error(), "nosuchsink") {
		t.Errorf("Err = %v", err)
	}
	if n := len(pids(t, dir)); n != 2 {
		t.Errorf("%d children started, want 2", n)
	}
}

func TestWriteWhileRestarting(t *testing.T) {
	fake(t, "short")
	t.Setenv("FAKE_GST_BYTES", "1")
	old := restartDelay
	restartDelay = 300 * time.Millisecond
	t.Cleanup(func() { restartDelay = old })
	s, err := Start(nil, Options{Name: "test", Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Write([]byte("x"))
	eventually(t, "a failed write", func() bool {
		err := s.Write([]byte("y"))
		return err != nil && strings.Contains(err.Error(), "restarting")
	})
}

// TestCloseKillsAChildThatWillNotStop and its group.
func TestCloseKillsAChildThatWillNotStop(t *testing.T) {
	dir := fake(t, "deaf")
	s, err := Start(nil, Options{Name: "test", Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s.Close()
	if d := time.Since(start); d < stopGrace {
		t.Errorf("Close took %s, less than the %s grace a child gets", d, stopGrace)
	}
	for _, pid := range pids(t, dir) {
		if alive(t, pid) {
			t.Errorf("child %s outlived Close", pid)
		}
	}
}

// TestCloseDuringARestart stops the child that was starting.
func TestCloseDuringARestart(t *testing.T) {
	dir := fake(t, "short")
	t.Setenv("FAKE_GST_BYTES", "1")
	s, err := Start(nil, Options{Name: "test", Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_GST", "cat")
	s.Write([]byte("x"))
	eventually(t, "a second child", func() bool { return len(pids(t, dir)) == 2 })
	s.Close()
	for _, pid := range pids(t, dir) {
		if alive(t, pid) {
			t.Errorf("child %s outlived Close", pid)
		}
	}
}

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		`props,node.description="Robin camera"`: `"props,node.description=\"Robin camera\""`,
		`a\b`:                                   `"a\\b"`,
	} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWhy(t *testing.T) {
	for _, tc := range []struct{ name, msg, want string }{
		{
			// What gst-launch-1.0 1.24 prints when pipewiresink finds no PipeWire.
			"an element's error",
			"ERROR: from element /GstPipeline:pipeline0/GstPipeWireSink:pipewiresink0: Failed to connect\n" +
				"Additional debug info:\n" +
				"../src/gst/gstpipewiresink.c(860): gst_pipewire_sink_open (): /GstPipeline:pipeline0/GstPipeWireSink:pipewiresink0\n" +
				"ERROR: pipeline doesn't want to preroll.\n" +
				"Failed to set pipeline to PAUSED.",
			"ERROR: from element /GstPipeline:pipeline0/GstPipeWireSink:pipewiresink0: Failed to connect",
		},
		{"a parse failure", `WARNING: erroneous pipeline: no element "nosuchsink"`, `WARNING: erroneous pipeline: no element "nosuchsink"`},
		{"no element's error", "one\ntwo\nthree\nfour", "two | three | four"},
	} {
		if got := why(tc.msg); got != tc.want {
			t.Errorf("why of %s = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestLastLines(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"a\nb\nc\nd", 2, "c | d"},
		{"a\nb", 3, "a | b"},
		{"a", 1, "a"},
	} {
		if got := lastLines(tc.in, tc.n); got != tc.want {
			t.Errorf("lastLines(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}
