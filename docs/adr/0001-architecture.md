# ADR 0001: architecture of `remotejump gui`

Status: accepted, 2026-10-16

## Context

`remotejump gui <host>` opens a remote Linux desktop in one command. The only
thing the host has beforehand is SSH access the user already has. SSH is the
control plane (auth, probing, deploying the agent, signaling); WebRTC over UDP
is the data plane (video out, input back). No signaling server, no extra ports
opened for auth, no root on the host.

## Decisions

### Go + Pion for both the CLI and the host agent

Two binaries from one module. The CLI is pure Go (`CGO_ENABLED=0`), so it
cross-compiles to macOS and Linux with nothing but the Go toolchain. The agent
is a static Linux binary the CLI embeds and copies to the host over SFTP. Pion
gives us WebRTC, ICE (including ICE-TCP), DTLS, SCTP data channels and GCC
congestion control in Go, so the agent has no runtime dependencies beyond the
kernel and an X server.

### The system `ssh` client, multiplexed with ControlMaster

The CLI drives the user's own `ssh` binary rather than embedding an SSH
library. One `ssh -M` master holds the connection for the whole session; the
probe, SFTP upload, agent exec channel and port forward all ride it through
`-S <socket>`. This inherits everything the user's setup already does:
`~/.ssh/config` (`Match`, `Include`, `ProxyJump`, `ProxyCommand`), the agent,
FIDO keys, certificates, Kerberos, and interactive prompts for passwords, 2FA
and unknown host keys. A Go SSH library would need to reimplement all of that
and would still miss cases. Cost: the client needs OpenSSH with ControlMaster,
which rules out native Windows for now.

### Software H.264 with openh264, statically linked through cgo

Pion does not encode. Options considered:

| Option | Verdict |
|---|---|
| x264 via cgo | Best quality per CPU, but GPL-2.0 would make the agent binary GPL and constrain the project's licence. |
| openh264 via cgo | BSD-2. Emits Constrained Baseline, which every browser decodes in WebRTC. Runtime bitrate and IDR control, which is exactly what congestion control needs. **Chosen.** |
| GStreamer on the host | Needs packages on the host. Rejected. |
| ffmpeg subprocess | Needs ffmpeg on the host, or shipping a ~80 MB static build. Rejected. |

The C/C++ toolchain is a pinned `zig cc` fetched by the build, targeting
`x86_64-linux-musl` and `aarch64-linux-musl`. The agent is fully static and
cross-compiles from Linux or macOS with no containers or root.

The encoder runs in openh264's camera mode with scene-change detection off,
frame skipping on, and QP allowed from 16 to 51. Screen-content mode looked
like the obvious fit and lost on measurement (`internal/agent/h264`,
`TestCompareModes`): it forces scene-change detection, so full-screen motion
became an IDR on every frame at 18 to 32 Mbit/s whatever the target, and it
encoded three to four times slower. With its default QP clamp of 26 to 35 it
also could not follow a bitrate target at all. Frame skipping is what holds the
target on incompressible content; the skips cluster right after a keyframe,
while the link is still busy carrying it, so they cost nothing visible. A frame
rate control drops is encoded again on the next tick, or an idle screen would
freeze on a stale picture.

Patent note: openh264 built from source is not covered by Cisco's binary
licence. That is the same position as every other source build of an H.264
encoder; Phase 2 hardware encoders do not change it.

### Spawn mode requires Xvfb on the host; we do not bundle it

Xvfb 21.1 supports RandR resize up to the size it was started with, as long as
the client that created the mode stays connected (verified: the agent holds
its X connection for the session, so this holds). The agent starts Xvfb at the
maximum size and switches modes to match the browser window.

Bundling Xvfb was rejected: it is a dynamically linked server that also needs
`xkbcomp`, XKB data and fonts at runtime, which is a fragile tree to ship
across distros. And a host that lacks Xvfb also lacks any desktop to run in
it, so the user is installing packages either way; `xvfb` is one more name on
the same line. The probe reports exactly what is missing and the CLI prints
the install command. Attach mode needs none of it.

