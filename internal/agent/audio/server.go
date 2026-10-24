//go:build linux

package audio

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	serverTimeout = 5 * time.Second
	termGrace     = 2 * time.Second
	// maxSocketPath is what fits in sockaddr_un.sun_path with its NUL.
	maxSocketPath = 107
)

// errNoServer is what a host without pulseaudio gets in spawn mode.
var errNoServer = errors.New("no sound server for the new desktop: install pulseaudio on the host")

// server is a private PulseAudio daemon for a spawned desktop: one null
// sink as the default output, reachable only through a socket in the run
// dir and a cookie beside it.
//
// It runs as one process in its own group with Pdeathsig, so the kernel
// takes it down if the agent dies. The display package needs a watchdog
// too because its sessions fork; pulseaudio here forks nothing, since the
// startup script loads no module that would.
type server struct {
	dir, socket, cookie string

	cmd    *exec.Cmd
	exited chan struct{}
	err    error // valid once exited is closed
	stderr tail
	once   sync.Once
}

// startServer starts pulseaudio under runDir/pulse and returns once its
// socket accepts connections. ctx bounds startup only. extra goes on the
// daemon's command line after the agent's own options, which it overrides.
func startServer(ctx context.Context, runDir string, extra ...string) (_ *server, err error) {
	bin, err := exec.LookPath("pulseaudio")
	if err != nil {
		return nil, errNoServer
	}
	dir := filepath.Join(runDir, "pulse")
	s := &server{dir: dir, socket: filepath.Join(dir, "native"), cookie: filepath.Join(dir, "cookie")}
	if len(s.socket) > maxSocketPath {
		return nil, fmt.Errorf("pulseaudio: socket path %s is too long for a unix socket", s.socket)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("pulseaudio: %w", err)
	}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()
	cookie := make([]byte, cookieSize)
	rand.Read(cookie)
	if err := os.WriteFile(s.cookie, cookie, 0o600); err != nil {
		return nil, fmt.Errorf("pulseaudio: %w", err)
	}
	script := filepath.Join(dir, "robin.pa")
	if err := os.WriteFile(script, []byte(serverScript(s.socket, s.cookie)), 0o600); err != nil {
		return nil, fmt.Errorf("pulseaudio: %w", err)
	}

	s.cmd = exec.Command(bin, append([]string{
		"-n", "--file=" + script,
		"--daemonize=no", "--system=no", "--fail=yes",
		"--exit-idle-time=-1", "--disallow-exit=yes", "--disallow-module-loading=yes",
		"--use-pid-file=no", "--realtime=no", "--high-priority=no",
		"--log-target=stderr",
	}, extra...)...)
	s.cmd.Dir = dir
	s.cmd.Env = serverEnv(dir)
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	s.cmd.Stderr = &s.stderr
	if err := s.cmd.Start(); err != nil {
		return nil, fmt.Errorf("pulseaudio: %w", err)
	}
	s.exited = make(chan struct{})
	go func() {
		s.err = s.cmd.Wait()
		close(s.exited)
	}()

	ctx, cancel := context.WithTimeout(ctx, serverTimeout)
	defer cancel()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if c, err := net.Dial("unix", s.socket); err == nil {
			c.Close()
			return s, nil
		}
		select {
		case <-s.exited:
			return nil, s.exitError()
		case <-ctx.Done():
			return nil, fmt.Errorf("pulseaudio did not open its socket: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

// serverScript is the daemon's whole configuration. Without -n's default
// script nothing touches the host's sound hardware or Bluetooth. Rewinds
// are off because each one drops what the monitor had queued: with them a
// 20 ms recording got 70% of real time, so the RTP clock would drift.
func serverScript(socket, cookie string) string {
	return "load-module module-null-sink sink_name=robin rate=48000 channels=2 norewinds=1 sink_properties=device.description=robin\n" +
		"set-default-sink robin\n" +
		"load-module module-native-protocol-unix socket=" + quoteArg(socket) + " auth-anonymous=0 auth-cookie=" + quoteArg(cookie) + "\n"
}

// quoteArg quotes a module argument the way PulseAudio's parser reads it.
func quoteArg(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// serverEnv keeps the daemon's state, runtime files and D-Bus out of the
// user's own session: HOME and the runtime dir point into dir, and the
// session bus address points at nothing, so it neither collides with a
// sound server the user already runs nor autolaunches a bus of its own.
func serverEnv(dir string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + dir,
		"XDG_RUNTIME_DIR=" + dir,
		"XDG_CONFIG_HOME=" + filepath.Join(dir, "config"),
		"PULSE_RUNTIME_PATH=" + dir,
		"PULSE_STATE_PATH=" + filepath.Join(dir, "state"),
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + filepath.Join(dir, "no-bus"),
	}
}

// Env is what the desktop's programs need to play into the server. The
// cookie variable also keeps libpulse from writing one into ~/.config.
func (s *server) Env() []string {
	return []string{"PULSE_SERVER=unix:" + s.socket, "PULSE_COOKIE=" + s.cookie}
}

// Done is closed when the daemon exits.
func (s *server) Done() <-chan struct{} { return s.exited }

// Close stops the daemon, killing it if it ignores SIGTERM for termGrace,
// and removes its files. It is safe to call more than once.
func (s *server) Close() error {
	s.once.Do(func() {
		if s.cmd != nil && s.cmd.Process != nil {
			pgid := s.cmd.Process.Pid
			syscall.Kill(-pgid, syscall.SIGTERM)
			select {
			case <-s.exited:
			case <-time.After(termGrace):
				syscall.Kill(-pgid, syscall.SIGKILL)
				<-s.exited
			}
		}
		os.RemoveAll(s.dir)
	})
	return nil
}

// exitError describes how the daemon ended, with the end of its log.
func (s *server) exitError() error {
	status := "exited"
	if s.err != nil {
		status = s.err.Error()
	}
	if log := strings.TrimSpace(s.stderr.String()); log != "" {
		return fmt.Errorf("pulseaudio %s: %s", status, log)
	}
	return fmt.Errorf("pulseaudio %s", status)
}

// tail keeps the last few KiB written to it.
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
