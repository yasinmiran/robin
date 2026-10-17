// Package sshmux drives the system OpenSSH client as a connection multiplexer.
// One master process holds the connection; commands, SFTP and port forwards
// ride it through its control socket. Using the user's own ssh keeps their
// config, agent, jump hosts, hardware keys and interactive prompts working.
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

	"github.com/yasinmiran/remotejump/internal/tail"
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
}

// Master is a running ssh ControlMaster.
type Master struct {
	ssh  string
	args []string
	dest string
	dir  string
	ctl  string

	cmd     *exec.Cmd
	stderr  tail.Buffer
	done    chan struct{}
	waitErr error

	closeOnce sync.Once
	closeErr  error
}

const controlName = "ctl"

// Dial starts a master connection to dest and returns once it accepts
// multiplexed sessions. ctx bounds only the wait, which includes any prompts
// the user answers; the connection itself lives until Close or a network drop.
func Dial(ctx context.Context, dest string, opts Options) (*Master, error) {
	if dest == "" || strings.HasPrefix(dest, "-") {
		return nil, fmt.Errorf("sshmux: invalid destination %q", dest)
	}
	dir, err := controlDir()
	if err != nil {
		return nil, err
	}
	m := &Master{
		ssh:  cmp.Or(opts.SSHPath, "ssh"),
		args: opts.Args,
		dest: dest,
		dir:  dir,
		ctl:  filepath.Join(dir, controlName),
		done: make(chan struct{}),
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	m.cmd = exec.Command(m.ssh, m.masterArgs()...)
	m.cmd.Stderr = io.MultiWriter(&m.stderr, stderr)
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

// ForwardLocal makes the master listen on the unix socket path local and
// forward each connection to remote, which is "host:port" or a unix socket
// path on the remote host. local must not exist yet; SocketPath gives a
// suitable one. The forward lasts until the master exits.
func (m *Master) ForwardLocal(ctx context.Context, local, remote string) error {
	if !filepath.IsAbs(local) || strings.Contains(local, ":") {
		return fmt.Errorf("sshmux: local forward path %q must be absolute and free of ':'", local)
	}
	if remote == "" {
		return errors.New("sshmux: empty forward target")
	}
	return m.control(ctx, "forward", "-L", local+":"+remote)
}

// SocketPath returns a path for a unix socket called name in the master's
// private directory, which Close removes. The directory is chosen so short
// names fit the platform's socket path limit.
func (m *Master) SocketPath(name string) string {
	return filepath.Join(m.dir, name)
}

// Done is closed when the master process exits, whether through Close, a
// network drop or a signal.
func (m *Master) Done() <-chan struct{} {
	return m.done
}

// Close asks the master to exit, kills it if it does not, and removes the
// control directory. Commands still running over the master fail.
func (m *Master) Close() error {
	m.closeOnce.Do(func() {
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
// being an escape.
func (m *Master) sessionArgs() []string {
	return []string{
		"-F", os.DevNull,
		"-o", "ControlMaster=no",
		"-o", "ProxyCommand=false",
		"-o", "BatchMode=yes",
		"-S", m.ctl, "-T", "-a", "-x", "-e", "none",
	}
}

// controlDir makes a private directory for the control socket. ssh binds the
// socket under a temporary name 17 bytes longer than ControlPath before
// renaming it, so the limit is checked against that.
func controlDir() (string, error) {
	limit := 108 // sizeof(sun_path) on Linux
	if runtime.GOOS != "linux" {
		limit = 104
	}
	const worst = len("/rj-4294967295/" + controlName + ".0123456789abcdef")
	var lastErr error
	for _, base := range []string{os.TempDir(), "/tmp"} {
		// ssh expands % tokens in the control path.
		if len(base)+worst >= limit || strings.Contains(base, "%") {
			continue
		}
		dir, err := os.MkdirTemp(base, "rj-")
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