The agent picks the lightest session it finds: Xfce, else a window manager
with a terminal beside it, else a bare terminal. The terminal starts first
and the window manager adopts it. The other order looked natural and was
broken: openbox sets `_NET_SUPPORTING_WM_CHECK` a few milliseconds after it
starts but only handles map requests later, and a terminal mapped in that
window stayed unmanaged and invisible for 12 to 19 seconds (measured in the
e2e host image, with and without network, so not a DNS stall).

### X11 in pure Go

Capture (MIT-SHM `GetImage`, DAMAGE to skip idle frames), input (XTest),
resize (RandR) and clipboard (selections, XFIXES owner notifications) all use
`github.com/jezek/xgb`. No libX11 linkage, no cgo outside the encoder.

### The browser offers and controls ICE

The browser creates the offer (a recvonly video transceiver plus the control
data channel) and is the ICE-controlling agent; the agent answers. When the
browser sees the connection go `disconnected` or `failed` it calls
`restartIce()`, which produces a fresh offer, so recovery from a UDP drop is a
renegotiation rather than a new session.

### Transport fallback: ICE-TCP through an SSH forward, always offered

ProxyJump cannot carry UDP, so the fallback is core. The agent accepts ICE-TCP
on a listener only reachable from the host itself; the CLI forwards to it over
the SSH master and hands the browser one extra candidate, a passive TCP
candidate on the client's loopback that leads into the forward. It is offered
on every session alongside the direct UDP host and STUN candidates, and ICE
priority picks UDP when UDP works. There is no detection step to get wrong.
The CLI drops any other TCP candidate the agent emits, so TCP media can only
take the SSH path.

Details that testing settled. The synthesized candidate has priority
16777215, type preference 0 as for relayed candidates; pair priority uses the
lower of the two sides, so every tunnel pair ranks below every direct UDP pair
whatever the browser assigns its own candidates. The agent must gather a
loopback candidate for its TCP mux to accept the forwarded connection, which
also gathers loopback UDP candidates; the CLI drops those, since they would
point the browser at its own machine. The browser learns nothing it should
not: its host candidates stay mDNS names and the agent sees it as
peer-reflexive.

Failover needed two more pieces. When UDP dies, Chrome nominates the
lower-priority tunnel pair within about two seconds, and Pion, as the
controlled agent, ignores any nomination of lower priority than its current
pair, so it kept sending into the dead path until ICE failed half a minute
later. The agent now follows a nomination once its own pair has gone
disconnected (after 2 s of silence) and the nominated pair has answered a
check; gating it that way keeps the ordinary startup churn of nominations from
flipping pairs. And because a path can die in one direction only, which leaves
the agent's pair looking healthy, the agent sends a heartbeat over the control
channel every second and the page restarts ICE after 3.5 s without one.
Measured in the e2e `restart` scenario, media is back over the tunnel about
three seconds after UDP is dropped.

### Congestion control: GCC with no pacer and a bounded NACK responder

The browser sends transport-wide congestion control feedback; Pion's GCC turns
it into a target bitrate that the encoder follows. Two Pion defaults had to go,
both found by streaming through a shaped virtual link (`internal/agent/peer`,
`TestCongestionControlAdaptsToLink`). The default leaky-bucket pacer queues
without bound, so whatever was sent while the target fell drained for seconds
and the delay that added pinned the target at its floor. And the default NACK
responder resends anything in a 1024-packet buffer as often as it is asked:
after a startup overshoot the lost packets were resent, lost again and NACKed
again until retransmissions filled the link, and the loss estimator reported
93% loss with nothing being lost. The agent sends unpaced (the encoder holds
the rate) and answers NACKs only for packets younger than a second, at most
twice each, within a quarter of the target bitrate. A 4x overshooting start
and a link that drops mid-session now both settle within about 10% of the
link rate.

### Trust: the DTLS fingerprint arrives over SSH

The agent's SDP, including its DTLS fingerprint, reaches the browser only over
the SSH channel and then the token-guarded localhost websocket. WebRTC already
refuses a DTLS peer whose certificate does not match the fingerprint in the
remote description, so a party that can inject UDP or TCP packets cannot
impersonate the agent. The CLI pins the first fingerprint it sees and aborts
if a later answer (an ICE restart) carries a different one.

### Keyboard: keysyms with on-the-fly keymap entries

