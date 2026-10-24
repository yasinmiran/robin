package sshmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/yasinmiran/robin/internal/tail"
)

// Forward is a local endpoint that leads to an address on the remote host.
type Forward struct {
	network, addr string
}

// Dial opens a connection through the forward.
func (f *Forward) Dial() (net.Conn, error) {
	return net.Dial(f.network, f.addr)
}

// Forward makes remote, "host:port" or a unix socket path on the remote
// host, reachable from this machine until Close. Over a master it is a unix
// socket in the master's private directory. Without one it is an ssh -N -L
// of its own on a loopback TCP port, since Windows OpenSSH forwards no unix
// sockets, and any user of this machine can connect to that port.
func (m *Master) Forward(ctx context.Context, remote string) (*Forward, error) {
	if remote == "" {
		return nil, errors.New("sshmux: empty forward target")
	}
	if !m.mux {
		return m.forwardOwn(ctx, remote)
	}
	m.mu.Lock()
	m.sockets++
	sock := m.socketPath(fmt.Sprintf("fwd%d.sock", m.sockets))
	m.mu.Unlock()
	if err := m.forwardLocal(ctx, sock, remote); err != nil {
		return nil, err
	}
	return &Forward{"unix", sock}, nil
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

// forwardOwn starts an ssh that does nothing but forward a loopback port to
// remote, and returns once the port accepts connections.
func (m *Master) forwardOwn(ctx context.Context, remote string) (*Forward, error) {
	local, err := freeLoopbackAddr()
	if err != nil {
		return nil, err
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
	if err := p.awaitListening(ctx, local); err != nil {
		return nil, fmt.Errorf("sshmux: forward to %s: %w", remote, err)
	}
	return &Forward{"tcp", local}, nil
}

// freeLoopbackAddr returns a loopback TCP address nobody listens on: a port
// the system just handed out, released for ssh to take. ssh binds it only
// once it has logged in, so another program could take it meanwhile; the
// system hands ports out in turn, which makes that unlikely, and
// ExitOnForwardFailure then ends the forward.
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

// awaitListening returns once addr accepts a connection, or fails when the
// process exits or ctx ends first. The connection it opens and closes reaches
// the remote end as one that sends nothing.
func (p *process) awaitListening(ctx context.Context, addr string) error {
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close()
			return nil
		}
		select {
		case <-p.exited:
			msg := strings.TrimSpace(p.stderr.String())
			if msg == "" {
				msg = "no output"
			}
			return fmt.Errorf("ssh failed (%v): %s", p.err, msg)
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
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
