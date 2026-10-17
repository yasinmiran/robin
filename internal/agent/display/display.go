//go:build linux

// Package display spawns a private headless X session: an Xvfb server and a
// desktop or window manager running inside it.
//
// Every process it starts lives in its own process group and dies with the
// agent: Close terminates the groups, the kernel kills the direct children
// when the agent dies (Pdeathsig), and a small shell watchdog kills whatever
// is left in the groups, so nothing the session forked outlives the agent.
package display

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	startTimeout = 15 * time.Second
	termGrace    = 2 * time.Second
)

// Options configure Spawn.
type Options struct {
	// RunDir is a private 0700 directory owned by the agent session. Spawn
	// writes the Xauthority and display files there.
	RunDir string
	// MaxWidth and MaxHeight size the Xvfb screen. RandR can switch to any
	// mode up to this size, never beyond it.
	MaxWidth, MaxHeight int
	// Session is a shell command to run in the display. Empty picks the
	// lightest desktop installed.
	Session string
	// Xvfb overrides the Xvfb binary looked up on PATH.
	Xvfb string
	// Env holds extra variables for the session, such as the address of a
	// sound server the agent started.
	Env []string
}

// Display is a running Xvfb with a session inside it.
type Display struct {
	// Name is the DISPLAY value, for example ":7".
	Name string
	// XAuthority is the path of the authority file holding the display's
	// MIT-MAGIC-COOKIE-1.
	XAuthority string
	// Session describes what runs in the display, for example
	// "openbox-session + xterm".
	Session string

	xvfb, session *proc
	companion     *proc // a terminal next to a bare window manager; not watched
	watchdog      *proc
	guard         *os.File // write end of the watchdog's stdin
	files         []string
	done          chan error
	closeOnce     sync.Once
	closeErr      error
}

// Spawn starts Xvfb and the session. ctx bounds startup only; the display
// runs until Close.
func Spawn(ctx context.Context, o Options) (_ *Display, err error) {
	if o.MaxWidth <= 0 || o.MaxHeight <= 0 {
		return nil, fmt.Errorf("display: bad size %dx%d", o.MaxWidth, o.MaxHeight)
	}
	plan, err := pickSession(o.Session, exec.LookPath)
	if err != nil {
		return nil, err
	}
	xvfbPath := o.Xvfb
	if xvfbPath == "" {
		if xvfbPath, err = exec.LookPath("Xvfb"); err != nil {
			return nil, errors.New("display: Xvfb not found on PATH")
		}
	}
	if err := os.MkdirAll(o.RunDir, 0o700); err != nil {
		return nil, err
	}

	d := &Display{
		XAuthority: filepath.Join(o.RunDir, "Xauthority"),
		Session:    plan.name,
		done:       make(chan error, 1),
	}
	defer func() {
		if err != nil {
			d.Close()
		}
	}()

	if err := d.startWatchdog(); err != nil {
		return nil, err
	}
	cookie, err := newCookie()
	if err != nil {
		return nil, err
	}
	d.files = append(d.files, d.XAuthority)
	if err := writeXauthority(d.XAuthority, cookie, ""); err != nil {
		return nil, err
	}
	num, err := d.startXvfb(ctx, xvfbPath, o)
	if err != nil {
		return nil, err
	}
	d.Name = ":" + num
	if err := writeXauthority(d.XAuthority, cookie, num); err != nil {
		return nil, err
	}
	displayFile := filepath.Join(o.RunDir, "display")
	d.files = append(d.files, displayFile)
	if err := writeFileAtomic(displayFile, []byte(d.Name+"\n")); err != nil {
		return nil, err
	}

	env := append(sessionEnv(os.Environ(), d.Name, d.XAuthority), o.Env...)
	group := 0
	if plan.companion != nil {
		// The terminal goes first and the window manager adopts it when it
		// starts. A window mapped just after openbox announces itself can stay
		// unmanaged and invisible for over ten seconds.
		if d.companion, err = d.start(plan.companion, env, 0, false); err != nil {
			return nil, fmt.Errorf("display: start terminal: %w", err)
		}
		group = d.companion.group
		wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if err := waitForWindow(wctx, num, cookie); err != nil {
			slog.Warn("terminal window not mapped yet; starting the window manager anyway", "err", err)
		}
		cancel()
	}
	// One group for the whole session, so Close and the watchdog take both.
	if d.session, err = d.start(plan.main, env, group, true); err != nil {
		return nil, fmt.Errorf("display: start session: %w", err)
	}
	go d.watch()
	return d, nil
}

