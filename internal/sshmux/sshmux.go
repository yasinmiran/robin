// Package sshmux drives the system OpenSSH client as a connection multiplexer.
// One master process holds the connection; commands, SFTP and port forwards
// ride it through its control socket. Using the user's own ssh keeps their
// config, agent, jump hosts, hardware keys and interactive prompts working.
// Where ssh has no ControlMaster, as on Windows, Options.NoMux gives each of
// them a connection of its own instead.
package sshmux

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/yasinmiran/robin/internal/tail"
)

// Options configures every ssh invocation of a Master.
type Options struct {
	// SSHPath is the ssh binary. Default "ssh".
	SSHPath string
	// Args are extra ssh arguments for the master, placed before the
	// destination, for example {"-F", cfg}, {"-p", "2222"} or {"-J", jump}.
	// Sessions over the master do not see them.
	Args []string
	// Stderr receives the master's stderr for the life of the connection.
	// Default os.Stderr. Password, 2FA and host key prompts use /dev/tty, not
	// this writer.
	Stderr io.Writer
	// NoMux runs every command, SFTP session and forward as an ssh process
	// with a connection of its own, for clients without ControlMaster such
	// as Windows OpenSSH. Each one authenticates by itself, so a password or
	// 2FA prompt comes once per connection unless a key or an agent answers.
	NoMux bool
}

// Master is a running ssh ControlMaster, or with Options.NoMux the settings
// that each connection of its own starts from.
type Master struct {
	ssh  string
	args []string
	dest string
	dir  string // private directory for unix sockets; none for tcpForwards
	ctl  string
	mux  bool
	// tcpForwards puts forwards without a master on loopback TCP ports,
	// which freeAddr picks, rather than unix sockets in dir: Windows'
	// OpenSSH forwards no unix sockets.
	tcpForwards bool
	freeAddr    func() (string, error)
	// out receives the stderr of the master, or of each forward without one.
	out io.Writer

	cmd     *exec.Cmd
	stderr  tail.Buffer
	done    chan struct{}
	waitErr error

	mu       sync.Mutex
	forwards []*process // forwards of their own, stopped by Close
	closed   bool       // Close has stopped the forwards
	sockets  int        // unix sockets handed out for forwards

	closeOnce sync.Once
	closeErr  error
}

const controlName = "ctl"

// Dial starts a master connection to dest and returns once it accepts
// multiplexed sessions. ctx bounds only the wait, which includes any prompts
// the user answers; the connection itself lives until Close or a network drop.
//
// With opts.NoMux nothing connects yet: each command, SFTP session and
// forward connects when it starts, and the first to fail says why.
func Dial(ctx context.Context, dest string, opts Options) (*Master, error) {
	if dest == "" || strings.HasPrefix(dest, "-") {
		return nil, fmt.Errorf("sshmux: invalid destination %q", dest)
	}
	m := &Master{
		ssh:         cmp.Or(opts.SSHPath, "ssh"),
		args:        opts.Args,
		dest:        dest,
		mux:         !opts.NoMux,
		tcpForwards: opts.NoMux && runtime.GOOS == "windows",
		freeAddr:    freeLoopbackAddr,
		out:         cmp.Or[io.Writer](opts.Stderr, os.Stderr),
		done:        make(chan struct{}),
	}
	if m.tcpForwards {
		return m, nil
	}
	dir, err := controlDir()
	if err != nil {
		return nil, err
	}
	m.dir, m.ctl = dir, filepath.Join(dir, controlName)
	if !m.mux {
		return m, nil
	}

	name, args := ignoringInterrupt(m.ssh, m.masterArgs()...)
	m.cmd = exec.Command(name, args...)
	m.cmd.Stderr = io.MultiWriter(&m.stderr, m.out)
	// A ProxyCommand child can hold stderr open after the master exits.
	m.cmd.WaitDelay = time.Second
	if err := m.cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("sshmux: start %s: %w", m.ssh, err)
	}
	go func() {
		m.waitErr = m.cmd.Wait()
		close(m.done)
	}()

	if err := m.waitReady(ctx); err != nil {
		_ = m.cmd.Process.Kill()
		<-m.done
		os.RemoveAll(dir)
		return nil, err
	}
	return m, nil
}

