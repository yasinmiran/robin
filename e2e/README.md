# remotejump e2e

End-to-end tests for `remotejump gui`. A real browser (Google Chrome in a
Playwright container) runs the real CLI against an sshd container that has
nothing on it but what a user's Linux box would have: OpenSSH, Xvfb, openbox and
xterm. Nothing is mocked between the two, so a pass means SSH bootstrap, the
agent, H.264 encode and decode, WebRTC, input, clipboard, resize and teardown all
worked over a real network.

## Running

```sh
# build bin/remotejump-linux-amd64 first (the repo Makefile does)
e2e/run.sh                # direct, blocked, restart
e2e/run.sh direct         # one scenario
e2e/run.sh selftest       # the harness itself, no CLI needed
RJ_BIN=/path/to/remotejump e2e/run.sh restart
```

Prerequisites are rootless `podman` (3.4 is what this was built on), network
access the first time for image pulls (Ubuntu, the Playwright image, Chrome from
Google's apt repo, npm), and around 3.6 GB of disk for the two images. Docker
is not supported. Every run builds both images first, which is a few seconds
once the layers are cached.

Exit status is non-zero if any scenario fails. Containers and the `rj-e2e`
network are removed on exit, Ctrl-C included, and two runs cannot share a
machine because the names are fixed.

## Scenarios

Each scenario gets a fresh host container, so blocked UDP or a leftover agent
cannot leak into the next one. The tests run in order against one session.

| Scenario | Host network | Proves |
|---|---|---|
| `direct` | open | the session comes up on UDP |
| `blocked` | UDP dropped (except loopback) before the CLI starts | ICE-TCP through the SSH forward carries the whole session; the selected remote candidate is `127.0.0.1` |
| `restart` | open, then UDP dropped mid-session | the page notices, restarts ICE and recovers onto TCP within 15 s (`RJ_RESTART_TIMEOUT_MS`; typically about 3 s), and input still works afterwards |

Every scenario also checks, in this order:

1. **Frames**: a non-black frame within 20 s of the URL appearing, and
   `framesDecoded` grows after the host retitles the xterm (damage that does not
   come from remotejump's own input).
2. **Keyboard**: clicking the xterm through the video and typing
   `echo rj-e2e > /tmp/rj-e2e-<scenario>` creates that file on the host, and
   the xterm region of the picture changes.
3. **Pointer**: moving to two points on the page puts the X pointer within 2 px
   of the matching display pixel (`xdotool getmouselocation`).
4. **Clipboard**: `remotejump.sendClipboard()` reaches `xclip -o` on the host,
   and a host `xclip -i` shows up in `remotejump.lastRemoteClipboard`.
5. **Resize**: a 1000x700 viewport turns into a 1000x700 video and a 1000x700
   display (`xdotool getdisplaygeometry`) within 10 s.
6. **Transport**: per the table above, from `remotejump.stats()`.
7. **Teardown**: SIGINT to the CLI's process group (what Ctrl-C does) makes it
   exit 0 within 10 s, after which no `Xvfb`, `remotejump-agent`, `openbox` or
   `xterm` runs as `rj` and the agent's run dir is gone. Stdout must have held
   nothing but the URL. Set `RJ_SIGINT_PID_ONLY=1` to signal only the CLI.

If a test fails, Playwright restarts the worker and the remaining tests get a
new session, so one broken feature does not hide the others.

The `selftest` scenario checks the harness with no CLI at all: key-only sshd,
SFTP, ControlMaster and both `-O forward` shapes (local unix socket to remote
TCP, local TCP to remote unix socket), and that UDP really is dropped both ways
once blocked while SSH and host loopback keep working (each with a positive
control first). It also covers the `sudo iptables` path as `rj` over ssh. In
Chrome it lists H.264 and decodes an H.264 WebRTC loopback through the same
frame sampler the scenarios use, and it runs the host helpers against a
hand-started Xvfb laid out like the agent's run dir.

## Artifacts

`e2e/out/<scenario>/`:

- `cli.stderr.log`, `cli.stdout.log`: every CLI session in the run, with headers
- `console.log`: the page console, including `[rj-state]` transitions
- `report/index.html`: Playwright report; failing tests carry a screenshot, a
  trace, the last CLI stderr and a `remotejump.stats()` snapshot
- `video/`: a recording of every page
- `host-sshd.log`, `host-state.txt`: sshd log, process list, iptables rules and
  remotejump dirs on the host at the end of the scenario

Open a trace with `npx playwright show-trace <zip>` from `e2e/` (after
`npm ci`), or through the report.

## Layout

- `host.Containerfile`, `host/`: the host image. User `rj` (uid 1000) logs in
  with a key generated per run; `rj` may `sudo` only `iptables` and `ip6tables`.
  `rj-rundir` finds the agent's run dir (`~/.cache/remotejump/run/*`,
  `$XDG_RUNTIME_DIR/remotejump`, `/tmp/remotejump-1000`), `rj-x` runs a command
  on the agent's display, and `rj-block-udp` drops non-loopback UDP.
- `client.Containerfile`: Playwright plus Chrome stable and OpenSSH. The
  `PW_VERSION` build arg must match `@playwright/test` in `package.json`; the
  build fails if they drift.
- `run.sh`: images, network, keys, containers, scenarios. The client gets
  `Host rjhost` in `/root/.ssh/config` (with `HostKeyAlias rjhost` and a
  pre-filled known_hosts), the CLI at `/usr/local/bin/remotejump`, and runs as
  root so the root-owned key mounts stay readable.
- `tests/gui.spec.ts`: the scenarios. `tests/selftest.spec.ts`: the harness.
- `tests/helpers/`: the CLI runner, the frame sampler, page-to-display
  coordinate mapping, and host checks. Host checks use their own ssh
  connections (`ControlPath=none`), never the CLI's master.

## Rootless podman notes

sshd runs with `UsePAM no`: there is no audit or login session support for PAM
in a rootless container. That needs the account unlocked, so `rj` has a `*`
password, and it means `XDG_RUNTIME_DIR` is never set for ssh sessions. The
key mounts are owned by root inside the container, and sshd reads
`authorized_keys` as the user, which is why the public key is mounted 0644
under `/etc/ssh/authorized_keys.d/` instead of into `~rj/.ssh`. iptables works
with `--cap-add NET_ADMIN`. CNI networks here have no DNS between containers,
so the host gets a static IP (`RJ_E2E_SUBNET`, `RJ_E2E_HOST_IP` override it).
