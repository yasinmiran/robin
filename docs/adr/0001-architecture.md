# ADR 0001: architecture of `robin gui`

Status: accepted, 2026-10-16

## Context

`robin gui <host>` opens a remote Linux desktop in one command. The only
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

### Hardware H.264 through a host tool, picked where it pays

`--encoder vaapi` and `--encoder nvenc` encode on the host's GPU, and the
default, `--encoder auto`, picks one of them where the probe shows it pays
(below). The agent
is a fully static musl binary, which cannot dlopen the host's glibc libva,
its VA driver or the NVIDIA libraries, and a second, dynamically linked
helper would have to match every distribution's glibc and driver ABI. So
the agent runs a tool the host already has as a subprocess (`internal/agent/hwenc`):
ffmpeg with `h264_vaapi` or `h264_nvenc`, or `gst-launch-1.0` with
GStreamer's va, vaapi or nvcodec encoder. Frames go in as raw NV12 on
stdin; `vaapih264enc` on Intel's driver takes nothing else, so the agent
interleaves the chroma itself rather than paying for a `videoconvert`. One
access unit per frame comes back on stdout.

Where an access unit ends is the first problem. A raw Annex-B stream only
shows it when the next one begins, a frame later, and the stream loop waits
for each frame's access unit before it sends anything. So each tool wraps
its output in a container that states lengths: GStreamer in `multipartmux`
parts with a Content-Length, ffmpeg in FLV tags. FLV keeps the SPS and PPS
in its sequence header, and some encoders send them only once, so the
reader puts them back in front of every IDR, which a browser that joins or
recovers decodes from alone. GStreamer could have used FLV as well, had a
test pipeline through `flvmux` not hung in 1.20.

The second problem is that a running tool takes no orders: it cannot be
asked for an IDR or told a new bitrate (the va plugin can change its
bitrate in PLAYING, but `gst-launch-1.0` gives no way to reach it). The
encoder restarts the tool for both, since a fresh one starts with an IDR at
whatever bitrate it is given; on the Intel iGPU below that takes about 40
ms (35 to 50) from the request to the IDR, start included, against 5 to 7
ms for openh264.
A periodic keyframe was the other option and lost: frames reach the encoder
only when the screen changes, so a GOP counted in frames is not a period in
time, and a two-second timer would spend an IDR (about 100 KB at 2 Mbit/s,
a fifth of the budget) every two seconds to make recovery take a second on
average. The tools get an effectively infinite GOP instead. For bitrate, a
restart waits until the congestion controller's target has moved twofold
from what the tool runs at, and at least two seconds since its start,
because each one costs an IDR. In between, a token bucket holding 500 ms of
the target skips frames when the output runs ahead of it; a skipped frame
is retried on the next tick, as with openh264. The bucket is not optional:
on full-screen noise at 1080p the Intel encoder sent 4.4 Mbit/s against a
500 kbit/s target and against a 2 Mbit/s one, and neither the QP range nor
the CPB size moved it. With the bucket it sent 2.0 to 2.1 Mbit/s against 2
Mbit/s. A target stepped 500, 4000, 500 kbit/s was followed at 583, 2702,
616 kbit/s, where openh264 gives 672, 2588, 669.

Rate control is VBR with the target as the peak and 70% of it as the
average. Under CBR the hardware spent the whole target on any picture, 17 to
20 Mbit/s at a 20 Mbit/s target for a typing pattern openh264 sends in 62
kbit/s, and a QP floor (`min-qp`, `-qmin`) changed nothing. Under VBR the
same frames cost 0.7 to 1.6 Mbit/s and scrolling 2.3 to 2.9 Mbit/s, where
openh264 spends 8.

What it buys, measured with `TestCompare` on an i7-13800H and its iGPU
(iHD 22.3, ffmpeg 4.4, GStreamer 1.20), per encoded frame at 30 fps with a
target high enough that nothing is skipped, ranges over typing, scrolling, a
video window and full-screen noise:

| | openh264 | ffmpeg `h264_vaapi` | GStreamer `vaapih264enc` |
|---|---|---|---|
| time per frame, 1080p | 1.2 to 3.2 ms | 4.8 to 5.1 ms | 5.5 to 6.1 ms |
| CPU, agent and tool, 1080p | 2.9 to 8.5 ms | 2.6 to 2.8 ms | 3.2 to 3.9 ms |
| time per frame, 2160p | 5.0 to 10.1 ms | 18.6 to 19.8 ms | 30.8 to 34.2 ms |
| CPU, agent and tool, 2160p | 12.2 to 28.2 ms | 11.5 to 12.5 ms | 21.1 to 22.1 ms |

