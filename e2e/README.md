# Robin e2e

End-to-end tests for `robin gui`. A real browser (Google Chrome in a
Playwright container) runs the real CLI against an sshd container that has
nothing on it but what a user's Linux box would have: OpenSSH, Xvfb, openbox,
xterm and pulseaudio. Nothing is mocked between the two, so a pass means SSH
bootstrap, the agent, H.264 encode and decode, Opus, WebRTC, input, clipboard,
resize and teardown all worked over a real network. The `wayland` scenario
swaps the host for a headless GNOME Wayland session, reached the same way,
the `firefox` and `webkit` scenarios swap Chrome for Playwright's own
Firefox and WebKit, `nomux` runs the CLI without a ControlMaster as on
Windows, `touch` makes Chrome a touch screen, and `systemd` boots systemd
in the host so a kept session has to live as a unit of the user's manager.

## Running

```sh
# build bin/robin-linux-amd64 first (the repo Makefile does)
e2e/run.sh                # direct, blocked, restart, resume, rejoin, attach, wayland, webkit, firefox, nomux, touch, systemd
e2e/run.sh direct         # one scenario
e2e/run.sh selftest       # the harness itself, no CLI needed
ROBIN_BIN=/path/to/robin e2e/run.sh restart
```

Prerequisites are rootless `podman` (3.4 is what this was built on), network
access the first time for image pulls (Ubuntu, the Playwright image, Chrome from
Google's apt repo, npm), and around 3.6 GB of disk for the two images. Docker
is not supported. Every run builds both images first, which is a few seconds
once the layers are cached. The `wayland` scenario adds the GNOME host image
(about 1 GB) and needs Go on the machine to build `robin-consent`. The
client image also fetches Cisco's OpenH264 plugin for Firefox (600 KB) from
the URL Firefox's own plugin manifest names, and checks it against the
manifest's sha512.

Exit status is non-zero if any scenario fails. Containers and the `robin-e2e`
network are removed on exit, Ctrl-C included. Two runs can share a machine
when each sets its own `ROBIN_E2E_ID` (which names the network, containers and
images), `ROBIN_E2E_SUBNET` and `ROBIN_E2E_HOST_IP`:

```sh
ROBIN_E2E_ID=robin-e2e-2 ROBIN_E2E_SUBNET=10.89.43.0/24 ROBIN_E2E_HOST_IP=10.89.43.10 make e2e
```

`ROBIN_E2E_COVER=<dir>` collects coverage, as `make cover-e2e` does with a
CLI and agent built with `-cover`. The host's sshd sets
`GOCOVERDIR=/cover` in every session (`SetEnv` in `host/sshd_config`), and
`run.sh` mounts `<dir>/<scenario>/agent` there; the CLI gets
`GOCOVERDIR=<dir>/<scenario>/cli`, and the Chrome project saves the page's
V8 coverage in `<dir>/<scenario>/page`. A `-cover` binary writes its data
only when it exits normally, so a scenario that fails before its SIGINT
teardown hands in little or nothing.

## Scenarios

Each scenario gets a fresh host container, so blocked UDP or a leftover agent
cannot leak into the next one. The tests run in order against one session.

| Scenario | Host network | Proves |
|---|---|---|
| `direct` | open | the session comes up on UDP |
| `blocked` | UDP dropped (except loopback) before the CLI starts | ICE-TCP through the SSH forward carries the whole session; the selected remote candidate is `127.0.0.1` |
| `restart` | open, then UDP dropped mid-session | the page notices, restarts ICE and recovers onto TCP within 15 s (`ROBIN_RESTART_TIMEOUT_MS`; typically about 3 s), and input still works afterwards |
| `resume` | open, SSH cut mid-session | the robin user's sshd processes are killed, dropping every connection at once; the page says Reconnecting, the CLI redials and resumes the same daemon, Xvfb and xterm, the marker typed before the cut is still on screen, and typing works again |
| `rejoin` | open | the CLI is SIGKILLed, ssh master and all; the desktop stays, `robin ls` lists it as kept, a `robin gui --mic` exits non-zero saying the session started without a microphone and leaves it kept, a new `robin gui` resumes it with the marker on screen, the page's Disconnect, keep desktop button makes the CLI exit 0 and leaves the desktop, and `robin end` tears everything down |
| `attach` | open | `--attach auto` finds and streams the desktop the `robin` user already has running (`robin-live`: Xvfb on `:5` at 2304x800 split into two monitors with `xrandr --setmonitor`, its cookie in `~/.Xauthority`, an xterm and openbox, started over ssh before the CLI) and leaves it as it was; `--max-size 1920x1080` keeps the whole desktop larger than the session, so showing every monitor scales |
| `wayland` | open, Wayland host image | `--attach auto` finds the `robin` user's GNOME Wayland session (`robin-wayland start`: mutter with a 1280x720 virtual monitor, PipeWire with pipewire-pulse, the GNOME portal and a fullscreen probe window, started over ssh before the CLI), the portal's consent dialog is answered once, and a second run restores without it; see below |
| `firefox` | open | the direct scenario in Playwright's Firefox with the OpenH264 plugin: every check passes |
| `webkit` | open | the direct scenario in Playwright's WebKit (Safari's WebRTC stack): H.264 negotiates and decodes, keyboard, clipboard, resize, sound, UDP and teardown work; see below for what it cannot show |
| `nomux` | UDP dropped (except loopback) before the CLI starts | the `blocked` checks with `--ssh-mux=false`, the way the CLI runs on Windows: no ControlMaster runs on the client, the agent and the tunnel's `ssh -N -L <private dir>/fwd1.sock:...` are ssh processes of their own, and ICE-TCP through that forward carries the session, and Ctrl+C reaches the CLI alone: its stderr announces no reconnect and no `ending the kept session`, the end command a fresh login would carry |
| `touch` | open | Chrome as a touch screen (`isMobile`, `hasTouch`), fingers sent through the DevTools protocol; see below |
| `systemd` | open, systemd host image | systemd is PID 1, logind kills what a login started when it ends (`KillUserProcesses=yes`) and `robin` lingers, so the probe has the CLI start the kept session's daemon with `systemd-run --user`; see below |

