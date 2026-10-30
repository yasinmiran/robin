package sshmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yasinmiran/robin/internal/tail"
)

// Forward is a local endpoint that leads to an address on the remote host.
type Forward struct {
	network, addr string
	// proc is the forward's own ssh, without a master; once it has exited,
	// whatever listens at addr is not the forward.
	proc *process
	// stdio, set in place of addr, opens each connection as an ssh -W.
	stdio func() (net.Conn, error)
}

// Dial opens a connection through the forward.
func (f *Forward) Dial() (net.Conn, error) {
	if f.stdio != nil {
		return f.stdio()
	}
	if f.proc != nil {
		select {
		case <-f.proc.exited:
			return nil, fmt.Errorf("sshmux: the forward's ssh has exited: %w", f.proc.failure())
		default:
		}
	}
	return net.Dial(f.network, f.addr)
}

// Forward makes remote, a loopback address and port on the remote host,
// reachable from this machine until m.Close. It is a unix socket in the
// master's private directory, served by the master or, without one, by an
// ssh -N -L of its own. On Windows, whose OpenSSH forwards no unix sockets,
// it listens nowhere: each Dial starts an ssh -W that logs in for that
// connection alone, as a loopback port would let any user of this machine
// through.
func (m *Master) Forward(ctx context.Context, remote string) (*Forward, error) {
	if err := checkTarget(remote); err != nil {
		return nil, err
	}
	if m.stdioForwards {
		m.mu.Lock()
		closed := m.closed
		m.mu.Unlock()
		if closed {
			return nil, errors.New("sshmux: forward after Close")
		}
		return &Forward{stdio: func() (net.Conn, error) { return m.dialStdio(remote) }}, nil
	}
	if !m.mux {
		return m.forwardOwn(ctx, remote)
	}
	sock := m.nextForwardPath()
	if err := m.forwardLocal(ctx, sock, remote); err != nil {
		return nil, err
	}
	return &Forward{network: "unix", addr: sock}, nil
}

// checkTarget refuses a forward target that is not a loopback IP literal
// and a port of 1 to 65535, written as netip writes it. The target comes
// from the host, and ssh reads the -L spec as more than a target: it splits
// it at colons, takes brackets and backslashes as quoting, and expands
// ${VAR} from the client's environment, which would hand the host whatever
// secret that environment holds.
func checkTarget(remote string) error {
	ap, err := netip.ParseAddrPort(remote)
	if err != nil || !ap.Addr().IsLoopback() || ap.Addr().Zone() != "" || ap.Port() == 0 || ap.String() != remote {
		return fmt.Errorf("sshmux: malformed forward target %q: want a loopback address and port", remote)
	}
	return nil
}

// ownArgs start an ssh that connects by itself, without a master. It reads
// the user's config and takes Args, as a master would, with our options
// first so they win. ControlPath=none keeps a ControlMaster in that config
// from making it a master that outlives the session. clearForwards drops
// the forwards the config asks for, which every connection would otherwise
// set up again; a forward's own ssh -L keeps them, because the option
// drops the command line's -L as well.
func (m *Master) ownArgs(clearForwards bool) []string {
	args := []string{
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "RemoteCommand=none",
	}
	if clearForwards {
		args = append(args, "-o", "ClearAllForwardings=yes")
	}
	args = append(args, m.args...)
	return append(args, "-T", "-a", "-x", "-e", "none")
}

func (m *Master) forwardArgs(local, remote string) []string {
	args := append([]string{"-o", "ExitOnForwardFailure=yes"}, m.ownArgs(false)...)
	return append(args, "-N", "-n", "-L", local+":"+remote, "--", m.dest)
}

func (m *Master) stdioArgs(remote string) []string {
	return append(m.ownArgs(true), "-W", remote, "--", m.dest)
}

// forwardOwn starts an ssh that does nothing but forward to remote, and
// returns once that ssh listens.
func (m *Master) forwardOwn(ctx context.Context, remote string) (*Forward, error) {
	sock := m.nextForwardPath()
	p, err := m.startProcess(m.forwardArgs(sock, remote), nil, nil)
	if err != nil {
		return nil, err
	}
	if !m.track(p) {
		p.stop()
		return nil, errors.New("sshmux: forward after Close")
	}
	if err := p.awaitListening(ctx, sock); err != nil {
		return nil, fmt.Errorf("sshmux: forward to %s: %w", remote, err)
	}
	return &Forward{network: "unix", addr: sock, proc: p}, nil
}