Most of the hardware routes' CPU is the copy: a 2160p frame is 12 MB
through a pipe and into a GPU surface, about 5 ms of it in the agent. In a
real session it evened out. Scrolling an xterm through the CLI and Chrome
cost about a fifth of a core at 1440x900 and about one core at 3840x2160
with either encoder, since capture, colour conversion and the copy into the
pipe stay in the agent and outweigh what the GPU takes off it. So hardware
encoding helps where openh264 is the bottleneck, on slow CPUs and on busy
4K content, and costs latency everywhere: several milliseconds a frame
through the pipe and a restart per requested keyframe.

It started out opt-in for that reason. The original design asked for the
probe to pick it, and these numbers say where a pick is right, so `auto`,
the default, applies them (`probe.Report.AutoEncoder`). The GPU is taken
when the probe shows a working route and either of two things holds. The
host has four cores or fewer: openh264 needs up to 8.5 ms of a fast core
per 1080p frame, a quarter of a core at 30 fps, which a small host feels and
a slow core makes worse. Or the session may exceed 1920x1080 and ffmpeg
drives the GPU: at 2160p openh264 spends up to 28 ms of CPU a frame on busy
content against 12 through ffmpeg. GStreamer's route does not count for
large sessions, since it spends 31 to 34 ms on a 2160p frame, a whole frame
interval at 30 fps, for 21 ms of CPU. VA-API comes before NVENC, which has
not run on NVIDIA hardware. A 1080p session on a host with more cores stays
in software, where the GPU would save a few milliseconds of CPU a frame and
add as many of latency. Because a pick is a guess from the probe, the CLI
marks it (`Start.EncoderFallback`): a picked encoder that fails its trial
frame, or later at the session's size, gives way to openh264 for the rest
of the session and Ready says why, where an encoder the user named still
fails the session.

The first `Open` tries each route the host offers on a 320x240 frame,
ffmpeg first since it measured faster and cheaper, and keeps the first that
returns a Baseline IDR before a second frame is sent; a tool that held
frames back would pair every picture with the one before. Low-power
variants are tried as well: on this GPU iHD 24.1 lists H.264 encoding only
under the low-power entrypoint, and the va plugin then offers only
`vah264lpenc`. ffmpeg can report a
failed encoder and then wait on stdin forever, so once it prints an error,
which with `-loglevel error` is all it prints, the frame in flight gets
100 ms more. Each tool runs in its own process group, killed whole on close
and by Pdeathsig if the agent dies first. The session opens a small
encoder before it brings the desktop up, so a host whose tools fail
reports it to the CLI instead of streaming a black screen, and the CLI
checks the probe (a render node the user can open, or a loaded NVIDIA
driver, and a tool with the right encoder) before it deploys anything.

Verified on Ubuntu 22.04 (GStreamer 1.20, ffmpeg 4.4) and, in a rootless
podman container on the same GPU, Ubuntu 24.04 (GStreamer 1.24 with the va
and vaapi plugins, ffmpeg 6.1, iHD 24.1): every route emits Constrained
Baseline that ffmpeg decodes, and Chrome decoded both the ffmpeg and the
GStreamer route at 30 fps under `profile-level-id=42e01f`. The NVENC routes
follow the vendors' documented options and have not been run on NVIDIA
hardware.

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

The session gets a locale even when SSH brought none. With PAM, sshd reads
the system default into the environment, and many clients pass their own
`LANG` on; without either, as with the `UsePAM no` sshd of the manual run on
an Ubuntu 22.04 workstation, the desktop ran in the C locale and xterm showed
the prompt's UTF-8 as mojibake and misdrew the line after editing it. The agent
now does what a display manager does: it keeps any `LANG`, `LC_ALL` or
`LC_CTYPE` it was given, else takes `LANG` from `/etc/locale.conf` or
`/etc/default/locale`, else uses `C.UTF-8`.

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

### Zero playout delay on video

Every video packet carries the playout-delay header extension with a minimum
and maximum of zero, so Chrome decodes and shows a frame as soon as it is
complete instead of holding it for its jitter estimate. On a still screen
the agent sends nothing until Chrome, after 3 s without a frame, asks for a
keyframe. The jitter estimate grows on that trickle of large frames until it
holds new frames back for most of a second, so what you type after a pause
showed late. The e2e measures it after 20 s of a still screen. In the
direct UDP scenario, before the change, four keystrokes 250 ms apart
produced one frame on screen, 414 ms after the first key and held 397 ms
after it arrived; the last two keystrokes had not shown 1.5 s later. With
the extension each keystroke reached the screen within 11 to 34 ms, with
frames held about 1 ms, and the other scenarios agree: 9 to 33 ms through
the SSH tunnel, 7 to 31 ms after failing over to it mid-session, 6 to 36 ms
attached. A
desktop stream has no reason to smooth playout the way a call does, and a
TCP fallback that delivers in bursts now shows each burst as it lands.
Browsers that do not offer the extension, Firefox among them, keep their
own playout timing.

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
Before any of that the agent asks the kernel (`SO_PEERCRED`) who listens on
the socket and goes no further unless it is the user or root. Any local user
can create a socket in `/tmp/.X11-unix` for a display nobody runs, or claim
the abstract name of one whose server has no socket file, and whatever
answers there would get the cookie, the keystrokes and the clipboard.