The X11 scenarios of `tests/gui.spec.ts` also check, in this order:

1. **Frames**: a non-black frame within 20 s of the URL appearing, and
   `framesDecoded` grows after the host retitles the xterm (damage that does not
   come from Robin's own input).
2. **Keyboard**: clicking the xterm through the video and typing
   `echo robin-e2e > /tmp/robin-e2e-<scenario>` creates that file on the host, and
   the xterm region of the picture changes.
3. **Latency**: after 20 s of a still screen, four keystrokes 250 ms apart
   each reach the screen within 400 ms: from the keydown to the first frame
   whose xterm shows the echo (`requestVideoFrameCallback`). Without
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
   keeps its size instead: the video stays at the primary monitor's 1280x800,
   the display at 2304x800, and only the video element shrinks.
8. **Sound**: the CLI reported no missing sound, the page shows the speaker
   toggle muted and receives audio packets, and once the toggle is clicked,
   noise played on the host with `paplay` through the session's sound server
   raises Chrome's `totalAudioEnergy`. Nothing wrote to `~/.config/pulse`.
   In `direct` the CLI runs with `--mic --camera`, and Chrome's fake devices
   play the files `tests/helpers/fakemedia.ts` writes: a 440 Hz tone that
   swells twice a second, and a picture of one red. **Microphone**: the
   switch starts off and the session's default source records silence;
   switched on, it records the tone at 440 Hz within 5%; switched off,
   silence again, and the page's local description never changed.
   **Camera**: the CLI says the spawned desktop has no v4l2loopback device,
   and the page shows no camera switch.
9. **Files**, in `direct` and `attach`: a file set on the status tab's
   input lands in `~/Downloads` with its SHA-256 intact, the same name again
   lands as `robin-e2e-upload (1).bin` and the tab says so, and a file
   dropped on the page arrives too, with nothing else left in the folder.
   Four keys typed while a 256 MiB upload moves must echo within the same
   400 ms, and the upload must have moved on by the last of them.
   Then the host offers a file named `<b>robin<b> & co.bin`: `robin-send`
   typed into the spawned session's xterm, or in `attach`
   `~/.cache/robin/robin-agent-* send` over ssh, which finds the one
   session by itself. The tab shows the name as text (no elements inside
   it) and the size, stays out while the offer waits, and nothing downloads
   until Save is clicked; the download then matches the host's SHA-256, and
   the sender prints that it was saved and exits 0. A second offer is
   dismissed, and the sender says declined and exits 1.
10. **Rich clipboard**, in `direct` and `attach`: an `image/png` and HTML
   pasted into the page come out of `xclip -t image/png` and `-t text/html`
   on the host, and an image `xclip` copies there arrives as
   `robin.lastRemoteClip.png`. Chrome gets the paste as a `ClipboardEvent`;
   Firefox builds such an event's `clipboardData` afresh, keeping nothing a
   script put in it, so there the `DataTransfer` goes to `robin.paste`.
11. **Sharp text**, in `direct`, `attach` and `firefox`, wherever the
   browser reports the RTP timestamp of the frame it shows (the test says
   why it skips where it does not): coloured text in the xterm, once still,
   ends up under tiles everywhere, and the page's frame composed with its
   tile canvas matches `xwd -root` on the host in every pixel, where the
   video alone does not. Typing then changes some tiles; a trace of every
   frame shown and every manifest finds no frame shown with a tile that a
   manifest at or before it names as changed, and each such tile is sharp
   again within 5 s. The same holds with every control message held
   300 ms behind the video (`helpers/sharp.ts` wraps the page's control
   channel before it loads), when the tiles must hide instead. The switch
   in the status tab turns them off: once the agent has answered, no
   manifest arrives over the next frames. Then on again.
12. **Transport**: per the table above, from `robin.stats()`.
13. **Input method** (Chrome only; in `restart` it runs before the transport
   check): with the xterm clicked, `echo `, then what
   Chrome sends for an input method (a `Process` key, a composition that the
   page shows under its status tab, and the committed `日本語ü`), then
   ` > /tmp/robin-e2e-ime-<scenario>` and Enter, writes `日本語ü` to that
   file on the host: characters the keymap lacks typed through spare keys.
14. **Teardown**: SIGINT to the CLI's process group (what Ctrl-C does) makes it
   exit 0 within 10 s, after which no `Xvfb`, `robin-agent`, `openbox`,
   `xterm` or `pulseaudio` runs as `robin`, the agent's run dir is gone, and
   no ssh the CLI started runs on the client. Stdout must have held
   nothing but the URL. Set `ROBIN_SIGINT_PID_ONLY=1` to signal only the CLI.
   In `attach` the agent and its run dir go while the `robin` user's desktop stays:
   `robin-live state` (the same Xvfb, openbox and xterm, display size,
   monitors, autorepeat, a hash of each keymap, the screen's gamma and the
   sound server's sinks) must match what
   `robin-live start` saved before the CLI ran. A test just before types a
   character the US keymap lacks (ж) and sees autorepeat off, so the session
   did change both before putting them back.

In `attach` more checks run before teardown. **Monitors**: the page is
told about LEFT (1280x800, primary) and RIGHT (1024x768) and starts on LEFT;
choosing RIGHT and then all from the status tab's menu turns the video into
1024x768 and then the whole 2304x800 screen scaled to fit, and the pointer
lands within 2 px of the matching pixel of the monitor shown. **Actual
size**: in the 1000x700 window, the status tab's switch lays LEFT out at
1280x800 desktop pixels, each a whole number of device pixels, the pointer
at the right edge pans it as far as it goes, and the pointer then lands
within 2 px of a pixel the fit view had off the page; once every tile is
drawn, the tile canvas lies exactly on the picture. **Sharp text,
scaled**: with all monitors shown, the video scaled down to 1918x666, the
tiles cover the 2304x800 screen at its own size and match the host's in
every pixel.
**Private**: the CLI says the room sees and hears the session and names
`--curtain` and `--silent`, and the page is told both switches are off.
The curtain switch in the status tab blacks out the room's screen
(`robin-room brightness`, the brightness xrandr reads off the screen's
gamma, goes to 0) while the page's picture of a retitled xterm stays lit,
and switched off brings the brightness back above 0.5. The silent room
switch keeps the room's speakers quiet (`robin-room loudness`, the RMS of a
second of the sink `desk`'s monitor, stays below 50) while noise plays on
the host and Chrome's `totalAudioEnergy` grows, and switched off lets it
past 1000 again. No error shows under either switch, and both go on again
before the teardown, which has to give the room its screen and speakers
back.

After the attach teardown, **Kept**: a second CLI attaches with
`--keep 10m` (`session.json` says mode attach, keep 600). SSH is cut and
new logins refused (an `iptables` REJECT on port 22) until the click is in,
lifted over the one connection of its own the cut spares, or by a timer on
the host after a minute; while the page says Reconnecting and has no peer
connection, a click on the curtain switch leaves it busy, and once the CLI
has redialed and the page is connected, the switch is on and the room's
brightness reads 0. The CLI is then SIGKILLed: `robin gui --id <id> --mic`
exits non-zero saying the session started without a microphone, leaving it
kept with the same daemon, and `robin gui --id <id> --curtain --silent`
resumes it with the room dark before the page loads and without the hint
that the room sees the session; the page is told both switches are on, and
noise played on the host stays below 50 RMS at the room's speakers. SIGINT
then ends it with exit 0, no agent or run dir left and `robin-live state`
as before.

The `touch` scenario runs `tests/touch.spec.ts` against the X11 host, with
a 1024x640 touch screen. In order:

1. **Status tab**: the keyboard button shows, the text box does not take the
   focus (which would raise a soft keyboard), and the tab does not tuck away.
2. **Tap**: the X pointer lands within 2 px of the pixel under the finger.
3. **Pinch**: two fingers spreading from 200 to 400 px zoom the picture 2x in
   the page while the video keeps its size, a tap then lands within 2 px of
   the pixel the zoomed picture shows there, and pinching in goes back to 1x.
4. **Scroll**: after `seq 200` in the xterm, two fingers dragged down scroll
   it back, which changes the xterm region of the picture.
5. **Soft keyboard**: the keyboard button focuses the text box; text typed
   as a soft keyboard does (each letter as input, a word composed) makes
   `echo touch-ü > /tmp/robin-e2e-touch` run on the host; pressed again,
   the button lets go.
6. **Teardown**: as in the X11 scenarios.

The `systemd` scenario runs `tests/systemd.spec.ts` against the systemd host
image. In order:

1. **logind**: a `setsid sleep` started over ssh is gone once that login
   ends, so a daemon that only left the process session would die too.
2. **Unit**: the CLI's session comes up with a picture and no warning about
   lingering; `systemctl --user` shows `robin-<id>.service` active with the
   daemon as its main process, the daemon's cgroup is that unit under
   `user@1000.service`, and the proxy and its sshd are in the login's
   `session-*.scope`.
3. **Kept**: the CLI is SIGKILLed; the proxy and its sshd go, while the
   daemon, its unit and Xvfb stay, and `session.json` says kept.
4. **Resume**: `robin ls` lists it as kept, and a new `robin gui` resumes
   the same daemon in the same unit, with a picture.
5. **End**: `robin end robinhost <id>` ends it while that CLI is connected;
   the CLI exits 0, the page says the session ended, the unit is inactive,
   the run directory is gone and nothing of the session runs.

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
9. **Microphone** (the CLI runs with `--mic --camera` here too): the agent's
   PipeWire source, `robin-mic-<pid>`, records silence through
   pipewire-pulse until the switch goes on, then the page's tone.
10. **Camera**: its node, `robin-camera-<pid>`, read with `pipewiresrc`,
   shows black, then the page's red within a few levels once switched on,
   then black again once off. Neither switch changes the page's local
   description.
11. **Teardown**: SIGINT ends the CLI with exit 0 within 10 s; then no agent,
   `gst-launch-1.0`, portal session, mutter remote desktop or screencast
   session, or microphone or camera node is left, mutter still runs, and
   the restore token beside the agent is mode 600.
12. **Restore**: a second CLI run reaches Ready with no consent notice, its
    frames decode and change, and it leaves nothing behind either.

The `firefox` and `webkit` scenarios run `tests/gui.spec.ts` against the
direct scenario's host, as Playwright projects of the same names
(`ROBIN_E2E_BROWSER` tells the spec which), without `--mic` or `--camera`. Firefox runs every check above.
Playwright's WebKit on Linux decodes WebRTC video but renders no remote
track into a `<video>` element: in a blank page, a VP8 loopback between two
peer connections stays at `readyState` 0 with `videoWidth` 0, where Chrome
and Firefox show it, and a local canvas stream does render. So in `webkit`
the checks that read the picture or map the pointer onto it (frames,
keyboard through a click, latency, pointer, cursor, and files and the rich
clipboard, which type into the xterm through the picture) are skipped, and
in their place:

- **Frames**: from `getStats`, the negotiated codec is H.264 with
  `profile-level-id=42e01f` and `packetization-mode=1`, keyframes decoded,
  a decoded frame size, and `framesDecoded` growing after the host retitles
  the xterm.
- **Keyboard**: with the xterm focused from the host (there is no picture
  to click), the typed command creates its file.
- **Resize**: the decoded frame size, not the element's, follows the
  viewport along with the display.

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
- `console.log`: the page console, including `[robin-state]` transitions and
  `[robin-csp]` lines for anything the page's CSP blocked
- `report/index.html`: Playwright report; failing tests carry a screenshot, a
  trace, the last CLI stderr, a `robin.stats()` snapshot and the browser's own
  inbound video stats
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
- `sdhost.Containerfile`, `sdhost/`: the systemd host image, layered on the
  X11 host image: systemd as init, sshd with `UsePAM yes` so each login is
  a logind session, `KillUserProcesses=yes`, and `robin` lingering. `run.sh`
  starts it with `--systemd=always` instead of `--init`.
- `client.Containerfile`: Playwright plus Chrome stable and OpenSSH. The
  `PW_VERSION` build arg must match `@playwright/test` in `package.json`; the
  build fails if they drift.
- `run.sh`: images, network, keys, containers, scenarios. The client gets
  `Host robinhost` in `/root/.ssh/config` (with `HostKeyAlias robinhost` and a
  pre-filled known_hosts), the CLI at `/usr/local/bin/robin`, and runs as
  root so the root-owned key mounts stay readable.
- `tests/gui.spec.ts`: the X11 scenarios. `tests/touch.spec.ts`: the touch
  screen. `tests/wayland.spec.ts`: the Wayland one. `tests/systemd.spec.ts`:
  the systemd one. `tests/selftest.spec.ts`: the harness.
- `tests/helpers/`: the CLI runner, the frame sampler, page-to-display
  coordinate mapping, page coverage, and host checks. Host checks use their own ssh
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

The systemd host runs PAM after all, and logind works, but a rootless
container cannot give a login an audit session of its own, so logind names
every login after the one podman itself runs in (`session-8083.scope`, say),
and a login that starts after another ended gets the same scope back.
`tests/systemd.spec.ts` therefore follows the CLI's login by its processes,
not by its scope's name.
