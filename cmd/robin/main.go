// Command robin opens a remote Linux desktop in the browser, using
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
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/yasinmiran/robin/internal/gui"
	"github.com/yasinmiran/robin/internal/protocol"
	"github.com/yasinmiran/robin/internal/relay"
)

var version = "dev"

const defaultSTUN = "stun:stun.l.google.com:19302"

const usage = `usage: robin gui [flags] [user@]host [-- ssh-args...]
       robin version

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
		fmt.Println("robin", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "robin: unknown command %q\n\n%s", os.Args[1], usage)
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
	maxSize := fs.String("max-size", "", fmt.Sprintf("largest remote resolution, WxH (default %dx%d, %dx%d on hosts with %d cores or fewer)",
		protocol.DefaultMaxWidth, protocol.DefaultMaxHeight, protocol.SmallHostMaxWidth, protocol.SmallHostMaxHeight, protocol.SmallHostCPUs))
	fps := fs.Int("fps", 0, fmt.Sprintf("frame rate cap (default %d, %d on small hosts)", protocol.DefaultFPS, protocol.SmallHostFPS))
	maxKbps := fs.Int("max-kbps", 0, fmt.Sprintf("video bitrate ceiling in kbit/s (default %d)", protocol.DefaultMaxKbps))
	session := fs.String("session", "", "command to run in the spawned display (default: the lightest desktop found)")
	attach := fs.String("attach", "", `stream a desktop already running on the host instead of starting one: an X11 display such as ":0", "auto" for your active session, or "wayland"`)
	monitor := fs.String("monitor", protocol.MonitorPrimary, `X11 desktops attached to with --attach: the monitor to show first, by name (such as DP-1) or number (1, 2, ...), "primary", or "all" for every monitor as one picture; the page switches between them. Wayland shows the monitor picked in its sharing dialog`)
	audio := fs.Bool("audio", true, "stream the desktop's sound as well")
	hidpi := fs.Bool("hidpi", false, "size the remote display in device pixels: sharper on HiDPI screens, more to encode (raise --max-size to go native)")
	encoder := fs.String("encoder", protocol.EncoderAuto, "video encoder on the host: auto (the GPU where the host has one and it pays, else software), software, vaapi or nvenc")
	stun := fs.String("stun", defaultSTUN, `comma-separated STUN servers for NAT traversal, or "none"`)
	listen := fs.String("listen", "127.0.0.1:0", "loopback address for the browser page")
	noBrowser := fs.Bool("no-browser", false, "print the page URL on stdout instead of opening a browser")
	sshPath := fs.String("ssh", "ssh", "ssh client to use")
	sshMux := fs.Bool("ssh-mux", defaultSSHMux(runtime.GOOS), "carry everything over one ssh ControlMaster connection; false gives each command and forward a connection of its own, the default on Windows, whose OpenSSH has no ControlMaster")
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
		Dest: dest, SSHPath: *sshPath, SSHArgs: sshArgs, NoMux: !*sshMux,
		Transport: t, MaxWidth: w, MaxHeight: h, FPS: *fps, MaxKbps: *maxKbps,
		Session: *session, Attach: *attach, Monitor: *monitor, Audio: *audio, Encoder: *encoder, HiDPI: *hidpi, STUN: parseSTUN(*stun),
		Listen: *listen, NoBrowser: *noBrowser, Verbose: *verbose,
		Stdout: os.Stdout, Stderr: os.Stderr, Log: log,
	})
	if err != nil {
		return fail(err)
	}
	return 0
}

// defaultSSHMux reports whether the ssh client on goos multiplexes with
// ControlMaster. Windows OpenSSH does not.
func defaultSSHMux(goos string) bool {
	return goos != "windows"
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
	fmt.Fprintln(os.Stderr, "robin:", err)
	return 1
}