For `--attach auto` the CLI first looks for a Wayland session: when logind
says the active one is Wayland (or there is no logind) and the probe sees a
compositor socket and the session bus, it attaches through the portal
described below. Otherwise it takes the display of the user's active local
X11 session from logind. Under GDM that field is empty, because GDM registers
the session before its X server starts (the GNOME workstation used for the
checks below reports exactly that), so the fallback is the one socket in
`/tmp/.X11-unix` the user owns. A Wayland session would pass that test too,
through Xwayland, whose root window shows none of the desktop, so with an
active Wayland session the probe cannot reach, `auto` refuses to guess and
lists what it found.

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

Shrinking a whole multi-monitor desktop did not survive its first real use.
Attached from a laptop to that workstation, the 5120x2640 root (two 2560x1440
screens with a 1920x1200 panel below them) went out at 1920x990, 2.7 times
smaller than on the screens, and text could not be read. So an attached
desktop shows one RandR 1.5 monitor at a time: the primary by default, any
other by `--monitor` or from the page, or `all` for the old single picture.
The X11 connection keeps a viewport rectangle; capture copies only that part
of the root with MIT-SHM, pointer positions count from its corner, and the
picture still shrinks when one monitor exceeds the session maximum. DAMAGE
now reports bounding boxes, so drawing on a monitor that is not shown costs
no capture; an event outside the viewport resets the box, so the next
drawing anywhere is reported again rather than hidden inside a box that
already covers it. The viewport also cuts the cost of the hash the agent
takes when DAMAGE proves unreliable, which that workstation's mutter did:
grabbing and hashing the 5120x2640 root takes 11.7 ms, one 2560x1440 monitor
3.2 ms (`BenchmarkCaptureThreeMonitors`), about a third of a core at 30 fps
against a tenth. Monitors made with `xrandr --setmonitor` change without any
RandR event, so the agent lists them again every second as well as on each
CRTC, output or screen change; when the monitor shown goes away, it shows the
default one and tells the page. One browser tab shows one monitor; a window
per monitor, as some remote desktop clients open, would take a video track
per monitor and is not built.

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

### Sound: the PulseAudio protocol in Go, Opus from a static libopus

The agent records the desktop's sound and sends it as a stereo Opus track
beside the video. Options for the capture:

| Option | Verdict |
|---|---|
| libpulse through cgo | The static agent cannot load the host's libpulse, and a static copy brings its own dependencies along. Rejected. |
| `parec` or `pw-record` as a subprocess | Needs those tools on the host, and one more process to supervise. Rejected. |
| PipeWire's native protocol | A client of its own, and PulseAudio hosts would still need this one. Rejected. |
| PulseAudio's native protocol in Go | No host libraries, and PipeWire serves the same protocol through `pipewire-pulse`, so one client covers both. **Chosen**, with the protocol layer of `jfreymuth/pulse` (MIT). |

The agent drives that protocol layer itself, one connection and one record
stream, rather than the library's client: it needs to hear when the server
ends the stream, and to send its own server's cookie. The library prints
opcodes it does not know with `fmt.Println`, which is harmless only because
the agent's stdout has already been moved to stderr. The stream records the
monitor of the server's default sink at 48 kHz stereo in 20 ms fragments, and
each fragment is encoded on the protocol's read goroutine as it arrives; a
session that falls behind loses packets rather than stalling the reader.

A spawned desktop has no sound server, so the agent starts a private
pulseaudio for it: no default script (`-n`), so no module touches sound cards
or Bluetooth; a null sink as the default output; the native protocol on a
socket in the run directory with a cookie beside it; `HOME` and the runtime
and state paths inside the run directory; and a session bus address that leads
nowhere, so it can neither meet the user's own sound server over D-Bus nor
autolaunch a bus. The desktop's programs get `PULSE_SERVER` and
`PULSE_COOKIE`; without the cookie variable libpulse writes a cookie into
`~/.config/pulse`, which the e2e host checks for. The daemon is one process
that forks nothing, in its own group with Pdeathsig, so it needs none of the
display's watchdog; a test kills an agent with SIGKILL and checks the daemon
goes with it. PipeWire as the private server was considered and not built: it
needs WirePlumber to link streams, WirePlumber 0.4 and 0.5 take different
configuration languages, and its default configuration claims the host's
sound cards. Hosts with only PipeWire get sound when attached, not in a
spawned session.

