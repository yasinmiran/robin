//go:build linux

// Package gstsink feeds raw media to a gst-launch-1.0 pipeline on its
// stdin: the browser's microphone and camera on their way into the host's
// PipeWire or a v4l2loopback device.
//
// The child dies with the agent: it has its own process group, Pdeathsig,
// no inherited stdout, and Close stops it. A child that exits on its own is
// started again, because the pipewiresink of PipeWire 1.4 quits when a
// program reading its video node lets go.
package gstsink

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yasinmiran/robin/internal/agent/xdg"
	"github.com/yasinmiran/robin/internal/tail"
)

// Timings, variables so tests can shorten them.
var (
	// startGrace is how long a new pipeline must last to count as built:
	// gst-launch-1.0 exits well inside it on a missing element or device.
	startGrace = 500 * time.Millisecond
	// restartDelay spaces the starts of a child that keeps exiting.
	restartDelay = 200 * time.Millisecond
	// stopGrace is how long Close waits after closing stdin, the pipeline's
	// end of stream, before it kills the child.
	stopGrace = 2 * time.Second
	// stableAfter is how long a child must run for its exit to count as the
	// pipeline having worked rather than failed.
	stableAfter = 5 * time.Second
	// maxQuickExits is how many children in a row may exit within
	// stableAfter before the sink gives up. A program that opens the camera
	// for a moment and lets go ends a child each time, so it allows for a
	// few.
	maxQuickExits = 20
)

// ErrClosed is what Write returns once Close has been called.
var ErrClosed = errors.New("gstsink: closed")

// Options describe a pipeline's child.
type Options struct {
	// Name says what the pipeline is for, in logs and errors.
	Name string
	// Env is added to the agent's environment for the child, after
	// XDG_RUNTIME_DIR, which it may override.
	Env []string
	Log *slog.Logger
}

// Sink is a running pipeline. Write and Close are safe for concurrent use.
type Sink struct {
	args []string
	o    Options
	log  *slog.Logger

	mu     sync.Mutex
	cur    *child // nil while a dead child is being replaced
	closed bool
	err    error         // why the sink gave up
	done   chan struct{} // closed once the supervisor has stopped
}

type child struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stderr  *tail.Buffer
	started time.Time
	exited  chan struct{}
	err     error // valid once exited is closed
}

// Start runs gst-launch-1.0 with args and returns once the pipeline has
// lasted startGrace, so one that cannot be built fails here, in gst's own
// words.
func Start(args []string, o Options) (*Sink, error) {
	s := &Sink{args: args, o: o, log: o.Log, done: make(chan struct{})}
	if s.log == nil {
		s.log = slog.Default()
	}
	c, err := s.spawn()
	if err != nil {
		return nil, err
	}
	s.cur = c
	go s.supervise(c)
	return s, nil
}

// spawn starts one child and waits out startGrace.
func (s *Sink) spawn() (*child, error) {
	cmd := exec.Command("gst-launch-1.0", s.args...)
	// pipewiresink finds the user's PipeWire through XDG_RUNTIME_DIR.
	cmd.Env = append(append(os.Environ(), "XDG_RUNTIME_DIR="+xdg.RuntimeDir()), s.o.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	c := &child{cmd: cmd, stderr: &tail.Buffer{}, started: time.Now(), exited: make(chan struct{})}
	cmd.Stderr = c.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	c.stdin = stdin
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("%s: gst-launch-1.0 is not installed on the host", s.o.Name)
		}
		return nil, fmt.Errorf("%s: start gst-launch-1.0: %w", s.o.Name, err)
	}
	go func() {
		c.err = cmd.Wait()
		close(c.exited)
	}()
	select {
	case <-c.exited:
		return nil, fmt.Errorf("%s: %s", s.o.Name, c.reason())
	case <-time.After(startGrace):
		return c, nil
	}
}

// supervise replaces each child that exits before Close, until
// maxQuickExits in a row have died young.
func (s *Sink) supervise(c *child) {
	defer close(s.done)
	quick := 0
	for {
		<-c.exited
		s.mu.Lock()
		s.cur = nil
		if s.closed {
			s.mu.Unlock()
			return
		}
		if time.Since(c.started) < stableAfter {
			quick++
		} else {
			quick = 1
		}
		if quick >= maxQuickExits {
			s.err = fmt.Errorf("%s: gst-launch-1.0 exited %d times in a row, last: %s", s.o.Name, quick, c.reason())
			s.mu.Unlock()
			s.log.Warn("pipeline keeps exiting; giving up", "pipeline", s.o.Name, "err", s.err)
			return
		}
		s.mu.Unlock()
		s.log.Info("pipeline exited; starting it again", "pipeline", s.o.Name, "why", c.reason())

		time.Sleep(restartDelay)
		next, err := s.spawn()
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			if next != nil {
				next.stop()
			}
			return
		}
		if err != nil {
			// A pipeline that built before and does not now has lost what it
			// needs, such as the PipeWire daemon.
			s.err = err
			s.mu.Unlock()
			s.log.Warn("pipeline would not start again", "pipeline", s.o.Name, "err", err)
			return
		}
		s.cur = next
		s.mu.Unlock()
		c = next
	}
}

// Write hands p to the pipeline in one piece. While a dead child is being
// replaced it fails, and the caller drops what it had: the next child
// starts its stream at the next whole frame.
func (s *Sink) Write(p []byte) error {
	s.mu.Lock()
	c, closed, err := s.cur, s.closed, s.err
	s.mu.Unlock()
	switch {
	case closed:
		return ErrClosed
	case err != nil:
		return err
	case c == nil:
		return fmt.Errorf("%s: restarting", s.o.Name)
	}
	_, err = c.stdin.Write(p)
	return err
}

// Err says why the sink gave up starting children, or is nil while it has
// not.
func (s *Sink) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close ends the stream and stops the child, killing it if it has not
// exited within stopGrace. It is safe to call more than once.
func (s *Sink) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		<-s.done
		return nil
	}
	s.closed = true
	c := s.cur
	s.mu.Unlock()
	if c != nil {
		c.stop()
	}
	<-s.done
	return nil
}

// stop closes stdin, which gst-launch-1.0 reads as the end of the stream,
// and kills the child's group if it lingers.
func (c *child) stop() {
	c.stdin.Close()
	select {
	case <-c.exited:
	case <-time.After(stopGrace):
		syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
		<-c.exited
	}
}

// reason describes how the child ended, with what it said about why.
func (c *child) reason() string {
	status := "exited"
	if c.err != nil {
		status = c.err.Error()
	}
	if msg := strings.TrimSpace(c.stderr.String()); msg != "" {
		return "gst-launch-1.0 " + status + ": " + why(msg)
	}
	return "gst-launch-1.0 " + status
}

// why picks gst's reason out of what it printed: the first error an element
// posted, which the lines after it only restate ("pipeline doesn't want to
// preroll"), or else the last lines, where a pipeline that does not parse
// says so.
func why(msg string) string {
	for _, line := range strings.Split(msg, "\n") {
		if strings.HasPrefix(line, "ERROR: from element ") {
			return line
		}
	}
	return lastLines(msg, 3)
}

// lastLines keeps the last n lines of s.
func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// Quote returns s as a double-quoted value gst-launch-1.0 parses back to
// s, for a property value or a caps field with spaces in it.
func Quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
