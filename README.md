# remotejump

`remotejump gui <host>` opens a Linux desktop from `<host>` in your browser.
The only thing the host needs beforehand is SSH access you already have: the
CLI connects with your own `ssh`, copies a small static agent over, starts a
headless X session as you, and streams it back over WebRTC. No signaling
server, no ports opened for auth, no root.

![A remotejump session: an xterm on the remote desktop, the status tab at the top](docs/img/session.png)

Media goes straight over UDP when the network allows it. When it does not
(UDP blocked, or a host you only reach through `ProxyJump`), it rides the SSH
connection instead, and if UDP disappears mid-session the browser moves onto
that path by itself in about three seconds.

## Install

From a checkout, with Go 1.25 or newer:

```sh
make
```

That fetches a pinned zig toolchain and builds openh264 and libopus the first
time (about half a minute, cached under `.cache/`), builds static agents for
linux/amd64 and linux/arm64, and embeds both in `bin/remotejump`. `make dist`
cross-builds the CLI for macOS and Linux on amd64 and arm64 into `dist/`. The
CLI itself is pure Go; only the agents need the C toolchain, and the build
downloads it, so there is nothing to install by hand on Linux or macOS.

## Usage

```sh
remotejump gui alice@build-box
remotejump gui build-box -- -p 2222 -i ~/.ssh/other_key   # anything after -- goes to ssh
remotejump gui --max-size 2560x1440 --fps 60 workstation
remotejump gui --no-browser lab-node                     # print the URL instead of opening it
remotejump gui --attach auto workstation                 # the desktop you are logged in to there
```

Your `~/.ssh/config`, agent, `ProxyJump`, `ProxyCommand`, certificates and
password or 2FA prompts all work as they do for plain `ssh`, because it is
plain `ssh`: one ControlMaster connection carries the probe, the agent upload,
the agent itself and the tunnel. The browser tab can be reloaded; the remote
session survives until you press Ctrl+C in the terminal.

With `--attach`, nothing new is started: the agent streams an X11 desktop that
is already running on the host, usually the one you are logged in to at the
machine. `auto` picks the display of your active session, or the one X server
you own when the session does not name one (GDM never does), and `:1` names a
display outright. The cookie comes from wherever startx, LightDM, GDM, mutter
or SDDM left it; failing that, the agent gets in the way an
`xhost +si:localuser:$USER` grant allows.

That desktop stays shared: whoever sits at the machine sees the pointer move.
It keeps its own resolution, too. Your browser window scales the picture
instead of resizing the screen, and a desktop larger than `--max-size` (a 4K
monitor, several monitors side by side) is scaled down to fit, with the pointer
mapped back onto the full-size screen. Ending the session closes the agent's X
connection and nothing else; autorepeat and the keys it bound for characters
missing from the keymap go back to how they were, and clipboard text pasted
from the browser is handed to the desktop's clipboard manager if one runs.
The desktop's sound comes along. A browser only starts a video by itself
while it is muted, so the page opens silent and the status tab shows a
speaker: click it to hear the session, and again after a reload. When the
host cannot provide sound the session runs without it, and the CLI says why.

| Flag | Default | |
|---|---|---|
| `--transport` | `auto` | `direct` for UDP only, `tunnel` to force the SSH path |
| `--max-size` | `1920x1080` | largest remote resolution; the display follows your window up to this. 1280x720 on hosts with two cores or fewer |
| `--fps` | `30` | frame rate cap (20 on small hosts) |
| `--hidpi` | off | size the remote display in the browser's device pixels: sharp text on Retina screens, about four times the pixels to encode. Raise `--max-size` to go fully native |
| `--max-kbps` | `10000` | video bitrate ceiling; congestion control works below it |
| `--encoder` | `software` | `vaapi` or `nvenc` to encode on the host's GPU; see [Hardware encoding](#hardware-encoding) |
| `--session` | lightest found | command to run in the display, e.g. `startxfce4` |
| `--attach` | | stream a desktop already running on the host: `auto`, or an X display such as `:1` |
| `--audio` | `true` | stream the desktop's sound; `--audio=false` leaves it out |
| `--stun` | Google's public server | STUN servers for NAT traversal, comma-separated, or `none` |
| `--no-browser` | | print the page URL on stdout instead of opening a browser |
| `--listen` | `127.0.0.1:0` | address for the local page; loopback only |
| `-v` | | log the CLI and the agent to stderr |

## What the host needs

Linux on x86_64 or arm64, an SSH server you can log in to, and for the
headless session: `Xvfb` plus something to run in it, either a desktop
(`xfce4-session`) or a window manager (`openbox`, `fluxbox`, `icewm`, `twm`)
with a terminal (`xterm`). If anything is missing the CLI says what, with the
install line for that distro, for example:

```
sudo apt install xvfb openbox xterm
```

Attaching to a running desktop needs none of that, only the desktop.

For sound in a spawned session the host also needs `pulseaudio`. The agent starts
a private one with a null sink as its only output and its socket in the
agent's run directory, so it touches neither the sound hardware nor a sound
server you already run, and it stops with the session. Attached to a running
desktop, the agent records that desktop's own sound server instead,
PulseAudio or PipeWire alike, since PipeWire speaks the same protocol through
`pipewire-pulse`.

No GPU or monitor is needed; the session renders in software, and a light
window manager keeps that cheap. Software H.264 at 1080p30 costs one to two
cores, which is why small hosts get a lower default size and frame rate.

The agent goes to `~/.cache/remotejump` (or `$XDG_RUNTIME_DIR/remotejump`, or
`/tmp/remotejump-$UID` when the home directory is mounted `noexec`), checked
by hash and reused across sessions. It runs as you, starts its own display on
a free number, and when the SSH channel closes it kills everything it started
and removes its files.

## Hardware encoding

```sh
remotejump gui --encoder vaapi build-box   # Intel or AMD, through VA-API
remotejump gui --encoder nvenc gpu-box     # NVIDIA
```

The agent is a static binary and cannot load the host's GPU libraries, so
for these it pipes frames through a tool the host already has: ffmpeg, or
GStreamer's `gst-launch-1.0`. VA-API needs a render node in `/dev/dri` you
can open (usually that means being in the `render` group) and either ffmpeg
built with `h264_vaapi` or `gst-launch-1.0` with GStreamer's va or vaapi
plugin and gst-plugins-good. NVENC needs the NVIDIA driver loaded, and
ffmpeg with `h264_nvenc` or GStreamer's nvcodec plugin. On Debian and
Ubuntu, `sudo apt install ffmpeg` brings both encoders. The CLI checks all
of this against the probe before it deploys anything and says what is
missing; the agent then tries each route the host offers on a test frame,
ffmpeg first, and if none works the error lists why each one failed.

Software stays the default, and nothing switches to hardware by itself,
because on a reasonably fast CPU it saves little. On an i7-13800H with its
iGPU, a session scrolling a terminal cost about a fifth of a core at
1440x900 and about one core at 3840x2160 with either encoder: copying each
frame out to the tool eats most of what the GPU saves. It pays where
openh264 is the bottleneck, on slow CPUs and on busy 4K content (28 ms of
CPU per frame for openh264 against 12 ms through ffmpeg). What it costs:

- A keyframe the browser asks for takes about 40 ms instead of 5, because a
  running tool cannot be asked for one and is restarted instead.
- Each frame spends 5 ms in the encoder at 1080p and about 20 ms at 2160p
  through ffmpeg, against 1 to 3 and 5 to 10 ms for openh264; GStreamer's
  vaapi plugin takes 31 to 34 ms at 2160p, a whole frame at 30 fps.
- Congestion control is followed by skipping frames, and the tool is
  restarted at the new bitrate when the target has moved twofold, at most
  every two seconds.
- The first GStreamer run on a host builds its plugin registry, which can
  take a few seconds.
- NVENC follows NVIDIA's documented options but has not been run on NVIDIA
  hardware yet.

