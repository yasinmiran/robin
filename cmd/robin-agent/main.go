//go:build linux

// Command robin-agent is the host half of robin. The CLI copies it
// to the host and runs it over SSH; it is not meant to be started by hand.
//
// serve runs on the SSH exec channel: it starts a session daemon, or finds
// the one to resume, and relays JSON-lines signaling between its stdin and
// stdout and the daemon's socket, logging to stderr. daemon owns the
// desktop, in a process that outlives the SSH connection; it exits, tearing
// down what it spawned, when the CLI says bye, the keep window runs out with
// no CLI attached, or end tells it to.
package main

import (
	"context"
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
	"github.com/yasinmiran/robin/internal/agent/h264"
	"github.com/yasinmiran/robin/internal/agent/hwenc"
	"github.com/yasinmiran/robin/internal/protocol"
)

var version = "dev"

const usage = "usage: robin-agent serve [-v] [-keep d] [-resume id] [-detach setsid|systemd-run] | daemon ... | end id | version"

func main() {
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
	default:
		fmt.Fprintf(os.Stderr, "robin-agent: unknown command %q\n%s\n", os.Args[1], usage)
		os.Exit(2)
	}
}

// runRoot is where sessions keep their directories: beside the agent.
func runRoot() (exe, root string, err error) {
	exe, err = os.Executable()
	if err != nil {
		return "", "", fmt.Errorf("locate executable: %w", err)
	}
	return exe, filepath.Join(filepath.Dir(exe), "run"), nil
}

// serve is the CLI's end on the host: a proxy to the session daemon.
func serve(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	verbose := fs.Bool("v", false, "log at info level")
	keep := fs.Duration("keep", 0, "how long the desktop waits for the CLI to come back")
	resume := fs.String("resume", "", "resume the kept session with this id instead of starting one")
	detach := fs.String("detach", daemon.DetachSetsid, "how the daemon leaves the SSH session: setsid or systemd-run")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	exe, root, err := runRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "robin-agent:", err)
		return 1
	}
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
			Exe: exe, RunRoot: root, Keep: *keep, Verbose: *verbose, Detach: *detach, Env: daemonEnv(),
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

// daemonEnv is what a daemon under systemd-run needs from this environment:
// where tools are, and where a coverage build writes.
func daemonEnv() []string {
	var env []string
	for _, name := range []string{"PATH", "GOCOVERDIR"} {
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
	syscall.Dup2(int(logFile.Fd()), 2)
	syscall.Dup2(int(logFile.Fd()), 1)
	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	// xgb reports a dead X server on its own logger; the session already
	// reports that through the display watcher.
	xgb.Logger = stdlog.New(io.Discard, "", 0)

	exe, _, err := runRoot()
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

// end stops the daemon of the session named, and waits for it to go.
func end(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	_, root, err := runRoot()
	if err == nil {
		err = daemon.End(filepath.Join(root, filepath.Base(args[0])))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "robin-agent:", err)
		return 1
	}
	return 0
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
