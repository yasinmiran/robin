//go:build linux

package wayland

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/godbus/dbus/v5"

	"github.com/yasinmiran/robin/internal/agent/xdg"
)

// sessionBus connects to the desktop session's bus. The socket in the runtime
// directory comes first: an SSH login can carry a DBUS_SESSION_BUS_ADDRESS
// of its own that the desktop's portal is not on.
func sessionBus() (*dbus.Conn, error) {
	sock := filepath.Join(xdg.RuntimeDir(), "bus")
	addr := "unix:path=" + sock
	if fi, err := os.Stat(sock); err != nil || fi.Mode().Type() != os.ModeSocket {
		if addr = os.Getenv("DBUS_SESSION_BUS_ADDRESS"); addr == "" {
			return nil, fmt.Errorf("no session bus at %s: is a desktop session running for this user?", sock)
		}
	}
	conn, err := dbus.Connect(addr)
	if err != nil {
		return nil, fmt.Errorf("connect to the session bus: %w", err)
	}
	return conn, nil
}

// signals fans the connection's one signal channel out by object path and
// member, so a goroutine waiting for one signal never consumes another's.
type signals struct {
	conn *dbus.Conn

	mu     sync.Mutex
	subs   map[sigKey][]chan *dbus.Signal
	closed bool
}

type sigKey struct {
	path dbus.ObjectPath
	name string // interface.member
}

func newSignals(conn *dbus.Conn) *signals {
	s := &signals{conn: conn, subs: map[sigKey][]chan *dbus.Signal{}}
	ch := make(chan *dbus.Signal, 64)
	conn.Signal(ch)
	go s.dispatch(ch)
	return s
}

// dispatch runs until the connection closes, then closes every subscriber.
func (s *signals) dispatch(ch <-chan *dbus.Signal) {
	for sig := range ch {
		s.mu.Lock()
		for _, c := range s.subs[sigKey{sig.Path, sig.Name}] {
			select {
			case c <- sig:
			default: // a subscriber that stopped reading loses signals, not the bus
			}
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for _, cs := range s.subs {
		for _, c := range cs {
			close(c)
		}
	}
	s.subs = nil
}

// watch subscribes to signal iface.member on path. The match rule is in
// place when it returns, so a method called afterwards cannot answer before
// the subscription exists. stop ends the subscription and closes c, as
// does the connection closing.
func (s *signals) watch(path dbus.ObjectPath, iface, member string) (c <-chan *dbus.Signal, stop func(), err error) {
	opts := []dbus.MatchOption{
		dbus.WithMatchObjectPath(path), dbus.WithMatchInterface(iface), dbus.WithMatchMember(member),
	}
	if err := s.conn.AddMatchSignal(opts...); err != nil {
		return nil, nil, err
	}
	ch := make(chan *dbus.Signal, 16)
	key := sigKey{path, iface + "." + member}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, dbus.ErrClosed
	}
	s.subs[key] = append(s.subs[key], ch)
	s.mu.Unlock()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			s.conn.RemoveMatchSignal(opts...)
			s.mu.Lock()
			defer s.mu.Unlock()
			subs := s.subs[key]
			for i, c := range subs {
				if c == ch {
					s.subs[key] = append(subs[:i], subs[i+1:]...)
					close(ch)
					break
				}
			}
		})
	}
	return ch, stop, nil
}