// dialStdio connects to remote over the stdin and stdout of an ssh -W,
// which closing the connection stops. It returns before ssh has logged in;
// a login that fails ends the connection.
func (m *Master) dialStdio(remote string) (net.Conn, error) {
	sshIn, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	r, sshOut, err := os.Pipe()
	if err != nil {
		sshIn.Close()
		w.Close()
		return nil, err
	}
	p, err := m.startProcess(m.stdioArgs(remote), sshIn, sshOut)
	sshIn.Close()
	sshOut.Close()
	if err != nil {
		w.Close()
		r.Close()
		return nil, err
	}
	c := &stdioConn{r: r, w: w, proc: p, remote: remote, m: m}
	if !m.track(p) {
		c.Close()
		return nil, errors.New("sshmux: forward after Close")
	}
	return c, nil
}

// track has Close stop p, and reports false, with p left to the caller,
// once Close has run.
func (m *Master) track(p *process) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		m.forwards = append(m.forwards, p)
	}
	return !m.closed
}

func (m *Master) untrack(p *process) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forwards = slices.DeleteFunc(m.forwards, func(q *process) bool { return q == p })
}

// stopForwards stops the forwards of their own and refuses new ones.
func (m *Master) stopForwards() {
	m.mu.Lock()
	ps := m.forwards
	m.forwards, m.closed = nil, true
	m.mu.Unlock()
	for _, p := range ps {
		p.stop()
	}
}

// stdioConn is a connection over the stdin and stdout of an ssh -W.
type stdioConn struct {
	r, w   *os.File // ssh's stdout and stdin
	proc   *process
	remote string
	m      *Master
	once   sync.Once
}

func (c *stdioConn) Read(b []byte) (int, error)  { return c.r.Read(b) }
func (c *stdioConn) Write(b []byte) (int, error) { return c.w.Write(b) }

// CloseWrite closes ssh's stdin, which ssh passes on as the end of what
// the connection sends.
func (c *stdioConn) CloseWrite() error { return c.w.Close() }

func (c *stdioConn) Close() error {
	c.once.Do(func() {
		c.w.Close()
		c.proc.stop()
		c.r.Close()
		c.m.untrack(c.proc)
	})
	return nil
}

func (c *stdioConn) LocalAddr() net.Addr  { return stdioAddr("ssh -W") }
func (c *stdioConn) RemoteAddr() net.Addr { return stdioAddr(c.remote) }

func (c *stdioConn) SetDeadline(t time.Time) error {
	return errors.Join(c.r.SetReadDeadline(t), c.w.SetWriteDeadline(t))
}
func (c *stdioConn) SetReadDeadline(t time.Time) error  { return c.r.SetReadDeadline(t) }
func (c *stdioConn) SetWriteDeadline(t time.Time) error { return c.w.SetWriteDeadline(t) }

// stdioAddr names an end of a stdioConn.
type stdioAddr string

func (a stdioAddr) Network() string { return "ssh" }
func (a stdioAddr) String() string  { return string(a) }

// process is an ssh of its own that runs until it is stopped.
type process struct {
	cmd    *exec.Cmd
	stderr tail.Buffer
	exited chan struct{}
	err    error // set before exited closes
}

// startProcess starts an ssh with args, reading stdin and writing stdout,
// or /dev/null for either one that is nil.
func (m *Master) startProcess(args []string, stdin, stdout *os.File) (*process, error) {
	p := &process{cmd: shielded(context.Background(), m.ssh, args...), exited: make(chan struct{})}
	if stdin != nil {
		p.cmd.Stdin = stdin
	}
	if stdout != nil {
		p.cmd.Stdout = stdout
	}
	p.cmd.Stderr = io.MultiWriter(&p.stderr, m.out)
	// A ProxyCommand child can hold stderr open after ssh exits.
	p.cmd.WaitDelay = time.Second
	if err := p.cmd.Start(); err != nil {
		return nil, fmt.Errorf("sshmux: start %s: %w", m.ssh, err)
	}
	go func() {
		p.err = p.cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

// awaitListening returns once ssh listens at sock, or fails when it exits
// or ctx ends first. A unix socket in the private directory can only be
// ssh's, so a connection there tells, which reaches the remote end as one
// that sends nothing.
func (p *process) awaitListening(ctx context.Context, sock string) error {
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if accepts(ctx, "unix", sock) {
			return nil
		}
		select {
		case <-p.exited:
			return p.failure()
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// accepts reports whether addr takes a connection, which it closes.
func accepts(ctx context.Context, network, addr string) bool {
	c, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, addr)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// failure says why ssh exited. Only once exited is closed.
func (p *process) failure() error {
	msg := strings.TrimSpace(p.stderr.String())
	if msg == "" {
		msg = "no output"
	}
	return fmt.Errorf("ssh failed (%v): %s", p.err, msg)
}

// stop asks ssh to exit, which also ends a ProxyCommand it started, and
// kills it if it does not. Windows can only kill.
func (p *process) stop() {
	if p.cmd.Process.Signal(syscall.SIGTERM) != nil {
		_ = p.cmd.Process.Kill()
	}
	select {
	case <-p.exited:
	case <-time.After(5 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.exited
	}
}
