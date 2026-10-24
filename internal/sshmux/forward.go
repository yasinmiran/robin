package sshmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
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
}

// Dial opens a connection through the forward.
func (f *Forward) Dial() (net.Conn, error) {
	if f.proc != nil {
		select {
		case <-f.proc.exited:
			return nil, fmt.Errorf("sshmux: the forward's ssh has exited: %w", f.proc.failure())
		default:
		}
	}
	return net.Dial(f.network, f.addr)
}

// Forward makes remote, "host:port" or a unix socket path on the remote
// host, reachable from this machine until m.Close. It is a unix socket in
// the master's private directory, served by the master or, without one,
// by an ssh -N -L of its own. On Windows, whose OpenSSH forwards no unix
// sockets, that ssh listens on a loopback TCP port instead, which any user
// of this machine can connect to.
func (m *Master) Forward(ctx context.Context, remote string) (*Forward, error) {
	if err := checkTarget(remote); err != nil {
		return nil, err
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

// checkTarget refuses a forward target that is neither host:port, with a
// port of 1 to 65535 and brackets only around an IPv6 address, nor an
// absolute unix socket path. ssh splits the -L spec at colons and takes
// brackets and backslashes as quoting, so anything else would land in other
// fields of the spec than the target's.
func checkTarget(remote string) error {
	malformed := fmt.Errorf("sshmux: malformed forward target %q: want host:port or an absolute socket path", remote)
	if strings.HasPrefix(remote, "/") {
		if strings.ContainsAny(remote, `:\`) {
			return malformed
		}
		return nil
	}
	host, port, err := net.SplitHostPort(remote)
	if err != nil {
		return malformed
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return malformed
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" {
			return malformed
		}
	} else if !hostname(host) {
		return malformed
	}
	if net.JoinHostPort(host, port) != remote {
		return malformed
	}
	return nil
}

// hostname reports whether s is made of DNS labels: letters, digits and
// inner hyphens, joined by dots.
func hostname(s string) bool {
	if s == "" {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

// ownArgs start an ssh that connects by itself, without a master. It reads
// the user's config and takes Args, as a master would, with our options
// first so they win. ControlPath=none keeps a ControlMaster in that config
// from making it a master that outlives the session. clearForwards drops
// the forwards the config asks for, which every connection would otherwise
// set up again; a forward's own ssh keeps them, because the option drops
// the command line's -L as well.
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

// forwardOwn starts an ssh that does nothing but forward to remote, and
// returns once that ssh listens.
func (m *Master) forwardOwn(ctx context.Context, remote string) (*Forward, error) {
	network, local := "unix", ""
	if m.tcpForwards {
		addr, err := m.freeAddr()
		if err != nil {
			return nil, err
		}
		network, local = "tcp", addr
	} else {
		local = m.nextForwardPath()
	}
	p, err := m.startProcess(m.forwardArgs(local, remote))
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	closed := m.closed
	if !closed {
		m.forwards = append(m.forwards, p)
	}
	m.mu.Unlock()
	if closed {
		p.stop()
		return nil, errors.New("sshmux: forward after Close")
	}
	if err := p.awaitListening(ctx, network, local); err != nil {
		return nil, fmt.Errorf("sshmux: forward to %s: %w", remote, err)
	}
	return &Forward{network: network, addr: local, proc: p}, nil
}

// freeLoopbackAddr returns a loopback TCP address nobody listens on: a port
// the system just handed out, released for ssh to take. ssh binds it only
// once it has logged in, and another user could take it meanwhile, which
// awaitListening tells from ssh taking it.
func freeLoopbackAddr() (string, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("sshmux: pick a forward port: %w", err)
	}
	defer l.Close()
	return l.Addr().String(), nil
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

// process is an ssh of its own that runs until it is stopped.
type process struct {
	cmd    *exec.Cmd
	stderr tail.Buffer
	exited chan struct{}
	err    error // set before exited closes
}

func (m *Master) startProcess(args []string) (*process, error) {
	p := &process{cmd: exec.Command(m.ssh, args...), exited: make(chan struct{})}
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

// lookupListener is listenerOf, which the tests can stand in for.
var lookupListener = listenerOf

// awaitListening returns once ssh listens at addr, or fails when it exits
// or ctx ends first. A unix socket in the private directory can only be
// ssh's, so a connection there tells, which reaches the remote end as one
// that sends nothing. A loopback port can be anyone's, and a connection
// would reach whoever took it first, so there the system's table of
// listeners has to name ssh; another program listening fails the forward.
func (p *process) awaitListening(ctx context.Context, network, addr string) error {
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if network == "tcp" {
			listening, ours, err := lookupListener(addr, p.cmd.Process.Pid)
			// A port that accepts while the table shows nobody on it means
			// the table cannot be read right here: fail rather than wait
			// for a listener it will never show. Read again first, in case
			// ssh began to listen in between.
			if err == nil && !listening && accepts(ctx, network, addr) {
				if listening, ours, err = lookupListener(addr, p.cmd.Process.Pid); err == nil && !listening {
					err = fmt.Errorf("%s accepts connections, but the system's table of listeners shows none there", addr)
				}
			}
			switch {
			case err != nil:
				p.stop()
				return err
			case listening && !ours:
				p.stop()
				return fmt.Errorf("another program listens on %s", addr)
			case ours:
				return nil
			}
		} else if accepts(ctx, network, addr) {
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
