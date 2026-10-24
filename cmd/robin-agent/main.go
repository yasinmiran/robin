//go:build linux

// Command robin-agent is the host half of robin. The CLI copies it
// to the host and runs it over SSH; it is not meant to be started by hand.
//
// serve runs on the SSH exec channel: it starts a session daemon, or finds
// the one to resume, and relays JSON-lines signaling between its stdin and
// stdout and the daemon's socket, logging to stderr. daemon owns the
// desktop, in a process that outlives the SSH connection; it exits, tearing
// down what it spawned, when the CLI says bye, the keep window runs out with
// no CLI attached, or end tells it to. As robin-agent send, or through the
// robin-send link a spawned session puts on its programs' PATH, it offers
// files to the session's browser instead.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/jezek/xgb"

	"github.com/yasinmiran/robin/internal/agent"
	"github.com/yasinmiran/robin/internal/agent/audio"
	"github.com/yasinmiran/robin/internal/agent/daemon"
	"github.com/yasinmiran/robin/internal/agent/files"
	"github.com/yasinmiran/robin/internal/agent/h264"
	"github.com/yasinmiran/robin/internal/agent/hwenc"
	"github.com/yasinmiran/robin/internal/protocol"
)

var version = "dev"

const usage = "usage: robin-agent serve [-v] [-keep d] [-resume id] [-detach setsid|systemd-run] | daemon ... | end id | send FILE... | version"

func main() {
	if filepath.Base(os.Args[0]) == agent.SendCommand {
		os.Exit(send(os.Args[1:]))
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println("robin-agent", version)
	case "serve":
		os.Exit(serve(os.Args[2:]))
	case "daemon":
		os.Exit(runDaemon(os.Args[2:]))
	case "end":
		os.Exit(end(os.Args[2:]))
	case "send":
		os.Exit(send(os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "robin-agent: unknown command %q\n%s\n", os.Args[1], usage)
		os.Exit(2)
	}
}

// runRoot is where sessions keep their directories: beside the agent exe.
func runRoot(exe string) string {
	return filepath.Join(filepath.Dir(exe), "run")
}

// executable is this binary's path.
func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate executable: %w", err)
	}
	return exe, nil
}

// serve is the CLI's end on the host: a proxy to the session daemon.
func serve(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	verbose := fs.Bool("v", false, "log at info level")
	keep := fs.Duration("keep", 0, "how long the desktop waits for the CLI to come back")
	resume := fs.String("resume", "", "resume the kept session with this id instead of starting one")
	detach := fs.String("detach", string(protocol.DetachSetsid), "how the daemon leaves the SSH session: setsid or systemd-run")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	exe, err := executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "robin-agent:", err)
		return 1
	}
	root := runRoot(exe)
	// SIGHUP arrives when sshd tears the channel down; it means the same as
	// stdin closing: this controller is gone, the daemon decides what next.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	// Once the SSH channel is gone, a write to stdout or stderr would kill
	// the proxy with SIGPIPE before it closes the daemon's socket cleanly.
	signal.Ignore(syscall.SIGPIPE)
	// Stdout belongs to signaling. Nothing else may write there.
	out := os.Stdout
	os.Stdout = os.Stderr

	var dir string
	fromStart := false
	if *resume == "" {
		_, dir, err = daemon.Start(ctx, daemon.Spawn{
			Exe: exe, RunRoot: root, Keep: *keep, Verbose: *verbose, Detach: protocol.Detach(*detach), Env: daemonEnv(),
		})
		fromStart = true
	} else {
		dir, err = daemon.Find(root, *resume)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "robin-agent:", err)
		return 1
	}
	if err := daemon.Proxy(ctx, dir, os.Stdin, out, os.Stderr, fromStart); err != nil {
		fmt.Fprintln(os.Stderr, "robin-agent:", err)
		return 1
	}
	return 0
}

