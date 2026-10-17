# Robin e2e

End-to-end tests for `robin gui`. A real browser (Google Chrome in a
Playwright container) runs the real CLI against an sshd container that has
nothing on it but what a user's Linux box would have: OpenSSH, Xvfb, openbox,
xterm and pulseaudio. Nothing is mocked between the two, so a pass means SSH
bootstrap, the agent, H.264 encode and decode, Opus, WebRTC, input, clipboard,
resize and teardown all worked over a real network. The `wayland` scenario
swaps the host for a headless GNOME Wayland session, reached the same way.

## Running

```sh
# build bin/robin-linux-amd64 first (the repo Makefile does)
e2e/run.sh                # direct, blocked, restart, attach, wayland
e2e/run.sh direct         # one scenario
e2e/run.sh selftest       # the harness itself, no CLI needed
ROBIN_BIN=/path/to/robin e2e/run.sh restart
```

Prerequisites are rootless `podman` (3.4 is what this was built on), network
access the first time for image pulls (Ubuntu, the Playwright image, Chrome from
Google's apt repo, npm), and around 3.6 GB of disk for the two images. Docker
is not supported. Every run builds both images first, which is a few seconds
once the layers are cached. The `wayland` scenario adds the GNOME host image
(about 1 GB) and needs Go on the machine to build `robin-consent`.

Exit status is non-zero if any scenario fails. Containers and the `robin-e2e`
network are removed on exit, Ctrl-C included. Two runs can share a machine
when each sets its own `ROBIN_E2E_ID` (which names the network, containers and
images), `ROBIN_E2E_SUBNET` and `ROBIN_E2E_HOST_IP`:

```sh
ROBIN_E2E_ID=robin-e2e-2 ROBIN_E2E_SUBNET=10.89.43.0/24 ROBIN_E2E_HOST_IP=10.89.43.10 make e2e
```

## Scenarios

Each scenario gets a fresh host container, so blocked UDP or a leftover agent
cannot leak into the next one. The tests run in order against one session.

| Scenario | Host network | Proves |
|---|---|---|
| `direct` | open | the session comes up on UDP |
| `blocked` | UDP dropped (except loopback) before the CLI starts | ICE-TCP through the SSH forward carries the whole session; the selected remote candidate is `127.0.0.1` |
| `restart` | open, then UDP dropped mid-session | the page notices, restarts ICE and recovers onto TCP within 15 s (`ROBIN_RESTART_TIMEOUT_MS`; typically about 3 s), and input still works afterwards |
| `attach` | open | `--attach auto` finds and streams the desktop the `robin` user already has running (`robin-live`: Xvfb on `:5` at 1280x800 with its cookie in `~/.Xauthority`, an xterm and openbox, started over ssh before the CLI) and leaves it as it was |
| `wayland` | open, Wayland host image | `--attach auto` finds the `robin` user's GNOME Wayland session (`robin-wayland start`: mutter with a 1280x720 virtual monitor, PipeWire with pipewire-pulse, the GNOME portal and a fullscreen probe window, started over ssh before the CLI), the portal's consent dialog is answered once, and a second run restores without it; see below |

The four X11 scenarios also check, in this order:

1. **Frames**: a non-black frame within 20 s of the URL appearing, and
   `framesDecoded` grows after the host retitles the xterm (damage that does not
   come from Robin's own input).
2. **Keyboard**: clicking the xterm through the video and typing
   `echo robin-e2e > /tmp/robin-e2e-<scenario>` creates that file on the host, and
   the xterm region of the picture changes.
3. **Latency**: after 20 s of a still screen, four keystrokes 250 ms apart
   each reach the screen within 400 ms (`requestVideoFrameCallback`). Without
   the agent's zero playout delay, Chrome held the first of them for most of
   a second.
4. **Pointer**: moving to two points on the page puts the X pointer within 2 px
   of the matching display pixel (`xdotool getmouselocation`).
5. **Cursor**: the page's CSS cursor is the remote pointer image, and it
   changes when the pointer moves over the xterm.
6. **Clipboard**: `robin.sendClipboard()` reaches `xclip -o` on the host,
   and a host `xclip -i` shows up in `robin.lastRemoteClipboard`.
7. **Resize**: a 1000x700 viewport turns into a 1000x700 video and a 1000x700
   display (`xdotool getdisplaygeometry`) within 10 s. In `attach` the desktop
   keeps its size instead: video and display stay 1280x800 and only the video
   element shrinks.
8. **Sound**: the CLI reported no missing sound, the page shows the speaker
   toggle muted and receives audio packets, and once the toggle is clicked,
   noise played on the host with `paplay` through the session's sound server
   raises Chrome's `totalAudioEnergy`. Nothing wrote to `~/.config/pulse`.
9. **Transport**: per the table above, from `robin.stats()`.
10. **Teardown**: SIGINT to the CLI's process group (what Ctrl-C does) makes it
   exit 0 within 10 s, after which no `Xvfb`, `robin-agent`, `openbox`,
   `xterm` or `pulseaudio` runs as `robin` and the agent's run dir is gone. Stdout must have held
   nothing but the URL. Set `ROBIN_SIGINT_PID_ONLY=1` to signal only the CLI.
   In `attach` the agent and its run dir go while the `robin` user's desktop stays:
   `robin-live state` (the same Xvfb, openbox and xterm, display size, autorepeat
   and a hash of each keymap) must match what `robin-live start` saved before the
   CLI ran. A test just before types a character the US keymap lacks (ж) and
   sees autorepeat off, so the session did change both before putting them back.

The `wayland` scenario runs `tests/wayland.spec.ts` instead, since its host
has no X server for xdotool to ask. The probe window logs the input it gets
in monitor pixels, and each key press or `robin-wayland redraw` moves its
picture on a frame. In order:

1. **Consent**: the CLI picks `wayland` for `--attach auto`, prints who has
   to approve what on which screen and that it waits up to 2m0s, and reaches
   Ready only after `robin-wayland consent` has answered the dialog as the
   person at the screen would (`robin-consent` clicks through mutter's own
   remote desktop interface, a path only tests use).
2. **Frames**: video at the monitor's 1280x720, and `framesDecoded` grows
   after a redraw that does not come from Robin's input.
3. **Cursor**: the page hides its own pointer over the video, and the
   picture changes where the remote pointer moves, since it is drawn in.
4. **Pointer**: motion lands in the probe window within 2 px of the mapped
   pixel, and a click there arrives as one press and one release of button 1.
5. **Keyboard**: `Hi!` typed in the page arrives as those three characters.
6. **Latency**: the same 20 s still-screen check as the X11 scenarios.
7. **Clipboard**: `robin.sendClipboard()` text is what the probe window
   reads from the session's clipboard, and text it puts there shows up in
   `robin.lastRemoteClipboard`, both through the portal.
8. **Sound**: noise played into the session's PipeWire raises Chrome's
   `totalAudioEnergy` once the speaker is on.
9. **Teardown**: SIGINT ends the CLI with exit 0 within 10 s; then no agent,
   `gst-launch-1.0`, portal session or mutter remote desktop or screencast
   session is left, mutter still runs, and the restore token beside the
   agent is mode 600.
10. **Restore**: a second CLI run reaches Ready with no consent notice, its
    frames decode and change, and it leaves nothing behind either.

If a test fails, Playwright restarts the worker and the remaining tests get a
new session, so one broken feature does not hide the others.

The `selftest` scenario checks the harness with no CLI at all: key-only sshd,
SFTP, ControlMaster and both `-O forward` shapes (local unix socket to remote
TCP, local TCP to remote unix socket), and that UDP really is dropped both ways
once blocked while SSH and host loopback keep working (each with a positive
control first). It also covers the `sudo iptables` path as `robin` over ssh. In
Chrome it lists H.264 and decodes an H.264 WebRTC loopback through the same
frame sampler the scenarios use, and it runs the host helpers against a
hand-started Xvfb laid out like the agent's run dir.

## Artifacts

`e2e/out/<scenario>/`:

- `cli.stderr.log`, `cli.stdout.log`: every CLI session in the run, with headers
- `console.log`: the page console, including `[robin-state]` transitions
- `report/index.html`: Playwright report; failing tests carry a screenshot, a
  trace, the last CLI stderr and a `robin.stats()` snapshot
- `video/`: a recording of every page
- `host-sshd.log`, `host-state.txt`: sshd log, process list, iptables rules and
  Robin's dirs on the host at the end of the scenario

Open a trace with `npx playwright show-trace <zip>` from `e2e/` (after
`npm ci`), or through the report.

## Layout

- `host.Containerfile`, `host/`: the host image. User `robin` (uid 1000) logs in
  with a key generated per run; `robin` may `sudo` only `iptables` and `ip6tables`.
  `robin-rundir` finds the agent's run dir (`~/.cache/robin/run/*`,
  `$XDG_RUNTIME_DIR/robin`, `/tmp/robin-1000`), `robin-x` runs a command
  on the agent's display and sound server (the `robin` user's own desktop when the agent
  attached to it), `robin-live` starts that desktop and reports its state, and
  `robin-block-udp` drops
  non-loopback UDP.
