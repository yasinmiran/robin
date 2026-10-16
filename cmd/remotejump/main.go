// Command remotejump opens a remote Linux desktop in the browser, using
// nothing on the host but the SSH access the user already has.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/yasinmiran/remotejump/internal/gui"
	"github.com/yasinmiran/remotejump/internal/relay"
)

var version = "dev"

const defaultSTUN = "stun:stun.l.google.com:19302"

const usage = `usage: remotejump gui [flags] [user@]host [-- ssh-args...]
       remotejump version

gui opens a desktop on host in your browser. It connects with your ssh
config and agent, copies a small agent to the host, starts a headless X
session there and streams it over WebRTC. Media goes direct over UDP when it
can and through the SSH connection when it cannot.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "gui":
		os.Exit(runGUI(os.Args[2:]))
	case "version", "--version", "-version":
		fmt.Println("remotejump", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "remotejump: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func runGUI(args []string) int {
	fs := flag.NewFlagSet("gui", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usage+"\nflags:\n")
		fs.PrintDefaults()
	}
	transport := fs.String("transport", "auto", "media path: auto (direct UDP, SSH tunnel as fallback), direct, or tunnel")
	maxSize := fs.String("max-size", "", "largest remote resolution, WxH (default 1920x1080, 1280x720 on hosts with two cores or fewer)")
	fps := fs.Int("fps", 0, "frame rate cap (default 30, 20 on small hosts)")
	maxKbps := fs.Int("max-kbps", 0, "video bitrate ceiling in kbit/s (default 10000)")
	session := fs.String("session", "", "command to run in the spawned display (default: the lightest desktop found)")
	stun := fs.String("stun", defaultSTUN, `comma-separated STUN servers for NAT traversal, or "none"`)
	listen := fs.String("listen", "127.0.0.1:0", "loopback address for the browser page")
	noBrowser := fs.Bool("no-browser", false, "print the page URL on stdout instead of opening a browser")
	sshPath := fs.String("ssh", "ssh", "ssh client to use")
	verbose := fs.Bool("v", false, "verbose logging, including the agent's log")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return 2
	}
	dest, sshArgs := rest[0], rest[1:]
	if len(sshArgs) > 0 && sshArgs[0] == "--" {
		sshArgs = sshArgs[1:]
	}

	t, err := relay.ParseTransport(*transport)
	if err != nil {
		return fail(err)
	}
	var w, h int
	if *maxSize != "" {
		if w, h, err = parseSize(*maxSize); err != nil {
			return fail(err)
		}
	}

	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	err = gui.Run(ctx, gui.Options{
		Dest: dest, SSHPath: *sshPath, SSHArgs: sshArgs,
		Transport: t, MaxWidth: w, MaxHeight: h, FPS: *fps, MaxKbps: *maxKbps,
		Session: *session, STUN: parseSTUN(*stun),
		Listen: *listen, NoBrowser: *noBrowser, Verbose: *verbose,
		Stdout: os.Stdout, Stderr: os.Stderr, Log: log,
	})
	if err != nil {
		return fail(err)
	}
	return 0
}

func parseSize(s string) (int, int, error) {
	ws, hs, ok := strings.Cut(strings.ToLower(s), "x")
	w, werr := strconv.Atoi(ws)
	h, herr := strconv.Atoi(hs)
	if !ok || werr != nil || herr != nil || w < 64 || h < 64 {
		return 0, 0, fmt.Errorf("bad size %q, want WxH such as 1920x1080", s)
	}
	return w, h, nil
}

func parseSTUN(s string) []string {
	if s == "" || s == "none" {
		return nil
	}
	var out []string
	for _, u := range strings.Split(s, ",") {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, u)
		}
	}
	return out
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "remotejump:", err)
	return 1
}