The null sink needed `norewinds=1`. Each rewind drops what the monitor had
queued, and with 20 ms fragments a silent desktop delivered a third of real
time (parec agreed: 70% at 20 ms latency, all of it at 100 ms). Every packet
advances the RTP clock by 20 ms, so the browser's clock would have fallen
behind the desktop's. With rewinds off the monitor delivers exactly 50 frames
a second, silent or not (`internal/agent/audio`, `TestCost`).

Opus runs in `OPUS_APPLICATION_AUDIO` mode at 96 kbit/s and complexity 10,
with in-band FEC on and 5% expected loss. Without some expected loss libopus
never codes the redundancy. With it, speech-like sound goes out in hybrid
mode, whose SILK layer carries the previous frame again at low rate, while
tones and music stay in CELT, which has no in-band FEC, so a lost music
packet is left to the browser's concealment. One cost: silence right after
speech-like sound stays in hybrid mode at about 28 kbit/s until something
else plays, where CELT codes silence at 1.2 kbit/s. Measured in the X test
container, the whole audio path, agent and pulseaudio together, costs about
1.5% of a core while silent and 4.5% while playing speech-like noise at 90
kbit/s; complexity 5 would save about 1.5 points of that.

Browsers autoplay only muted media, so the page starts muted and shows a
speaker toggle once an audio track arrives; unmuting takes that click. Chrome
reports `totalAudioEnergy` only for audio it plays: a muted element reports
zero while packets arrive, so the e2e clicks the speaker before it plays
noise on the host and checks the energy rises.

### Attaching to Wayland: the portal, not mutter's own API

A Wayland compositor lets no client read the screen or inject input on its
own say-so, so attaching goes through an interface the compositor offers.

| Option | Verdict |
|---|---|
| `org.gnome.Mutter.RemoteDesktop` and `ScreenCast` directly | Works with no dialog at all: any process running as the user can watch and drive the screen without the person in front of it ever knowing. GNOME only. Rejected for exactly that. |
| xdg-desktop-portal `RemoteDesktop` and `ScreenCast` | The desktop asks the person at the screen, shows that sharing is on, and lets them stop it. GNOME and KDE Plasma implement it, and a restore token skips the dialog after the first approval. **Chosen.** |
| libei through the portal's `ConnectToEIS` | Not needed: the portal's `Notify*` methods carry keyboard, pointer and wheel on GNOME 46 and later and on Plasma 6, and libei would be a C library in a static agent. |

wlroots compositors such as Sway have no `RemoteDesktop` portal and are not
covered. The agent talks D-Bus with `godbus`, so it stays pure Go. The
mutter-direct path appears in one place, the test harness, which uses it to
click the consent dialog in a container where nobody is at the screen.

The same agent passes the same suites on GNOME 46, GNOME 49 and KDE Plasma
6.7 without a line that knows which desktop it is on. Three differences
showed up on KDE, none of them in the agent. KDE's dialog grants the
devices the request names or nothing, so there is no view-only grant to
handle there. KWin screencasts only while it composites with OpenGL: in a
container without a GPU it falls back to QPainter and the portal fails
Start with "Unsupported compositing type", so the KDE test container
borrows the host's render node. And the harness answers KDE's dialog by
pressing Approve through AT-SPI, which Qt serves only when
`QT_LINUX_ACCESSIBILITY_ALWAYS_ON` is set, as the test session does.
Restore tokens, the clipboard's MIME list, keysyms, the embedded cursor and
discrete wheel steps behave as on GNOME.

### The restore token is a credential, spent by every Start

Persistence is requested on `SelectDevices` with `persist_mode` 2, until
revoked; the ScreenCast spec forbids it on `SelectSources` for a remote
desktop session. Whoever holds the token and can reach the user's session
bus can start a session without a dialog, so it lives in the agent's
directory as `wayland-restore-token`, mode 0600, written to a temporary file
and renamed into place. The name keeps deploy's sweep of old agent files off
it.

