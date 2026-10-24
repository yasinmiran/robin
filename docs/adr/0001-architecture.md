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
and would still miss cases. Cost: one connection needs OpenSSH with
ControlMaster; Windows, whose OpenSSH has none, gets a connection per
command instead (see below).

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

### Coverage: every suite, merged per package

Most of the agent runs only where there is a display or a sound server, so
the unit tests alone undercount it badly: on 2026-10-24 they read
`internal/agent/x11` at 7.8% and `internal/agent/wayland` at 13.6%, which
says nothing about what test-x, test-wayland and e2e exercise. `make cover`
runs them all with coverage and merges the result.

- Every Go suite instruments the whole module: `-coverpkg=./...` for the
  unit tests and the test-x and test-wayland binaries, and `go build
  -cover`'s default, the main module, for the e2e CLI and agent. A
  statement counts wherever a test reached it, so the agent session tests
  credit `x11` and `peer`, not only `internal/agent`.
- The merge (`internal/tools/covercheck`) keys blocks by position and calls
  one covered when any profile ran it. That also settles the modes: the
  race-built container binaries count in atomic mode, everything else in
  set mode, and the merged profile is set mode for `go tool cover`. A
  package's number includes every file some build compiles, so
  `internal/agent/h264` counts both its openh264 file and its stub.