// daemonEnv is what a daemon under systemd-run needs from this environment,
// which its unit does not inherit: where tools, the home and the runtime
// directory are, the locale, and where a coverage build writes.
func daemonEnv() []string {
	var env []string
	for _, name := range []string{"PATH", "HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "LANG", "GOCOVERDIR"} {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// runDaemon owns one session until it ends.
func runDaemon(args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	verbose := fs.Bool("v", false, "log at info level")
	id := fs.String("id", "", "session id")
	dir := fs.String("dir", "", "the session's run directory, already made")
	keep := fs.Duration("keep", 0, "how long to wait for a controller to come back")
	if err := fs.Parse(args); err != nil || *id == "" || *dir == "" {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	// The log lives in the run directory, where serve tails it to the CLI.
	// It takes the place of stderr, so slog and every child write there.
	logFile, err := os.OpenFile(filepath.Join(*dir, daemon.LogFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "robin-agent:", err)
		return 1
	}
	syscall.Dup3(int(logFile.Fd()), 2, 0)
	syscall.Dup3(int(logFile.Fd()), 1, 0)
	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	// xgb reports a dead X server on its own logger; the session already
	// reports that through the display watcher.
	xgb.Logger = stdlog.New(io.Discard, "", 0)

	exe, err := executable()
	if err != nil {
		log.Error("locate executable", "err", err)
		return 1
	}
	// The daemon has no terminal and no SSH channel: a hangup means nothing
	// to it. SIGTERM is how end stops it.
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := daemon.Listen(*dir, filepath.Base(exe), log)
	if err != nil {
		log.Error("listen", "err", err)
		os.RemoveAll(*dir)
		return 1
	}
	defer ln.Close()
	// Serve removes the directory once a session ran; before that it is ours.
	defer os.RemoveAll(*dir)
	err = agent.Serve(ctx, ln, agent.Options{
		RunRoot: filepath.Dir(*dir), RunDir: *dir, Keep: *keep,
		StateDir: filepath.Dir(exe),
		Build:    version,
		Encoders: map[string]agent.NewEncoderFunc{
			protocol.EncoderSoftware: newEncoder,
			protocol.EncoderVAAPI:    hardware(hwenc.NewOpener(hwenc.VAAPI, log)),
			protocol.EncoderNVENC:    hardware(hwenc.NewOpener(hwenc.NVENC, log)),
		},
		NewAudio: newAudio,
		Log:      log,
	})
	if err != nil {
		log.Error("session ended", "err", err)
		return 1
	}
	log.Info("session ended")
	return 0
}

// end stops the daemon of the session named, and waits for it to go. It
// exits protocol.ExitNoSession when the host has no such session.
func end(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	exe, err := executable()
	if err == nil {
		err = daemon.End(context.Background(), runRoot(exe), args[0])
	}
	var noSession *daemon.NoSessionError
	switch {
	case errors.As(err, &noSession):
		fmt.Fprintln(os.Stderr, "robin-agent:", err)
		return protocol.ExitNoSession
	case err != nil:
		fmt.Fprintln(os.Stderr, "robin-agent:", err)
		return 1
	}
	return 0
}

// send offers files to the browser of a session of this agent: the one
// whose programs it runs among, else the only one running.
func send(args []string) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "robin-send:", err)
		return 1
	}
	return files.Main(args, filepath.Join(filepath.Dir(exe), "run"), os.Getenv, os.Stdout, os.Stderr)
}

func newEncoder(width, height, fps, bitrate int) (agent.Encoder, error) {
	return h264.New(width, height, fps, bitrate)
}

func newAudio(ctx context.Context, o agent.AudioOptions) (agent.AudioSource, []string, error) {
	src, env, err := audio.Start(ctx, audio.Options{RunDir: o.RunDir, Attached: o.Attached, Log: o.Log})
	if err != nil {
		return nil, nil, err // not src: a nil *audio.Source would make a non-nil interface
	}
	return src, env, nil
}

func hardware(o *hwenc.Opener) agent.NewEncoderFunc {
	return func(width, height, fps, bitrate int) (agent.Encoder, error) {
		e, err := o.Open(width, height, fps, bitrate)
		if err != nil {
			// Not e: a nil *hwenc.Encoder would be a non-nil Encoder.
			return nil, err
		}
		return e, nil
	}
}