Each `Start` consumes the token it was given. On success the token it returns
replaces the file. On any failure the file goes, and if the failure came
with a token the agent asks once more without one, unless the user declined
or nobody answered. A grant without both keyboard and pointer is never kept:
GNOME's Allow Remote Interaction switch is off by default, and a remembered
view-only grant would make every later session silently view only. If a
restored grant comes back view only anyway, the agent drops it and asks
with the dialog. Every request asks for the same devices, one monitor, the
same cursor mode and the clipboard, because KDE rejects a token for a
request that differs from the one it was issued for.

The wait for an answer is two minutes. Without a token the agent tells the
CLI at once, with a `notice` message before `Ready`; with one it waits two
seconds first, since a valid token answers immediately and only a revoked
one brings the dialog back. The CLI prints who has to do what on which
screen, and stretches its own wait for `Ready` to match, by at most five
minutes a notice and eleven in all, so an agent that keeps sending notices
cannot hold it for ever. If the CLI goes
away meanwhile, the agent closes the request, which by the portal spec ends
the dialog too.

### Wayland frames through gst-launch-1.0, in I420

The portal hands over a PipeWire connection that can see only the shared
stream. There is no pure Go client for PipeWire's native protocol and its
SPA pod format, and linking libpipewire would end the static agent, so the
agent runs `gst-launch-1.0` with that connection as fd 3:

```
pipewiresrc fd=3 path=N keepalive-time=100 ! video/x-raw,max-framerate=31/1
  ! videoconvert ! videoscale ! video/x-raw,format=I420,colorimetry=bt601,width=W,height=H
  ! y4menc ! fdsink fd=1
```

This does put GStreamer on the host, which the encoder decision above
rejected. That was about spawn mode working on a bare server; a host with a
Wayland desktop already runs PipeWire and most of GStreamer, and usually
lacks only the tools package.

`max-framerate` goes into the PipeWire format offer, so the compositor
paces the stream; one frame over the target leaves room for jitter.
`keepalive-time` resends the last frame on a static screen, without which
the first frame would wait for something to change. The agent compares each
frame with the last one it handed out and reports a repeat as no change, so
an idle desktop still encodes nothing. `y4menc` frames the byte stream and
states the size, and it copes with any row padding the converter leaves.
`videoscale` brings a screen larger than the session's maximum down to fit.

I420 won on measurement, a fully animated 1280x720 screen at 30 fps:
`gst-launch-1.0` spent 6% of a core converting to I420 against 4% passing
BGRx through, while converting BGRx in the agent costs about 4.5% of a core
in Go and moves 2.7 times the bytes through the pipe. So `desktop.Frame`
carries its pixel format and the stream copies an I420 frame instead of
converting it.

The pointer is embedded in the frames. Cursor metadata travels in PipeWire
buffer headers, which `gst-launch-1.0` cannot pass on, so the agent tells the
page to hide its own pointer over the video. The remote pointer then moves
at the video's latency, which is the cost.

### Wayland input: the compositor picks the keycode

Printable characters go in as keysyms through `NotifyKeyboardKeysym`, and
mutter finds the keycode and presses Shift or AltGr as the level needs. A
keysym missing from the active layout is dropped: a Wayland client cannot
change the compositor's keymap, so the spare-keycode trick the X11 path uses
has no equivalent. GNOME drops the first key event of a remote desktop
session and, about one session in five, leaves the first pointer event
unseen by clients, so the agent opens with a `VoidSymbol` tap, which no
layout has, and a zero relative motion.

Held keys are not repeated from the browser. Wayland clients repeat a held
key themselves at the rate the compositor announces, so passing the
browser's repeats on as presses would double them. Wheel input goes as
discrete steps, one per 100 browser pixels as on X11: smooth scroll's sign
differs between GNOME and KDE, discrete steps do not.

## Verification parked

Two checks are parked by decision (2026-10-18), not forgotten. Both need
hardware the test workstation cannot offer without changes the owner chose to
leave for later.

- NVENC on NVIDIA hardware. The workstation has an NVIDIA GPU and the 535
  driver's userspace, but no kernel module for its running kernel, and
  installing one needs root on a Puppet-managed machine. NVENC is tested
  against stand-in tools only, and `--encoder auto` prefers VA-API over it
  for that reason. To close it: install `linux-modules-nvidia-535-$(uname -r)`,
  load `nvidia`, run `ROBIN_HWENC_REQUIRE=nvenc make test` and one 4K session
  with `--encoder nvenc`.
- The macOS client and Safari. Every CLI package, tests included, compiles
  and passes `go vet` for darwin/arm64, but none of it has run on a Mac, and
  Safari's picture is unverified because Playwright's Linux WebKit renders no
  remote WebRTC video. To close it: run `dist/robin-darwin-arm64 gui` from a
  Mac against a Linux host and open the page in Safari and Chrome.