- The agent finds `GOCOVERDIR` through the e2e host's own sshd, `SetEnv
  GOCOVERDIR=/cover`, with the run's directory mounted there. A `-cover`
  binary writes its counters only when it returns from main or calls
  `os.Exit`; each scenario's SIGINT teardown ends both the CLI and the agent
  that way, and anything killed hands in nothing.
- The cover agent reaches the CLI through `go build -overlay`, which swaps
  `internal/agentbin/agents/robin-agent-linux-amd64.gz` in that one build.
  No source file knows about coverage, and `$(AGENTS)` never holds a cover
  agent for a release build to embed.
- The page is measured the way V8 reports it, from Chrome in e2e
  (Playwright's `page.coverage`, which only Chromium has) and from Node in
  `make test-js` (`NODE_V8_COVERAGE`), one format that
  `internal/tools/pagecover` merges. It counts lines rather than statements,
  which would take a JavaScript parser: a line holds code unless it is
  blank, a comment or closing brackets, and is covered when any of it ran.
- `coverage-baseline.txt` holds a number per package, written only from
  main. A package more than 0.5 points below its line fails `make cover`,
  and so does one that has vanished, until the baseline is rewritten; a new
  package passes until main records it. Per package rather than per file,
  so moving code between files never trips it.
- Node 18 decides whether `static/stats.js` is an ES module from the
  nearest `package.json` above that file, not above the test importing it,
  so `internal/web/static/package.json` says `"type": "module"`. The page
  embed lists its file types, so neither that file nor `static/test/` is
  served.

The first full run, on 2026-10-24 with warm build and image caches, took
7 min 40 s: test-x 50 s, test-wayland 2.5 min across GNOME 46, GNOME 49 and
KDE, the seven e2e scenarios 4.3 min, and the unit tests seconds from Go's
test cache (about 30 s cold). The unit tests alone cover 48.4% of the
module's 5158 statements, test-x and test-wayland bring that to 80.6%, and
e2e to 85.4%; e2e on its own covers 64.0% of the 4591 statements its two
binaries link. `x11` goes from 7.8% to 85.9% and `wayland` from 13.6% to
76.9%. Every scenario handed in CLI and agent data, and the five that run
Chrome the page's as well. Of the page's 569 code lines, Node's unit tests
of two modules cover 9.8% and the merged run 87.2%.

### Bulk transfers: a data channel each, checked end to end

Clipboard images, files and lossless tiles do not fit in a 256 KiB control
message, so a transfer layer went in under them before any of them existed.
Each transfer opens a data channel of its own, labelled `bulk`, from
whichever side sends. SCTP streams are cheap, and with one transfer per
channel, cancelling is closing it. The sender writes a JSON header (kind,
id, name, MIME type, size), the payload in binary chunks of 64 KiB and an
end frame carrying its SHA-256; the receiver answers with one verdict, done
or error, and closes. Without that verdict the sender could not tell a kept
transfer from one that failed its hash, or hit a full disk after the last
byte had already gone out. 64 KiB sits well inside the 256 KiB message
limit both the agent and Chrome set.

A feature registers each kind it owns with a size limit
(`protocol.RegisterBulkKind`, and `registerKind` in the page). The sender
checks the limit before it opens a channel, the receiver before it shows
the header to the feature, which may still refuse for reasons of its own.
Kinds nobody registered are refused, as are data before the header, chunks
over 64 KiB, bytes past the declared size, an end that comes early and a
hash that does not match; the sink being written to hears why. The
verdict goes out before the sink's abort, so a refused sender does not wait
on the sink's cleanup to learn of it.

Flow control is the WebRTC kind on both sides. A sender stops queuing at
1 MiB of `bufferedAmount` and goes on at 256 KiB, on `bufferedamountlow`,
and it wakes on a close or a cancel as well: Pion drops the low-water
callback once a channel is closing, so a sender that waited for that alone
would hang (`TestBulkCancelWhileBlocked` closes the far end mid-wait). The page hashes as it goes with its own
SHA-256, because Web Crypto digests a whole buffer at once and a file can be
bigger than the page can hold. That code is checked against `node:crypto`
at every length across the padding boundaries and in random pieces, and
against Go by the Chrome test, where a transfer only passes if the agent's
hash agrees.

In-process over loopback, without the race detector, 8 MiB crosses between
two Pion peers in 115 to 150 ms either way, and 5 MiB goes from Chrome to
the agent's peer in 170 to 360 ms, both bound by the CPU. On a slow link the
picture pays. Bulk is not paced: SCTP's loss-based congestion control fills
the bottleneck queue and GCC reads the delay as congestion. Over the shaped
link of `TestBulkSharesTheLinkWithVideo`, 3 Mbit/s with a 100 ms queue and
video settled at 2.9 Mbit/s, a 2 MiB transfer took 9.0 to 9.6 s at about
1.8 Mbit/s while the video got about 1 Mbit/s and its target fell to 0.75
Mbit/s. Nothing disconnected, and the target was back to 80% 7 to 9 s after
the transfer ended. A feature that sends steadily, such as sharp-text tiles,
has to pace itself under the target. A probe found one more coupling worth
knowing: Pion gives an association one receive window for all its channels,
so a sink on the agent that stopped reading held a control message back for
as long as the probe waited, 3 s, and a sink has to keep up or fail the
transfer rather than stall it. Its commit and abort run on the same read
loop and stall everything the same way.

### Features register what they handle

Several features are built in parallel (docs/plan-horizon-parity.md), and
each needs control messages, bulk kinds or a place in the status tab. So
nothing switches on message types any more. On the agent, each control type
and bulk kind maps to a handler its feature registers from `init` in its
own file (`handleControl`, `handleBulk`), and a unit test fails for any
handled bulk kind without a limit. A control type that is input registers
with `handleInput` instead, which is how it waits behind typed text (below)
without the text feature listing it; the page has `onControl` in
`control.js` and `onBulk` in `bulk.js`, and `Overlay.addSection` gives a
feature its own part of the status tab without touching `index.html`. The
peer routes data channels by label, closing any it does not know, and hands
remote media tracks to `Handlers.Track`, unset until the microphone and
camera arrive. Protocol version 2 marks the change, so a CLI and an agent
from either side of it refuse each other.

### Kept sessions: the desktop lives in a daemon, the SSH channel in a proxy

Until now the agent was the SSH exec channel: stdin closing meant teardown,
within 20 s of a dead network thanks to the idle watchdog. That is the right
reflex for a leaked display and the wrong one for a laptop lid. The change
keeps the reflex and moves it one process out.

- One abstraction, `agent.Controllers`, hands the session its controller
  connections one after another: the single stdio connection as before, or
  the connections a daemon accepts on a unix socket. `agent.Serve` is the
  old `Run` body around that: Start on the first controller, Resume on the
  later ones, the same Ready for each, and between controllers a linger of
  `--keep`. Nothing that holds state across controllers moved: the desktop,
  the audio source, the ICE-TCP listener, the stream, the cursor cache and
  the DTLS certificate were all already owned by the session and now simply
  outlive the connection. So does the fingerprint the CLI pinned, which is
  what makes a resumed answer acceptable to the relay and anything else
  answering fatal.
- `robin-agent serve` on the exec channel became a proxy: it starts (or,
  with `-resume`, finds) `robin-agent daemon`, waits for the socket, and
  copies bytes both ways, so the CLI's pings reach the daemon and the 20 s
  idle rule is unchanged, only its consequence is: a controller that goes
  silent is dropped, its peer closed, and the keep window opens. The
  daemon's log goes to a file in the run directory that the proxy tails to
  its stderr, so the CLI's "agent log:" on an error reads as before.
- Leaving the SSH session: the daemon is started with `setsid`, stdio on
  `/dev/null` and SIGHUP ignored, which is enough wherever logind leaves
  user processes alone on logout, the upstream default (`logind.conf(5)`:
  "Defaults to no"). `KillUserProcesses=yes` kills the
  whole session scope regardless of process sessions, so there the probe
  (busctl's live answer, else the last `logind.conf` setting read) makes
  the CLI ask for `systemd-run --user`, a transient unit outside the scope.
  That unit still stops with the user's manager when their last session
  ends unless lingering is on, so the CLI warns, naming
  `loginctl enable-linger`, and warns differently where `systemd-run` is
  missing, where the desktop will end with the connection after all. The
  containers the tests run in have no systemd, so the `systemd-run` path
  has a unit test for its command line and a hand check on a real host
  (see the README's Known limits); the `setsid` path is what test-x and e2e
  exercise.
- Teardown guarantees moved with the ownership: children are spawned by the
  daemon, so their process groups, `Pdeathsig` and the display watchdog
  die with it; the keep window expiring returns from `Serve` through the
  same defers as bye, and so does the desktop dying while nobody is
  attached. test-x proves all three with the real binary: SIGKILL of the
  proxy's process group leaves the daemon and Xvfb running (same pids), a
  second proxy resumes, and 4 s after it leaves the daemon, Xvfb, the
  watchdog and the run directory are gone. `robin-agent end` sends SIGTERM
  to the pid in `session.json` only after `/proc/<pid>/cmdline` shows it is
  that session's daemon, since the pid may have been reused, and removes a
  stale directory instead.
- Trust: the socket lives in the 0700 run directory and the daemon checks
  `SO_PEERCRED` against its own uid, one controller at a time (a second is
  told "another CLI is connected" after a 2 s handoff wait, long enough for
  the daemon to get from one controller to the next). A fresh `robin gui`
  reaches the daemon through the same SSH login that started it and pins
  the fingerprint of whatever it finds, which is what the first run did too.
- Identity across deploys: `deploy.Ensure` removes other agent versions
  from the directory, and Linux keeps the daemon's unlinked binary running,
  so after an upgrade the daemon runs code the CLI no longer carries.
  `session.json` records the agent's file name, which is its content hash,
  and a CLI whose embedded agent differs refuses to resume and says to run
  `robin end`. The version string is `dev` on every local build and would
  not have caught that.
- The CLI: the relay, the page server, the tunnel's loopback listener and
  the page itself outlive the SSH connection; only the master, the proxy
  and the forward are remade. The page is told `reconnecting`, drops its
  peer connection and builds a fresh one from the next `config`, the same
  path a reload takes. Redials back off from 1 s to 15 s until the keep
  window closes. Ctrl+C has to end the session, and a terminal delivers it
  to ssh as well: OpenSSH's sessions over the master die of it, but the
  master itself keeps an inherited SIGINT ignore, so the master is started
  through `sh -c 'trap "" INT; exec ssh ...'` and the CLI runs the agent's
  end command over it when bye got no answer (a bye that did arrive leaves
  no session to end, which counts as done); only when the master is gone
  too does it dial once more, bounded to 30 s, and failing that it names
  the time the host lets the desktop go. In the e2e teardown after the
  resume, the bye's answer was lost with the session's ssh, the end command
  over the surviving master found the session already gone, and the CLI
  exited 0 in 399 ms with one `connecting` line in its whole log. The
  page's End session button takes the same path: the bridge reports the
  wish and forwards nothing, the CLI sends bye and falls back to the end
  command, and only then tells the page `ended`; a socket that closes
  first leaves the page saying the end is unconfirmed. Errors that stop
  the CLI after Ready end the desktop the same way. What the CLI acts on
  crosses the wire as a code, not as wording: the daemon's busy refusal
  carries `protocol.CodeBusy`, and `robin-agent end` exits
  `protocol.ExitNoSession` when there is nothing left to end.
- Measured in the `resume` e2e scenario on 2026-10-24: killing the host's
  sshd children drops every connection at once, the page shows
  Reconnecting 5 ms later and is connected again 263 ms after the cut on
  the same display, Xvfb and xterm, with 50 of the xterm's 152,944 pixels
  changed against the frame saved before the cut. The `rejoin` scenario
  kills the CLI outright, resumes from a new `robin gui`, detaches from the
  page and ends with `robin end`, leaving no process or directory behind.
  In test-x a session with a 2 s keep window was gone, run directory and
  Xvfb included, 2.10 s after its controller left.

### Files: into the download folder, out on a click

The page writes files on the host now, so the first decision was where, and
the answer is one place: `$XDG_DOWNLOAD_DIR`, else what
`~/.config/user-dirs.dirs` says (xdg-user-dirs writes the variable there,
not into the environment), else `~/Downloads`, created 0700 when missing.
A download folder that is home itself counts as unset: xdg-user-dirs-update
writes `$HOME/` for a folder it cannot find, and an upload into home could
create a startup file the user lacks, `.bash_login` or `.zshenv`, which runs
at the next login. An upload is a bulk transfer of kind `file`. Its name has
to pass `files.CheckName`: one path component of valid UTF-8, at most
NAME_MAX's 255 bytes, not `.` or `..`, no control characters (NUL and
newlines among them), and none of Unicode's bidirectional controls: with a
right-to-left override, U+202E, after `invoice`, the name
`invoice<U+202E>fdp.exe` shows as `invoiceexe.pdf`. An upload also may not
start with a dot, so it is never hidden. The bytes go into
`.robin-upload-<random>` in the same directory, opened with
`O_EXCL|O_NOFOLLOW`, so the file is new and no symlink can redirect it, and
the rename stays on one filesystem. Only once the SHA-256 matched does it
get its name, by `renameat2` with `RENAME_NOREPLACE`; a name that is taken
moves on to `name (1).ext`, the number going before the extension, and
before `.tar` in `a.tar.gz`, with the stem cut at a character boundary to
stay within 255 bytes. A filesystem without the flag gets a hard link and an
unlink, which cannot replace either. An upload that would leave less than
256 MiB free, counting what uploads in progress still have to write, is
refused before a byte lands, so the desktop's own disk does not fill up.

A sink runs on the data channel's read loop, and holding that loop holds the
keyboard and pointer too, so `Upload.Write` never waits. It hands each
chunk, as Pion delivers it in a slice of its own, to a writer goroutine
through a queue of 256 chunks, 16 MiB, and fails the transfer when the queue
is full. On the workstation (ext4 on NVMe) that writer drains into the page
cache at 2.3 to 4.1 GB/s, where the bulk layer delivers 15 to 30 MB/s from
Chrome, so the queue fills only when the disk stops answering, and then the
upload fails instead of the session freezing. At most four uploads run at
once, which bounds the queues to 64 MiB. Commit has to wait, since the
verdict is what it returns: it waits for the drain and the rename, which
took 3 to 7 ms from 16 MiB to 1 GiB, and gives up after 5 s. Accept has
to ask the disk too, to make the folder, read the free space and create the
file, and does it off the inbox's lock with the same 5 s bound, so a
download folder on an NFS or FUSE mount that stopped answering fails the
upload rather than freeze the session and its teardown. Cancel,
failure and the end of the session remove the partial file; only a SIGKILL
leaves one, or a writer still stuck on the disk 5 s after the session ended,
since the agent exits rather than wait on it for good. The agent does not
sweep the user's download folder for names that look like its own. A rename
that finishes after Commit gave up removes the file it just named, so the
page, told the upload failed, never finds it there under its name.

There is no fsync before the rename. Flushing after the last byte took 15,
42 and 213 ms for 16 MiB, 256 MiB and 1 GiB on that NVMe disk, all of it on
the read loop; a disk that writes 150 MB/s would hold every channel about
7 s for a gigabyte, past any bound that leaves the session usable. A power
cut in the seconds after an upload can leave it short, which the README
says.

Downloads run the other way round, offer first and bytes after the click.
`robin-agent send FILE...`, also on a spawned desktop's `PATH` as a
`robin-send` link to the agent (it answers to that name), opens each file
itself and passes the descriptors with `SCM_RIGHTS` over `files.sock`, a
sequenced-packet socket in the 0700 run directory, mode 0600, whose peer
credentials must name the session's own uid. The page never names a path;
the session sends exactly what its sender could open, re-reading the size
when the user clicks Save, and an offer lasts as long as `robin-send`'s
connection, which hears saved, declined or failed for each file. It finds
the session by `ROBIN_FILES_SOCK`, which spawned desktops get, or as the only
`run/*/files.sock` that answers, and with several it refuses to guess.
Sending at the offer would have parked every offered file in the page's
memory and given the host a way to push a download into the browser; with
fetch on the click it has neither, as long as the page spends each click on
one transfer: a header for an offer that is not waiting on its click is
refused, so a host that opens a second channel for the same ID gets nothing
from it. Uploads hold to the same rule from the other side. A paste of files
lists them as uploads that each wait for a click on Upload, since the
browser's clipboard may still hold files copied long before, and Ctrl+V
meant a paste into a remote program, not a copy off the client. The page shows name and size as
`textContent`, gathers a download in 16 MiB blobs and saves it through an
anchor with `download`, which also lets the browser make the name safe for
the client's disk: Chrome turns `<` and `>` into `_`. Since the page holds a
download until it is whole, the 4 GiB limit on kind `file` is a bound on the
browser's memory as much as on the transfer.

### The clipboard: text, HTML and PNG in one transfer

The rich clipboard carries text/plain, text/html and image/png, the three
types `navigator.clipboard.write` takes in every browser with
`ClipboardItem`. One copy travels as one transfer of kind `clipboard`
holding each of its formats as a part (a byte of MIME type length, the type,
four bytes of data length, the data), up to 16 MiB in all, so the far side
offers them together as one clipboard the way the source did. Plain text
that fits a control message still goes as one. A newer copy on the host
cancels the transfer of an older one still under way, and the agent takes
two from the page at once at most, since each waits whole in memory and
only the newest counts.

Setting the clipboard is the one thing a sink does on the read loop on
purpose. The page holds the paste key until the transfer's verdict, for up
to 3 s, and the agent sets the clipboard before it answers, so the paste
that follows finds the new content; handed to a goroutine, the key could
win the race and paste the old one. The cost is one X round trip, 0.13 to
0.23 ms for a 3 MiB clip in test-x and capped by the 2 s wait for a
timestamp, or one D-Bus call to the portal on Wayland.

On X11 the agent asks a new owner for `TARGETS` and then, one at a time, for
its best text target, `text/html` and `image/png`. `TEXT` alone is skipped,
since it may come as `COMPOUND_TEXT`, and HTML that starts with a UTF-16 byte
order mark, as Firefox has served it, is converted to UTF-8. Anything bigger
than one request, `maxReq`, 256 KiB without BIG-REQUESTS, goes by INCR in
both directions: outbound the agent selects property changes on the
requestor's window and writes a chunk at each delete, up to 16 transfers at
once, dropping one whose requestor has been silent for 30 s. 16 MiB went
in from `xclip` in 181 ms and out to it in 211 ms. On Wayland the portal
does the formats: `SetSelection` lists the MIME types the content has,
`SelectionTransfer` names the one a program wants, and a copy on the desktop
is read type by type through `SelectionRead`, each pipe with an idle
timeout of 2 s rather than one for the whole transfer. GNOME 46, GNOME 49
and KDE Plasma 6.7 pass an image bigger than a pipe holds both ways.

A paste in the page is split by `splitPaste`: one PNG file and the text
beside it is clipboard content, which is how a browser pastes an image
copied from a page or a screenshot tool, and anything else with files in it
is uploads, its text dropped, because that text is the files' names. A PNG
copied as a file in a file manager therefore pastes as an image. Firefox
keeps nothing of a script's `DataTransfer` in a `ClipboardEvent` it
constructs, so its e2e run hands the page the `DataTransfer` through
`robin.paste` while Chrome's goes through a real paste event.

### Reproducible release binaries

The CLI names the agent on the host after the SHA-256 of its content, and
a release is a set of `dist/` binaries with published sums. Neither means
anything unless a second builder can get the same bytes from the tagged
commit, and until 2026-10-24 none could: two checkouts of one commit gave
agents that differed in size and in every hash. `make repro` (built twice,
from the checkout and from a copy at another path with an empty Go build
cache, then `cmp`) showed two leaks in its `strings` diff; the rest below
came from reading what the build records and asking what a builder on
another machine, or without a clone, would get.

The biggest was not a path at all. Go hands cgo's C files to the compiler
with whatever `CGO_CFLAGS` says, and ours said only `-I<prefix>/include`.
With no `-O`, `zig cc` compiles a debug build, and a zig debug build has
UBSan on, with a runtime that reports rather than traps. That put 280 KB of
sanitizer into every agent along with UBSan's source locations, which
carry the absolute path of each cgo file (clang does not run those through
`-ffile-prefix-map`, so Go's own `-ffile-prefix-map=<checkout>=/_/<module>`
never touched them) and the type descriptors that name
`cgo-gcc-prolog`. `CGO_CFLAGS` now starts with `-Og -fno-sanitize=undefined`.
The release agent runs no sanitizer, which is a behaviour change as much
as a byte one. `-Og` rather than `-O2`: zig keeps its stack protector only
in a debug build, and at `-O1` and above no `-fstack-protector` flag
brings the canaries back (measured with zig 0.17 on a C function with a
64-byte buffer). Trap-only UBSan (`-fsanitize=undefined
-fsanitize-trap=undefined`) also keeps them, with no runtime and no paths,
but turns any undefined behaviour in the glue or in `runtime/cgo` into a
crash, and is not adopted until `make check` has run with it. The glue is
a few calls per frame, so its optimization level is not measurable.

The Go build ID differed next. The linker derives it from the action
hashes of every input, and the cgo package's hash takes in `CC`, `CGO_CFLAGS`
and `CGO_LDFLAGS` as spelled, with the absolute toolchain prefix inside.
Making those path-free would have meant a compiler wrapper on `PATH` that
adds the include and library directories itself, so that the build cache
no longer notices a toolchain change. `-ldflags=-buildid=` instead leaves
the note empty, as Debian and Nix build Go, and every build uses it so the
recipes stay uniform. `-buildvcs=false` goes with it: the stamped
`vcs.revision`, `vcs.time` and `vcs.modified` make a tarball or a shallow
clone differ from a full one, and `VERSION` from `git describe` already
carries the commit into every binary through `-X main.version`.

The static libraries were a quieter case. `zig ar` is `llvm-ar`, which
writes deterministic archives by default (every member 0/0 at epoch, in the
order the makefiles list them), but each object inside carried DWARF with
`DW_AT_comp_dir` under `.cache/toolchain/build`, the realpath of zig's own
libc headers, and openh264 forces `-g` even in its release configuration.
The final link strips all of it with `-s`, so none of this reached an
agent, but a library that differs between caches is a library nobody can
check either. Both builds now compile with `-g0 -fno-sanitize=undefined`
and `-ffile-prefix-map` for the cache as spelled and as resolved
(`FIXED_CFLAGS` and `prefix_maps` in `toolchain/lib.sh`). openh264 takes them through
`CFLAGS_OPT`, the one variable its Makefile appends as is; setting `CFLAGS`
on its command line would have replaced every flag the Makefile adds,
`-DNDEBUG` included. Two caches at different paths, built from the shared
downloads, gave identical `libopenh264.a` and `libopus.a` with no path
string left in either. The first `REPRO_TOOLCHAIN=1` run then failed on
the agents by 32 bytes, because the checkout's cache still held libraries
from before the flag change while the copy had built fresh ones, and a
cache that outlives its flags is a quiet way for one builder to differ
from the rest. Each library now lives in a directory named by a key over
every input of its build: the source tarball's hash, the flags, the zig
and nasm versions, and the text of `lib.sh` and of the script itself. A
changed input is a new directory, so no checkout links a cache that
outlived its flags, worktrees sharing `.cache` with different scripts
stop rebuilding over each other, and a library is installed by renaming a
finished build into place, so a concurrent `go build` never links one
half written. The prefix maps stay out of the key: they spell the cache's
path, which every checkout sharing a cache through a link spells
differently, and the output does not depend on them. The builder's
`CFLAGS`, `LDFLAGS`, `MAKEFLAGS` and the like are unset before a library
is configured: openh264's Makefile appends to `CFLAGS`, opus's configure
reads `CPPFLAGS` and `LDFLAGS`, and neither showed in the key.

The gzip of each agent moved from the host's `gzip -9 -n` to
`internal/tools/gz`, Go's `compress/gzip` with a fixed header. `-n`
already dropped the name and the time, but GNU gzip, Apple's gzip and pigz
do not emit the same deflate stream for the same input, and the CLI embeds
the `.gz` byte for byte. `go.mod` now names the exact Go toolchain for the
same reason: the compiler's version is in the binary and its output
changes between patch releases.

The Go side is pinned rather than documented. `go.mod`'s `toolchain` line
is a floor under the default `GOTOOLCHAIN=auto`: a builder with a newer Go
builds with it, and a scratch module declaring `go1.27.1` built by a local
go1.27.3 produced a binary stamped go1.27.3. The Makefile therefore
exports the line as `GOTOOLCHAIN`, which beats both `go.mod` and the
user's saved env, and `go-version` fails the build if `go env GOVERSION`
still disagrees. `GOENV=off`, `GOFLAGS=-mod=readonly`, an empty
`GOEXPERIMENT`, `GOAMD64=v1` and `GOARM64=v8.0` go with it, since
`-race`, `-tags`, an experiment or a microarchitecture level would
otherwise reach the binary from the builder's shell or `go env -w`.
`VERSION` comes from `git describe --match 'v[0-9]*' --abbrev=12`, so a
fork's own tags and the repository's object count no longer change it,
and `make dist` builds only a tagged commit or an explicit `VERSION`.

What stays an input is the build host's architecture, since each host
runs its own zig binary. Linux x86_64 to Linux x86_64 is what `make repro`
has shown; macOS to Linux is expected to agree and has not been tried.
`make repro` builds the copy in a temp directory outside any repository,
with its own home, temp directory, Go build cache and zig, so a leak
through `HOME`, `TMPDIR`, the git state or zig's install path shows as a
difference; the first version of the check shared all of those with the
copy and could not have seen one. `REPRO_TOOLCHAIN=1` extends it to the C
libraries from source, `REPRO_SELFTEST=1` proves the comparison notices a
difference at all, and `make test-agents` (part of `make check` and `make
repro`; `make test` skips the test without agents, so a fresh clone's
unit tests need no toolchain) checks the agents this tree builds for the
checkout, the toolchain cache, the home directory, Go's directories, the pinned Go
version and the exact set of build settings, so a leak that comes back is
caught without the double build. Nothing checks it on every merge yet:
the repository has no CI.

### Without ControlMaster: an ssh per command and per forward

Windows' OpenSSH has no ControlMaster, so `sshmux` has a mode without one
(`Options.NoMux`, `--ssh-mux=false`), picked by default on Windows. Each
command and SFTP session is an ssh of its own that reads the user's config
and arguments as the master would, with `ControlMaster=no` and
`ControlPath=none` so a master in that config neither starts nor gets
ridden, and `ClearAllForwardings=yes` so its forwards do not come up once
per command. A forward is an `ssh -N -L <local>:<remote>` that lives
until `Close`, and the tunnel splices into it as it does into the master's
socket. `<local>` is a unix socket in a 0700 directory, as over a master,
so no other user of the client machine can reach it or take its place.
Windows' OpenSSH forwards no unix sockets, so there it is a loopback port
the CLI picks and releases for ssh, and ssh binds it only after logging
in, seconds or, with a password typed, minutes later. Another user can take
the port meanwhile, and it shows in ssh's command line. So the CLI does not
take a connection that the port accepts as proof: it waits until the
system's table of listeners (`GetExtendedTcpTable`) names ssh's process as
the one on that port, and fails the forward when another process holds it.
A port that accepts while the table shows nobody there fails it too, so a
table read wrong ends in an error rather than a wait with no end.
Nothing here runs Windows; the tests take that path on Linux, where
`/proc/net/tcp` and the process's open sockets answer the same question.
Once a forward's ssh has exited, its `Dial` refuses rather than connect to
whoever listens there next. That ssh cannot clear the config's
forwards: `ssh -G -o ClearAllForwardings=yes -L ...` shows the option drops
the command line's `-L` along with them, so a `LocalForward` for the host
comes up with the tunnel and, under `ExitOnForwardFailure`, takes it down if
it fails. The tests count one login per command, four for four commands,
none for `Dial`, which no longer has anything to wait for.

There is no master to watch either, so `Done` closes only with `Close`, and
a dropped network is noticed through the agent connection's idle timeout.
A kept session's redial and its end command after Ctrl+C, which kills
every ssh in the terminal's process group, log in afresh as every command
does. The Linux tests and the `nomux` e2e scenario force the mode, which
is how it is tested at all: nothing here runs Windows.

### Text: its own control, typed in order off the read loop

What an input method commits, or a soft keyboard types without naming a
key, goes as a `text` control rather than as made-up keys: a composition
has no keys to send, and the agent can type any character on X11 already.
Typing text can wait on the desktop (below), and control handlers run on
Pion's read loop, where waiting stalls every data channel. So text goes to
a goroutine of the session's, and input that arrives while it types (keys,
pointer, wheel, release and clipboard, a rich one sent as a bulk transfer
included) queues behind it, or Enter after a commit would land first. With nothing to type, input still applies at once
on the read loop, as before. The queue holds 256 messages. Past that, runs
of pointer moves merge into their last, then input is dropped, and a
release of every key and button, which the queue never refuses, follows
whatever was dropped: a 4096-byte CJK commit types for about 4 s on X11,
long enough for pointer moves alone to fill it, and a key-up lost there
would leave the key held. A browser that leaves has what it queued
dropped and its typing stopped, and its peer's handler applies nothing it
sends after that: the data channel can deliver a last message while the
peer closes, and text in it would start typing again.

On X11 each character the keymap lacks borrows one of 16 spare keycodes.
A commit of more than 16 such characters reuses keycodes faster than
clients read their key events, and a client reads the keymap when it gets
to the event: typing 64 distinct CJK characters into xterm in Xvfb got the
reused ones wrong in 5 runs of 5 with no wait and 7 of 10 with a 5 ms
wait; 15 ms was right 10 times of 10. `Type` waits until a spare keycode's
last press is 50 ms old before binding it again, which typed those 75
characters in about 205 ms.

### Wayland text: ASCII as keysyms, the rest pasted

The compositor drops a keysym its layout lacks, a Wayland client cannot add
one, and the portal does not say what the layout has. ASCII is on every
Latin layout, so text that is all ASCII types as keysyms; any other text is
pasted whole: offered as the clipboard, then Ctrl+V, then 100 ms for the
program to read it before the next input. A second after the last paste
the clipboard gets back what the user had there, unless something was
copied meanwhile: what a program copied or the page set, as last seen and
in each format the rich clipboard reads (text, HTML and PNG), or before
either, the text that was there when the session began. The portal reports
only changes, so the first paste reads that text, and GNOME 46, GNOME 49
and KDE Plasma 6.7 all answer the read. A paste never counts as the
user's clipboard, or the next paste would put the last one back. Where
there is nothing to put back (no format the agent reads, or nothing),
empty text is offered in place of the pasted text rather than leave it
there, and a clipboard manager that read the paste's offer reads the empty
one too. Putting it back as soon as
the offer was read failed on GNOME 46: mutter's clipboard manager reads
every new offer at once, so the probe window then pasted the old text, the
next paste read nothing, and xdg-desktop-portal crashed, leaving the remote
desktop session behind. The timed version passes on GNOME 46, GNOME 49 and
KDE Plasma 6.7, though GNOME 46's xdg-desktop-portal still died twice when
the probe read the clipboard at the moment the put-back offered anew; the
attach test reads from half a second after a put-back is due. Ctrl+V does
not paste in a terminal, an image copied before the session cannot be put
back, and a clipboard manager keeps the pasted text in its history; all
three are known limits.

### The page types through a hidden text box

Input methods and soft keyboards type only into something editable that
has the focus, so the page keeps the focus in a hidden text box. Keys the
browser names go out as keys, as before, and are cancelled so the box
stays empty and nothing types twice. Keys an input method owns stay with
the box: `isComposing`, Chrome's `Process` with keyCode 229 (its first key
of a composition comes before `isComposing` is set), `Dead`, and Android's
`Unidentified`. A composition is shown under the status tab while it lasts
and sent as text when it ends; other edits arrive as `beforeinput`, where
typed text goes as text and line breaks and deletions as Enter, Backspace
and Delete. The box moves to wherever the pointer went down, so an input
method opens its candidates there. On a touch-first device it takes the
focus only from the keyboard button, since focusing it raises the soft
keyboard.

### Touch: a finger is the mouse, and the zoom stays local

One finger taps to click, holds 500 ms to right click and drags with the
button down; two fingers scroll, the content following them, or pinch.
Which of the two is settled by what they do first, once both have moved:
the spread changing by 24 px pinches, the centre moving 8 px scrolls.
Chrome reports each finger of a touch event as a pointermove of its own,
and one finger of a pinch moves the centre on its own as far as a scroll
does; judged after each finger, the touch scenario's pinch in, 18 px a side
per step, scrolled instead. A pinch zooms and pans the
picture in the page with a CSS transform and never resizes the desktop,
which keeps the session the same for everyone. Mapping a point to the
remote needs no change for it: `getBoundingClientRect` reports the
transformed box, which a unit test checks against the zoom's own sums, and
the `touch` scenario checks on the host with a zoomed tap. With no pointer
to hover, the status tab stays out on touch-first devices, and touch screens
get a button that raises the soft keyboard.

### Windows and Android clients; iOS is not one

Windows opens the page with `rundll32 url.dll,FileProtocolHandler`, which
hands the URL to the default browser as it is, where `cmd /c start` would
parse it as a command line first. Termux on Android runs the linux/arm64
CLI unchanged and opens the page with `termux-open-url` when it is on
`PATH`. `make dist` builds both Windows CLIs, and `GOOS=windows go vet` and
the test binaries build. Signals needed no code: Ctrl+C reaches every
process on the console, the ssh children included, as it reaches the
terminal's process group on Unix, and Go delivers a closed console window
as SIGTERM, which the CLI already ends on. A forward's ssh is asked to exit
with SIGTERM, which lets it end a `ProxyCommand` it started, and killed on
Windows, which cannot send one. The local paths that assume Unix (the
control socket's directory, `-F /dev/null` for sessions over the master)
belong to the ControlMaster mode only; remote paths were already built with
`path`, not `filepath`. iOS cannot be a client: the page is served on the
client's loopback, so the CLI runs on the same device as the browser, and
iOS gives a user nothing that runs it.

## Verification parked

Two checks were parked by decision on 2026-10-18. Both needed hardware the
test workstation cannot offer without changes the owner chose to leave for
later; the second has since been closed by hand.

- NVENC on NVIDIA hardware. The workstation has an NVIDIA GPU and the 535
  driver's userspace, but no kernel module for its running kernel, and
  installing one needs root on a Puppet-managed machine. NVENC is tested
  against stand-in tools only, and `--encoder auto` prefers VA-API over it
  for that reason. To close it: install `linux-modules-nvidia-535-$(uname -r)`,
  load `nvidia`, run `ROBIN_HWENC_REQUIRE=nvenc make test` and one 4K session
  with `--encoder nvenc`.
- Safari, closed 2026-10-24. `dist/robin-darwin-arm64 gui --attach auto`
  from an Apple silicon Mac reached this workstation over Tailscale on
  direct UDP, with picture, input and sound in Chrome; the same session with
  `--no-browser`, opened in Safari, showed the picture and took typing. The
  e2e suite still cannot check Safari's picture, because Playwright's Linux
  WebKit renders no remote WebRTC video, so a change to the page's video or
  input path needs this hand check again.
