//go:build linux

// Command remotejump-agent is the host half of remotejump. The CLI copies it
// to the host and runs it over SSH; it is not meant to be started by hand.
//
// It speaks JSON-lines signaling on stdin and stdout, logs to stderr, and
// exits, tearing down the display it spawned, when stdin closes.
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

	"github.com/yasinmiran/remotejump/internal/agent"
	"github.com/yasinmiran/remotejump/internal/agent/audio"
	"github.com/yasinmiran/remotejump/internal/agent/h264"
	"github.com/yasinmiran/remotejump/internal/protocol"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: remotejump-agent serve [-v] | version")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println("remotejump-agent", version)
	case "serve":
		os.Exit(serve(os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "remotejump-agent: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func serve(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	verbose := fs.Bool("v", false, "log at info level")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// xgb reports a dead X server on its own logger; the session already
	// reports that through the display watcher.
	xgb.Logger = stdlog.New(io.Discard, "", 0)

	exe, err := os.Executable()
	if err != nil {
		log.Error("locate executable", "err", err)
		return 1
	}
	// SIGHUP arrives when sshd tears the channel down; it means the same as
	// stdin closing.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	// Once the SSH channel is gone, a write to stdout or stderr would kill the
	// process with SIGPIPE before it removes the display and its files.
	signal.Ignore(syscall.SIGPIPE)
	// Stdout belongs to signaling. Nothing else may write there.
	out := os.Stdout
	os.Stdout = os.Stderr

	err = agent.Run(ctx, os.Stdin, out, agent.Options{
		RunRoot:  filepath.Join(filepath.Dir(exe), "run"),
		Build:    version,
		Encoders: map[string]agent.NewEncoderFunc{protocol.EncoderSoftware: newEncoder},
		NewAudio: newAudio,
		Log:      log,
	})
	if err != nil {
		log.Error("session ended", "err", err)
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