The measurements and the reasons behind the design are in the
[ADR](docs/adr/0001-architecture.md#hardware-h264-through-a-host-tool-and-only-when-asked-for).

## What the client needs

macOS or Linux with OpenSSH (ControlMaster support, so not the Windows
build), and a browser that decodes H.264 in WebRTC: Chrome, Edge, Safari, or
Firefox with its OpenH264 plugin. Chrome is what the end-to-end tests drive.

## How it works

1. `ssh -M` opens the one connection everything else uses.
2. A POSIX shell probe reports the distro, architecture, live sessions, GPU,
   which spawn-mode tools exist, and a directory where the agent can run.
3. The agent for that architecture is uploaded over SFTP, verified by
   SHA-256 on the host and renamed into place.
4. The agent starts on an exec channel. Its stdin and stdout carry signaling
   as JSON lines, with a heartbeat both ways so a dead connection tears the
   session down instead of leaving a display running.
5. It starts Xvfb and a session, or attaches to the X display already running,
   captures with MIT-SHM and DAMAGE, encodes with openh264, and sends over a
   Pion peer connection. Sound comes from the monitor of a PulseAudio sink,
   read over its native protocol in Go, and goes out as Opus from libopus.
   Keyboard, pointer, clipboard and resize come back over a data channel and
   go in through XTest and RandR.
6. The CLI serves the page on loopback and relays offer, answer and
   candidates between it and the agent, adding one passive TCP candidate that
   leads into an SSH forward to the agent.

The decisions behind each of these, and what measurement changed along the
way, are in [docs/adr/0001-architecture.md](docs/adr/0001-architecture.md).

## Security

The agent's DTLS fingerprint reaches the browser only through SSH and the
local page, and the CLI pins it for the session, so nobody who can merely
reach the UDP or TCP path can stand in for the agent. The page and its
websocket listen on 127.0.0.1 and require a random 256-bit token from the URL
fragment, an exact `Origin` and `Host`, and allow one browser at a time. On
the host, the agent's ICE-TCP listener binds loopback and only accepts ICE
connections carrying the session's credentials; the X display is protected by
a fresh cookie in a 0600 file; the session's sound server listens only on a
socket inside the run directory; the agent and its run directory are 0700.
Input from the page is limited to what a keyboard, a pointer and a clipboard
can do in that session.

One exposure is on the client side: to open your browser the CLI hands the
page URL, token included, to `open` or `xdg-open`, so for that moment another
user logged in to the same client machine could read it from the process
list. On a shared client, use `--no-browser` and open the printed URL yourself.

## Testing

```sh
make test     # unit and integration tests; the encoders against real openh264 and libopus on linux/amd64
make test-x   # capture, input, display, sound server and agent session tests against Xvfb and pulseaudio, in podman
make e2e      # the whole thing: podman host, Playwright and Chrome client
make check    # all three
```

`make e2e` runs four scenarios against a fresh host container each: direct
UDP, UDP blocked on the host from the start, UDP blocked mid-session, and
`--attach auto` against a desktop the test user already has running. Each
asserts that decoded frames are not black and change after typing, that typed
text runs in the remote shell, pointer position, clipboard both ways, resize,
that sound played on the host is heard once the speaker is on, which
transport ICE picked, and that nothing survives the CLI; for the attached
desktop that means a resize leaves its screen alone and it outlives the session
with the same processes, autorepeat and keymaps. See
[e2e/README.md](e2e/README.md). Rootless containers cannot shape bandwidth,
so congestion control is tested separately over a shaped virtual link
(`go test ./internal/agent/peer -run Congestion`).

The hardware encoders are tested against stand-in tools everywhere, and
against the real GPU wherever `make test` runs on a host that has one along
with a tool to drive it (a GPU with neither ffmpeg nor a GStreamer encoder
plugin skips): every route the host offers must produce a Baseline IDR
first, P slices after, an IDR on request and the target bitrate on noise,
and ffmpeg, where it is installed, must decode the result. Set
`RJ_HWENC_REQUIRE=vaapi` (or `nvenc`) to make a missing route fail the run
instead of skipping it, and `RJ_HWENC_COMPARE=1` with the openh264 build tag
to print the comparison the ADR quotes.

## Known limits

- Attaching works with X11 desktops. On a Wayland session `--attach auto`
  will not guess, because the X server you own there is Xwayland, whose
  screen holds none of the desktop.
- An attached desktop arrives as one picture of all its monitors, gaps
  included, scaled to fit `--max-size`, so on a wide multi-monitor desktop
  small text gets hard to read.
- While attached, autorepeat is off for the whole display, keys held at the
  machine itself included, until the session ends.
- Text pasted from the browser into an attached desktop outlives the session
  only if that desktop runs a clipboard manager.
- Sound in a spawned session needs `pulseaudio` on the host. Current Fedora
  and Ubuntu desktops ship PipeWire without it, and their spawned sessions
  are silent.
- The page starts muted, because browsers autoplay only silent video: one
  click on the speaker, again after each reload.
- Sound goes one way; there is no microphone.
- In-band FEC protects speech only. Opus sends speech in hybrid mode, whose
  SILK layer repeats the previous frame at low rate; music stays in CELT,
  which has no such redundancy, so a lost music packet is concealed by the
  browser instead.
- Attached, the sound is the monitor of whatever output was the default when
  the session started, and the host's own speakers keep playing it.
- Clipboard is plain text up to 255 KiB in each direction.
- Keys map by character, so text arrives right whatever the client layout,
  but IME composition (CJK input) is not forwarded.
- The status tab reads 0 fps on an idle screen. Nothing is sent when nothing
  changes; that is not a stall.
- Through the SSH tunnel video is TCP inside TCP, so a lossy link shows
  head-of-line stalls that direct UDP would not.
- The macOS client builds but has not been run; neither has building the
  agents from a Mac.
- One browser per session; a second tab is refused until the first closes.
- Hardware encoding answers a keyframe request about 35 ms later than
  software and adds a few milliseconds to every frame, and the NVENC route
  is untested on real hardware; see [Hardware encoding](#hardware-encoding).
