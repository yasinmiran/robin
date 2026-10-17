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

That fetches a pinned zig toolchain and builds openh264 the first time (about
half a minute, cached under `.cache/`), builds static agents for linux/amd64
and linux/arm64, and embeds both in `bin/remotejump`. `make dist` cross-builds
the CLI for macOS and Linux on amd64 and arm64 into `dist/`. The CLI itself is
pure Go; only the agents need the C toolchain, and the build downloads it, so
there is nothing to install by hand on Linux or macOS.

## Usage

```sh
remotejump gui alice@build-box
remotejump gui build-box -- -p 2222 -i ~/.ssh/other_key   # anything after -- goes to ssh
remotejump gui --max-size 2560x1440 --fps 60 workstation
remotejump gui --no-browser lab-node                     # print the URL instead of opening it
```

Your `~/.ssh/config`, agent, `ProxyJump`, `ProxyCommand`, certificates and
password or 2FA prompts all work as they do for plain `ssh`, because it is
plain `ssh`: one ControlMaster connection carries the probe, the agent upload,
the agent itself and the tunnel. The browser tab can be reloaded; the remote
session survives until you press Ctrl+C in the terminal.

| Flag | Default | |
|---|---|---|
| `--transport` | `auto` | `direct` for UDP only, `tunnel` to force the SSH path |
| `--max-size` | `1920x1080` | largest remote resolution; the display follows your window up to this. 1280x720 on hosts with two cores or fewer |
| `--fps` | `30` | frame rate cap (20 on small hosts) |
| `--max-kbps` | `10000` | video bitrate ceiling; congestion control works below it |
| `--session` | lightest found | command to run in the display, e.g. `startxfce4` |
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

No GPU or monitor is needed; the session renders in software, and a light
window manager keeps that cheap. Software H.264 at 1080p30 costs one to two
cores, which is why small hosts get a lower default size and frame rate.

The agent goes to `~/.cache/remotejump` (or `$XDG_RUNTIME_DIR/remotejump`, or
`/tmp/remotejump-$UID` when the home directory is mounted `noexec`), checked
by hash and reused across sessions. It runs as you, starts its own display on
a free number, and when the SSH channel closes it kills everything it started
and removes its files.

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
5. It starts Xvfb, a session, captures with MIT-SHM and DAMAGE, encodes with
   openh264, and sends over a Pion peer connection. Keyboard, pointer,
   clipboard and resize come back over a data channel and go in through XTest
   and RandR.
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
a fresh cookie in a 0600 file; the agent and its run directory are 0700.
Input from the page is limited to what a keyboard, a pointer and a clipboard
can do in that session.

One exposure is on the client side: to open your browser the CLI hands the
page URL, token included, to `open` or `xdg-open`, so for that moment another
user logged in to the same client machine could read it from the process
list. On a shared client, use `--no-browser` and open the printed URL yourself.

## Testing

```sh
make test     # unit and integration tests; the encoder against real openh264 on linux/amd64
make test-x   # capture, input, display and agent session tests against Xvfb, in podman
make e2e      # the whole thing: podman host, Playwright and Chrome client
make check    # all three
```

`make e2e` runs three scenarios against a fresh host container each: direct
UDP, UDP blocked on the host from the start, and UDP blocked mid-session. Each
asserts that decoded frames are not black and change after typing, that typed
text runs in the remote shell, pointer position, clipboard both ways, resize,
which transport ICE picked, and that nothing survives the CLI. See
[e2e/README.md](e2e/README.md). Rootless containers cannot shape bandwidth,
so congestion control is tested separately over a shaped virtual link
(`go test ./internal/agent/peer -run Congestion`).

## Known limits

- Spawn mode only for now: it starts a new headless session rather than
  attaching to a desktop already running on the host.
- No audio, and the remote cursor shape is not shown; the browser keeps its
  own arrow.
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