// Done receives one error when Xvfb or the session exits, then is closed.
// That includes the exits Close causes.
func (d *Display) Done() <-chan error { return d.done }

// Close terminates the session and Xvfb, waiting up to about two seconds
// before killing whatever ignored SIGTERM, and removes the files Spawn wrote.
// It is safe to call more than once.
func (d *Display) Close() error {
	d.closeOnce.Do(func() {
		// Stop the watchdog first: once our own Waits reap the group leaders,
		// their ids may be reused, and it must not signal a stranger.
		d.watchdog.kill()
		d.watchdog.wait(termGrace)
		terminate(d.session, d.companion, d.xvfb)
		if d.guard != nil {
			d.guard.Close()
		}
		for _, f := range d.files {
			if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
				d.closeErr = errors.Join(d.closeErr, err)
			}
		}
	})
	return d.closeErr
}

// watch reports the first of Xvfb and the session to exit on d.done.
func (d *Display) watch() {
	var err error
	select {
	case <-d.xvfb.exited:
		err = d.xvfb.exitError("Xvfb")
	case <-d.session.exited:
		err = d.session.exitError("session " + strconv.Quote(d.Session))
	}
	d.done <- err
	close(d.done)
}

func (d *Display) startXvfb(ctx context.Context, path string, o Options) (string, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	defer r.Close()
	cmd := exec.Command(path,
		"-displayfd", "3",
		"-screen", "0", fmt.Sprintf("%dx%dx24", o.MaxWidth, o.MaxHeight),
		"-nolisten", "tcp",
		"-auth", d.XAuthority,
		"-noreset",
		"-dpi", "96",
	)
	cmd.ExtraFiles = []*os.File{w}
	d.xvfb, err = d.startCmd(cmd, 0, true)
	w.Close() // Xvfb holds the only write end now, so its death means EOF
	if err != nil {
		return "", fmt.Errorf("display: start Xvfb: %w", err)
	}

	num := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(r).ReadString('\n')
		num <- strings.TrimSpace(line)
	}()
	timeout := time.NewTimer(startTimeout)
	defer timeout.Stop()
	select {
	case n := <-num:
		if _, err := strconv.Atoi(n); err == nil {
			return n, nil
		}
		// EOF without a number: Xvfb is on its way out, so wait for its status.
		d.xvfb.wait(time.Second)
		select {
		case <-d.xvfb.exited:
			return "", d.xvfb.exitError("Xvfb")
		default:
			return "", errors.New("display: Xvfb closed -displayfd without a display number")
		}
	case <-d.xvfb.exited:
		return "", d.xvfb.exitError("Xvfb")
	case <-timeout.C:
		return "", errors.New("display: Xvfb did not report a display number in time")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// startWatchdog runs a shell that collects process group ids on stdin and,
// when stdin closes because the agent died, terminates every group it was
// given. Pdeathsig only reaches direct children; this reaches the rest.
func (d *Display) startWatchdog() error {
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer r.Close()
	const script = `trap '' HUP INT TERM
while read -r g; do set -- "$@" "$g"; done
for g; do kill -TERM -"$g"; done
sleep 2
for g; do kill -KILL -"$g"; done`
	cmd := exec.Command("/bin/sh", "-c", script, "remotejump-display-watchdog")
	cmd.Stdin = r
	// Its own group and no Pdeathsig: it has to outlive the agent.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if d.watchdog, err = startProc(cmd); err != nil {
		w.Close()
		return fmt.Errorf("display: start watchdog: %w", err)
	}
	d.guard = w
	return nil
}

