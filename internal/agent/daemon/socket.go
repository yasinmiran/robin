//go:build linux

// Package daemon keeps a robin session alive on the host after the SSH
// connection that started it is gone. The session runs in robin-agent
// daemon, a process of its own in its own session (setsid) or transient
// systemd unit, and listens for controllers on a unix socket in its run
// directory; robin-agent serve, on the SSH exec channel, starts or finds
// that daemon and relays its stdio to the socket.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/yasinmiran/robin/internal/agent"
	"github.com/yasinmiran/robin/internal/protocol"
)

// Names of the files in a session's run directory, beside session.json.
const (
	SocketFile = agent.SocketFile
	LogFile    = "agent.log"
)

// handoffWait is how long a connection waits for the session to want a
// controller before it is turned away: long enough for the daemon to get
// from one controller to the next, short enough that a CLI trying while
// another is attached hears so promptly.
const handoffWait = 2 * time.Second

// errBusy is sent to a controller that connects while another is attached,
// with protocol.CodeBusy.
var errBusy = errors.New("another CLI is connected to this session")

// NoSessionError says the host has no session with that id.
type NoSessionError struct{ ID string }

func (e *NoSessionError) Error() string { return "no session " + e.ID + " on this host" }

// Listener accepts controllers on the session's socket. It is the
// agent.Controllers of a daemon.
type Listener struct {
	dir   string
	agent string
	ln    net.Listener
	uid   int // the only user whose controllers are admitted
	log   *slog.Logger
	conns chan *protocol.Conn
}

// Listen creates the socket in dir, a 0700 directory that already exists,
// and starts accepting. agent is the file name of the binary this daemon
// runs from, recorded for the CLI to compare with its own.
func Listen(dir, agent string, log *slog.Logger) (*Listener, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := checkPrivate(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, SocketFile)
	if len(path) >= len(syscall.RawSockaddrUnix{}.Path) {
		return nil, fmt.Errorf("daemon: socket path %q is too long for a unix socket", path)
	}
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("daemon: listen: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	l := &Listener{dir: dir, agent: agent, ln: ln, uid: os.Getuid(), log: log, conns: make(chan *protocol.Conn)}
	go l.accept()
	return l, nil
}

// checkPrivate refuses a run directory anyone else could reach into.
func checkPrivate(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("daemon: %s is not a directory", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("daemon: %s is %o, want 0700", dir, fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("daemon: %s belongs to uid %d, not us", dir, st.Uid)
	}
	return nil
}

func (l *Listener) accept() {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			return
		}
		go l.admit(c)
	}
}

// admit hands one connection to the session, if it is ours and the session
// wants a controller now.
func (l *Listener) admit(c net.Conn) {
	if err := checkPeer(c, l.uid); err != nil {
		l.log.Warn("controller refused", "err", err)
		c.Close()
		return
	}
	conn := protocol.NewConn(c, c, protocol.DefaultPingInterval, protocol.DefaultIdleTimeout)
	// protocol.Conn never closes its stream; the socket goes when it does.
	go func() {
		<-conn.Done()
		c.Close()
	}()
	select {
	case l.conns <- conn:
	case <-time.After(handoffWait):
		l.log.Warn("controller refused", "err", errBusy)
		conn.Send(protocol.Message{Type: protocol.TypeError, Error: errBusy.Error(), Code: protocol.CodeBusy})
		conn.Close()
	}
}

// checkPeer accepts only a connection from a process of uid: the run
// directory already keeps everyone else out, and this makes sure of it.
func checkPeer(c net.Conn, uid int) error {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("daemon: not a unix connection: %T", c)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var cred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if credErr != nil {
		return fmt.Errorf("daemon: peer credentials: %w", credErr)
	}
	return checkUID(cred, uid)
}

func checkUID(cred *syscall.Ucred, us int) error {
	if int(cred.Uid) != us {
		return fmt.Errorf("daemon: connection from uid %d, not %d", cred.Uid, us)
	}
	return nil
}

// Next returns the next controller that connects.
func (l *Listener) Next(ctx context.Context) (*protocol.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Changed records the session's state in session.json for the probe.
func (l *Listener) Changed(s protocol.Session) {
	s.Agent = l.agent
	if err := agent.WriteSession(l.dir, s); err != nil {
		l.log.Warn("write session.json", "err", err)
	}
}

// Close stops accepting.
func (l *Listener) Close() error { return l.ln.Close() }