func (m *Master) waitReady(ctx context.Context) error {
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		// ssh creates the control socket only once authentication succeeds.
		if _, err := os.Stat(m.ctl); err == nil && m.control(ctx, "check") == nil {
			return nil
		}
		select {
		case <-m.done:
			msg := strings.TrimSpace(m.stderr.String())
			if msg == "" {
				msg = "no output"
			}
			return fmt.Errorf("sshmux: ssh to %s failed (%v): %s", m.dest, m.waitErr, msg)
		case <-ctx.Done():
			return fmt.Errorf("sshmux: ssh to %s: %w", m.dest, ctx.Err())
		case <-tick.C:
		}
	}
}

// Command returns an unstarted ssh command that runs remoteCmd over the master
// without a PTY, so its stdin and stdout are binary safe. The remote side runs
// remoteCmd through the user's login shell; build it with Quote. Cancelling
// ctx kills the local ssh process.
func (m *Master) Command(ctx context.Context, remoteCmd string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, m.ssh, m.childArgs(remoteCmd)...)
	cmd.WaitDelay = time.Second
	return cmd
}

// ignoringInterrupt wraps the master's command line so ssh starts with
// SIGINT ignored. Ctrl+C in a terminal reaches every process in the
// foreground group, and the master would die of it before the CLI could
// tell the host anything; ssh keeps an ignore it inherits (its client loop
// installs a SIGINT handler only when the signal is not already ignored),
// so the connection outlives the keystroke and the CLI ends the session
// over it, then closes it. Sessions over the master install their handler
// unconditionally and do die, which the CLI expects.
func ignoringInterrupt(ssh string, args ...string) (string, []string) {
	return "/bin/sh", append([]string{"-c", `trap '' INT; exec "$0" "$@"`, ssh}, args...)
}

// Output runs remoteCmd with stdin (nil for none) and returns its stdout. A
// failed command's error includes its stderr.
func (m *Master) Output(ctx context.Context, remoteCmd string, stdin io.Reader) ([]byte, error) {
	cmd := m.Command(ctx, remoteCmd)
	cmd.Stdin = stdin
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		err = fmt.Errorf("%w: %s", err, bytes.TrimSpace(exitErr.Stderr))
	}
	return out, err
}

// forwardLocal makes the master listen on the unix socket path local and
// forward each connection to remote, which is "host:port" or a unix socket
// path on the remote host. local must not exist yet; socketPath gives a
// suitable one. The forward lasts until the master exits.
func (m *Master) forwardLocal(ctx context.Context, local, remote string) error {
	if !m.mux {
		return errors.New("sshmux: unix socket forwards need a ControlMaster")
	}
	if !filepath.IsAbs(local) || strings.Contains(local, ":") {
		return fmt.Errorf("sshmux: local forward path %q must be absolute and free of ':'", local)
	}
	if remote == "" {
		return errors.New("sshmux: empty forward target")
	}
	return m.control(ctx, "forward", "-L", local+":"+remote)
}

// socketPath returns a path for a unix socket called name in the master's
// private directory, which Close removes. The directory is chosen so short
// names fit the platform's socket path limit.
func (m *Master) socketPath(name string) string {
	return filepath.Join(m.dir, name)
}

// nextForwardPath returns the socket path for the next forward.
func (m *Master) nextForwardPath() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sockets++
	return m.socketPath(fmt.Sprintf("fwd%d.sock", m.sockets))
}

// Done is closed when the master process exits, whether through Close, a
// network drop or a signal. Without a master it is closed by Close only: a
// network drop shows as the failure of whatever was using the connection.
func (m *Master) Done() <-chan struct{} {
	return m.done
}

// Dead reports whether the master process has exited.
func (m *Master) Dead() bool {
	select {
	case <-m.done:
		return true
	default:
		return false
	}
}