func (d *Display) start(argv, env []string, pgid int, logStderr bool) (*proc, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	return d.startCmd(cmd, pgid, logStderr)
}

// startCmd starts cmd in process group pgid (0 for a new group), killed by
// the kernel if the agent dies, and hands a new group to the watchdog.
func (d *Display) startCmd(cmd *exec.Cmd, pgid int, logStderr bool) (*proc, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid, Pdeathsig: syscall.SIGKILL}
	var stderr *os.File
	if logStderr {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		defer w.Close()
		cmd.Stderr = w
		stderr = r
	}
	p, err := startProc(cmd)
	if err != nil {
		if stderr != nil {
			stderr.Close()
		}
		return nil, err
	}
	if stderr != nil {
		p.stderrDone = make(chan struct{})
		go func() {
			io.Copy(&p.stderr, stderr)
			stderr.Close()
			close(p.stderrDone)
		}()
	}
	if pgid != 0 {
		p.group = pgid
	} else {
		fmt.Fprintln(d.guard, p.group)
	}
	return p, nil
}

// terminate sends SIGTERM to each process's group, gives the groups
// termGrace to empty, then SIGKILLs whatever is left and waits for it to go.
func terminate(ps ...*proc) {
	for _, p := range ps {
		p.signalGroup(syscall.SIGTERM)
	}
	waitGroups(ps, termGrace)
	for _, p := range ps {
		p.signalGroup(syscall.SIGKILL)
	}
	waitGroups(ps, termGrace)
	for _, p := range ps {
		p.wait(termGrace)
	}
}

// waitGroups polls until every group is empty or d has passed.
func waitGroups(ps []*proc, d time.Duration) {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		alive := false
		for _, p := range ps {
			alive = alive || p.groupAlive()
		}
		if !alive {
			return
		}
	}
}

// proc is one started child and its exit status.
type proc struct {
	cmd        *exec.Cmd
	group      int // process group id, which is not always this process's pid
	exited     chan struct{}
	err        error // valid once exited is closed
	stderr     tail
	stderrDone chan struct{} // nil when stderr is not captured
}

func startProc(cmd *exec.Cmd) (*proc, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &proc{cmd: cmd, group: cmd.Process.Pid, exited: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

func (p *proc) pgid() int { return p.group }

func (p *proc) signalGroup(sig syscall.Signal) {
	if p != nil && p.group > 0 {
		syscall.Kill(-p.pgid(), sig)
	}
}

func (p *proc) groupAlive() bool {
	return p != nil && p.group > 0 && syscall.Kill(-p.pgid(), 0) != syscall.ESRCH
}

func (p *proc) kill() {
	if p != nil {
		p.cmd.Process.Kill()
	}
}

func (p *proc) wait(d time.Duration) {
	if p == nil {
		return
	}
	t := time.NewTimer(max(d, 0))
	defer t.Stop()
	select {
	case <-p.exited:
	case <-t.C:
	}
}

// exitError describes how the process ended, with the end of its stderr.
func (p *proc) exitError(what string) error {
	if p.stderrDone != nil {
		select {
		case <-p.stderrDone:
		case <-time.After(200 * time.Millisecond): // descendants may hold stderr open
		}
	}
	status := "exited"
	if p.err != nil {
		status = p.err.Error()
	}
	if s := strings.TrimSpace(p.stderr.String()); s != "" {
		return fmt.Errorf("%s %s: %s", what, status, s)
	}
	return fmt.Errorf("%s %s", what, status)
}

// tail keeps the last few KiB written to it: enough to explain a failure
// without letting a chatty session grow memory.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

const tailSize = 4096

func (t *tail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if over := len(t.buf) - tailSize; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(b), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

func sessionEnv(base []string, display, xauthority string) []string {
	env := make([]string, 0, len(base)+3)
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "DISPLAY", "XAUTHORITY", "XDG_SESSION_TYPE", "WAYLAND_DISPLAY":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "DISPLAY="+display, "XAUTHORITY="+xauthority, "XDG_SESSION_TYPE=x11")
}