The browser sends `KeyboardEvent.key` and `.code`. Printable characters are
typed as keysyms: the agent finds a keycode and shift level that produce the
keysym, adjusts Shift and AltGr around the press, and binds a spare keycode
when the remote keymap has no such keysym. Named keys (Enter, arrows, F-keys,
modifiers) map from `key`/`code` to keysyms and are held until key-up. This
keeps text right regardless of the client's layout.

### Attach mode: the user's own X11 desktop, left as found

`--attach :N` streams an X display that is already running for the user,
usually the one they are logged in to at the machine. Nothing is spawned. The
agent opens one X connection and needs nothing installed beyond the desktop
itself, and the session ends when that connection does, from either side.

The agent tries the cookie for that display in `$XAUTHORITY`, then
`~/.Xauthority` (startx, LightDM), `/run/user/$UID/gdm/Xauthority`, mutter's
`.mutter-Xwaylandauth.*` and SDDM's `xauth_*`, and after those a connection
with no cookie it holds. xgb cannot send an empty authorization, so that last
attempt presents a zero cookie; the server refuses it and falls back to
host-based access, which is what an `xhost +si:localuser:$USER` grant opens.

For `--attach auto` the CLI takes the display of the user's active local X11
session from logind. Under GDM that field is empty, because GDM registers the
session before its X server starts (the GNOME workstation used for the checks below
reports exactly that), so the fallback is the one socket in `/tmp/.X11-unix`
the user owns. A Wayland session would pass that test too, through Xwayland,
whose root window shows none of the desktop; with an active Wayland session
`auto` refuses to guess and lists what it found.

The desktop's size belongs to its user. `Resize` returns `ErrFixedSize` and
the page scales the video into its window. A desktop larger than the session
maximum (a 4K monitor, or the 5120x2640 three-monitor desktop of that
workstation) is shrunk in the stream, in the same pass as the BGRX to I420
conversion, with each output pixel averaging the exact source area it covers.
Sampling instead drops strokes of text at ratios like 4:3. 3840x2160 to
1920x1080 takes about 6 ms on four threads (`BenchmarkScale4KTo1080p` in
`internal/agent/yuv`). The page reports pointer positions in video pixels, and
the stream maps each one to the desktop pixel under the centre of that video
pixel, using the scale of the frames it is sending.

Leaving the desktop as found took two changes to how input touches the
server. Autorepeat used to go off when the agent connected; it now goes off at
the first key, so a session that only watches changes nothing. And it goes off
differently. A spawned display switches it off per key, which survives Xfce
turning the global switch back on as it starts, but per-key changes set every
key's explicit-repeat flag in the XKB keymap and putting the bits back does not
clear them: after a session, `xkbcomp -xkb` printed `repeat= Yes` for every
key. An attached display gets the global switch instead, turned off again at
each key press in case the desktop has turned it on, and its keymap dump is
byte-identical afterwards (`TestAttachLeavesTheDesktopAsFound`), spare keycodes
bound for missing characters included. Clipboard text the agent still serves
at close goes to the session's clipboard manager over the freedesktop.org
`SAVE_TARGETS` handshake when something owns `CLIPBOARD_MANAGER`; without one
the selection loses its owner, as it does when any X client exits.

Capture skips idle ticks on the word of DAMAGE on the root window, and a
compositing manager redirects every window offscreen, so drawing in a window
reaches the root only when the compositor repaints. With xcompmgr and picom's
xrender backend that repaint raises DAMAGE (`TestDamageUnderCompositor` runs
both in the X test container). GNOME's mutter on X11 does the same: watched
view-only for 130 s on a live, locked session (Intel graphics, 5120x2640),
DAMAGE reported each minute's clock redraw and nothing else. GL compositors on
drivers that render around the X server's drawing code may not, and a screen
whose changes DAMAGE never sees would freeze in the browser. So a screen DAMAGE
calls idle is grabbed anyway once a second and compared by hash with the last
frame; the first difference switches Capture to comparing every frame, which
still keeps an idle screen from being encoded. In the 130 s on mutter, 128 of
those rechecks found nothing DAMAGE had missed. Each cost one MIT-SHM grab of
2 to 4 ms and a hash of the 54 MB frame, about 13 ms.