- `wlhost.Containerfile`, `wlhost/`: the Wayland host image, layered on the
  image `internal/agent/wayland/testdata` builds for `make test-wayland`
  (mutter, PipeWire, the GNOME portal, `session.sh`, `probe.py`), with sshd
  and the same `robin` login. `run.sh` stages its build context and builds
  `robin-consent` (`internal/agent/wayland/wltest/cmd/robin-consent`) into it; no
  product binary links that package. `robin-wayland` starts the session and
  the probe window, answers the dialog, reads what the probe logged, redraws
  it, and lists agent, `gst-launch-1.0`, portal and mutter sessions still
  alive.
- `client.Containerfile`: Playwright plus Chrome stable and OpenSSH. The
  `PW_VERSION` build arg must match `@playwright/test` in `package.json`; the
  build fails if they drift.
- `run.sh`: images, network, keys, containers, scenarios. The client gets
  `Host robinhost` in `/root/.ssh/config` (with `HostKeyAlias robinhost` and a
  pre-filled known_hosts), the CLI at `/usr/local/bin/robin`, and runs as
  root so the root-owned key mounts stay readable.
- `tests/gui.spec.ts`: the X11 scenarios. `tests/wayland.spec.ts`: the
  Wayland one. `tests/selftest.spec.ts`: the harness.
- `tests/helpers/`: the CLI runner, the frame sampler, page-to-display
  coordinate mapping, and host checks. Host checks use their own ssh
  connections (`ControlPath=none`), never the CLI's master.

## Rootless podman notes

sshd runs with `UsePAM no`: there is no audit or login session support for PAM
in a rootless container. That needs the account unlocked, so `robin` has a `*`
password, and it means `XDG_RUNTIME_DIR` is never set for ssh sessions. The
key mounts are owned by root inside the container, and sshd reads
`authorized_keys` as the user, which is why the public key is mounted 0644
under `/etc/ssh/authorized_keys.d/` instead of into `~robin/.ssh`. iptables works
with `--cap-add NET_ADMIN`. CNI networks here have no DNS between containers,
so the host gets a static IP (`ROBIN_E2E_SUBNET`, `ROBIN_E2E_HOST_IP` override it). `ROBIN_E2E_ID` (default `robin-e2e`) names the network, containers and images, so two checkouts can run at once with distinct subnets.
