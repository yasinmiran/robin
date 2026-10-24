//go:build linux

package agenttest

import (
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// recorder stands in for a test, to see what a helper would fail or skip
// it with. Fatal, Fatalf and Skipf end the goroutine as theirs do.
type recorder struct {
	testing.TB
	failed, skipped string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) { r.failed = fmt.Sprintf(format, args...) }

func (r *recorder) Fatal(args ...any) {
	r.failed = fmt.Sprint(args...)
	runtime.Goexit()
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.failed = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (r *recorder) Skipf(format string, args ...any) {
	r.skipped = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// record runs f against a fresh recorder on a goroutine of its own, which
// a fatal call may end.
func record(f func(r *recorder)) *recorder {
	r := &recorder{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f(r)
	}()
	<-done
	return r
}

// Without root or the second account, NewStranger skips the test, except
// inside the test-x container, where that means the image lost them.
func TestNewStrangerFailsOnlyInTestX(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("runs as root, so the account decides")
	}
	t.Setenv(xtestEnv, "")
	if r := record(func(r *recorder) { NewStranger(r) }); r.skipped == "" || r.failed != "" {
		t.Errorf("outside test-x: skipped %q, failed %q, want a skip", r.skipped, r.failed)
	}
	t.Setenv(xtestEnv, "1")
	if r := record(func(r *recorder) { NewStranger(r) }); r.skipped != "" || !strings.Contains(r.failed, strangerName) {
		t.Errorf("in test-x: skipped %q, failed %q, want a failure naming %s", r.skipped, r.failed, strangerName)
	}
}

func TestOpenDirWithoutATempDir(t *testing.T) {
	t.Setenv("TMPDIR", "/nonexistent/robin-agenttest")
	if r := record(func(r *recorder) { OpenDir(r) }); r.failed == "" {
		t.Error("OpenDir without a temp dir did not fail the test")
	}
}

func TestHungUp(t *testing.T) {
	reset := &net.OpError{Op: "read", Net: "unix", Err: os.NewSyscallError("read", syscall.ECONNRESET)}
	for err, want := range map[error]bool{io.EOF: true, reset: true, os.ErrDeadlineExceeded: false, io.ErrUnexpectedEOF: false} {
		if HungUp(err) != want {
			t.Errorf("HungUp(%v) = %v", err, !want)
		}
	}
	if HungUp(nil) {
		t.Error("HungUp of no error")
	}
}

// TerminalSafe takes newlines and plain text, and flags escapes, the
// separators that break a line and the marks that reorder it.
func TestTerminalSafe(t *testing.T) {
	for s, unsafe := range map[string]bool{
		"report.pdf\n":   false,
		"naïve 文字":       false,
		"\x1b]0;robin\a": true,
		"a b":            true,
		"a b":            true,
		"a‮b":            true,
	} {
		r := record(func(r *recorder) { TerminalSafe(r, "s", s) })
		if (r.failed != "") != unsafe {
			t.Errorf("TerminalSafe(%q) failed with %q, want unsafe %v", s, r.failed, unsafe)
		}
	}
}