// Close asks the master to exit, kills it if it does not, and removes the
// private directory. Commands still running over the master fail. Without
// a master it stops the forwards; commands end with their contexts.
func (m *Master) Close() error {
	m.closeOnce.Do(func() {
		m.stopForwards()
		if !m.mux {
			close(m.done)
			if m.dir != "" {
				m.closeErr = os.RemoveAll(m.dir)
			}
			return
		}
		select {
		case <-m.done:
		default:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = m.control(ctx, "exit")
			cancel()
			select {
			case <-m.done:
			case <-time.After(5 * time.Second):
				_ = m.cmd.Process.Kill()
				<-m.done
			}
		}
		m.closeErr = os.RemoveAll(m.dir)
	})
	return m.closeErr
}

// control runs "ssh -O op". It reads no config file: the control socket is
// all a control command needs, and a user's LocalForward entries would
// otherwise ride along with -O forward.
func (m *Master) control(ctx context.Context, op string, extra ...string) error {
	args := append([]string{"-F", os.DevNull, "-S", m.ctl, "-O", op}, extra...)
	args = append(args, "--", m.dest)
	cmd := exec.CommandContext(ctx, m.ssh, args...)
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sshmux: ssh -O %s: %w: %s", op, err, bytes.TrimSpace(out))
	}
	return nil
}

// masterArgs builds the master's command line. Our -o options come before
// the user's Args so they win (ssh keeps the first value it sees); flags come
// after so they override any the user passed.
func (m *Master) masterArgs() []string {
	args := []string{
		"-o", "ControlMaster=yes",
		"-o", "ControlPersist=no",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ClearAllForwardings=yes",
		"-o", "RemoteCommand=none",
	}
	args = append(args, m.args...)
	return append(args, "-S", m.ctl, "-N", "-n", "--", m.dest)
}

func (m *Master) childArgs(remoteCmd string) []string {
	args := m.sessionArgs()
	args = append(args, "--", m.dest)
	if remoteCmd != "" {
		args = append(args, remoteCmd)
	}
	return args
}

func (m *Master) subsystemArgs(name string) []string {
	return append(m.sessionArgs(), "-s", "--", m.dest, name)
}

// sessionArgs are shared by every session over the master. Sessions read no
// config and ignore Args: only the master authenticates, and the user's
// forwards, RemoteCommand or a -J would conflict with session flags.
// ProxyCommand=false makes ssh fail instead of silently opening a fresh
// connection when the master is gone. -e none keeps "~." in binary stdin from
// being an escape. Without a master, each session is a connection of its
// own and takes ownArgs instead.
func (m *Master) sessionArgs() []string {
	if !m.mux {
		return m.ownArgs(true)
	}
	return []string{
		"-F", os.DevNull,
		"-o", "ControlMaster=no",
		"-o", "ProxyCommand=false",
		"-o", "BatchMode=yes",
		"-S", m.ctl, "-T", "-a", "-x", "-e", "none",
	}
}

// controlDir makes a private directory for unix sockets: the master's
// control socket, and the forwards'. ssh binds the control socket under a
// temporary name 17 bytes longer than ControlPath before renaming it, so
// the limit is checked against that.
func controlDir() (string, error) {
	limit := 108 // sizeof(sun_path) on Linux
	if runtime.GOOS != "linux" {
		limit = 104
	}
	const worst = len("/robin-4294967295/" + controlName + ".0123456789abcdef")
	var lastErr error
	for _, base := range []string{os.TempDir(), "/tmp"} {
		// ssh expands % tokens in the control path.
		if len(base)+worst >= limit || strings.Contains(base, "%") {
			continue
		}
		dir, err := os.MkdirTemp(base, "robin-")
		if err == nil {
			return dir, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("temp dir path too long for a unix socket")
	}
	return "", fmt.Errorf("sshmux: control dir: %w", lastErr)
}

// Quote joins args into a POSIX shell command line, single-quoting each one
// that needs it.
func Quote(args ...string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = quoteArg(a)
	}
	return strings.Join(quoted, " ")
}

func quoteArg(a string) string {
	if a == "" {
		return "''"
	}
	if strings.IndexFunc(a, unsafeShellRune) < 0 {
		return a
	}
	return "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
}

func unsafeShellRune(r rune) bool {
	switch {
	case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9':
		return false
	}
	return !strings.ContainsRune("_-./:@,+%", r)
}
