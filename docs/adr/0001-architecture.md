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
a slow core makes worse. Or the session may exceed 2560x1440 and ffmpeg
drives the GPU: at 2160p openh264 spends up to 28 ms of CPU a frame on busy
content against 12 through ffmpeg. GStreamer's route does not count for
large sessions, since it spends 31 to 34 ms on a 2160p frame, a whole frame
interval at 30 fps, for 21 ms of CPU. VA-API comes before NVENC, which has
not run on NVIDIA hardware. A 1080p or 1440p session on a host with more
cores stays in software, where the GPU would save a few milliseconds of CPU
a frame and add as many of latency: at 1440p openh264 needs 5.5 to 16.8 ms
of CPU a frame, GStreamer's vaapih264enc 7.6 to 7.8 ms between the agent
and the tool for 10 to 11 ms a call (`TestCompare`, same machine). The
threshold sat at 1920x1080 until 2560x1440 became the default size, below. Because a pick is a guess from the probe, the CLI
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

### Congestion control: GCC with a bounded NACK responder

The browser sends transport-wide congestion control feedback; Pion's GCC turns
it into a target bitrate that the encoder follows. Two Pion defaults had to go,
both found by streaming through a shaped virtual link (`internal/agent/peer`,
`TestCongestionControlAdaptsToLink`). The default leaky-bucket pacer queues
without bound, so whatever was sent while the target fell drained for seconds
and the delay that added pinned the target at its floor. And the default NACK
responder resends anything in a 1024-packet buffer as often as it is asked:
after a startup overshoot the lost packets were resent, lost again and NACKed
again until retransmissions filled the link, and the loss estimator reported
93% loss with nothing being lost. The agent sent unpaced (the encoder holds
the rate) and answers NACKs only for packets younger than a second, at most
twice each, within a quarter of the target bitrate. A 4x overshooting start
and a link that drops mid-session now both settle within about 10% of the
link rate.

Unpaced did not last: keyframes sent at once overflow a shallow bottleneck
faster than the bounded responder repairs it. The agent now paces bursts
with a pacer of its own whose wait is bounded, and the responder keeps a
second of history and asks for a keyframe when it cannot repair; see
[Video in motion](#video-in-motion-measured-against-a-stand-in-for-chrome).
With the pacer the two scenarios above settle at 1003 and 1058 kbit/s on a
1000 kbit/s link.

### Zero playout delay on video

Every video packet carries the playout-delay header extension with a minimum
and maximum of zero, so Chrome decodes and shows a frame as soon as it is
complete instead of holding it for its jitter estimate. On a still screen
the agent used to send nothing until Chrome, after 3 s without a frame,
asked for a keyframe (it now sends the picture again every 500 ms; see
[Video in motion](#video-in-motion-measured-against-a-stand-in-for-chrome)).
The jitter estimate grew on that trickle of large frames until it held new
frames back for most of a second, so what you type after a pause showed
late. The e2e measures it after 20 s of a still screen. In the
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
remote media tracks to `Handlers.Track`, where the microphone and camera
take them. Protocol version 2 marks the change, so a CLI and an agent
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
  not have caught that. The refusal comes before the upload, which used to
  run first and unlink the kept daemon's binary even though the CLI then
  left the session alone, and the upload spares every agent a kept
  session runs from: the desktop's `robin-send` is a symlink to it, and
  dangled after a `--new` beside the session.
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

### Video in motion: measured against a stand-in for Chrome

The first real session, an Apple silicon Mac over Tailscale on direct UDP
to a 2560x1440 monitor shown through the default `--max-size` of
1920x1080, looked pixelated at times, and its log showed the browser asking
for a keyframe about every 3 s. Every change below was judged by the
numbers of one harness, built before anything changed.

**The harness.** `internal/agent/vqtest` plays a clip through the real
stream loop, the agent's peer and its encoder, over the shaped virtual link
the peer tests use, to a stand-in for Chrome's video receiver, and scores
every picture it shows. The clip is a 2560x1440 screen of an editor with
13 px code, 14 px prose and a 12 px terminal, rendered by Chrome in the e2e
client image (`testdata/render.sh`), cropped about its centre for smaller
sizes; over it run segments of stillness, typing (a glyph every third
frame), scrolling (9 px a frame), a window drag (12 px right and 6 px up a
frame) and video (noise over a moving gradient on a third of the screen).
Each access unit carries its clip frame in an SEI message, so each decoded
picture is scored against its own source: PSNR and SSIM on luma, and the
SSIM of the worst 64x64 tile, which shows blocky text in one window that
the mean of a mostly flat screen hides. A picture encoded smaller than the
screen is stretched back bilinearly first, as the browser does. The
receiver follows libwebrtc: it NACKs a gap at once and again every 100 ms,
the round trip a receive-only peer assumes since no report block gives it
one, ten times at most; it holds every frame behind a gap until the gap
fills or a keyframe starts afresh (`H26xPacketBuffer`); and it asks for a
keyframe after 3 s without a decodable frame while packets still arrive,
or after 200 ms while one is required. One detail mattered more than any
other: Pion's SRTP drops packets more than 64 behind the newest as replays,
which threw most retransmissions away and made the first runs look far
worse than Chrome, whose window is 1024. `TestVideoQuality` runs a 15 s
1080p clip in `make test`; `TestVideoQualityMatrix` (`ROBIN_VQ=1`) runs a
24 s clip (6 s still, 3 s each of typing, scrolling, a drag and video, 6 s
still) at three sizes over three links, and names variants that put back
each policy below:

| link | rate | buffer | round trip | random loss |
|---|---|---|---|---|
| fast | 50 Mbit/s | 50 ms | 30 ms, 1 ms jitter | 0.1% |
| lossy | 20 Mbit/s | 50 ms | 40 ms, 2 ms jitter | 1% |
| tight | 4 Mbit/s | 100 ms | 60 ms | none |

Before any change, per size and link: the pictures shown of the frames
sent, the keyframes the browser asked for (on a still screen, or stalled
behind a loss), and the mean PSNR of what it showed, at the screen's size:

| | fast | lossy | tight |
|---|---|---|---|
| 1080p | 261/261, 3+0, 42.4 dB | 273/274, 1+1, 42.7 dB | 0/115, 0+7 |
| 1440p scaled to 1080p | 297/297, 2+0, 26.3 dB | 293/297, 1+2, 26.3 dB | 283/285, 2+1, 25.8 dB |
| 1440p | 254/255, 2+1, 41.1 dB | 164/238, 2+2, 37.1 dB | 0/25, 0+7 |

And after them, from one run each on this shared machine (between runs the
tight link's lost packets vary by half again, and the congestion target
by more, so read differences smaller than that as noise):

| | fast | lossy | tight |
|---|---|---|---|
| 1080p | 240/240, 0+0, 42.7 dB | 70/72, 0+0, 41.7 dB | 250/274, 0+0, 42.0 dB |
| 1440p scaled to 1080p | 250/250, 0+0, 26.1 dB | 320/320, 0+0, 26.4 dB | 279/312, 0+0, 26.3 dB |
| 1440p | 312/312, 0+0, 48.5 dB | 298/299, 0+0, 44.9 dB | 231/250, 0+0, 40.1 dB |

The 1080p lossy run is one where congestion control collapsed, last item
below.

**Why the browser asked for keyframes.** Two causes, and the first is the
3 s of the report. Chrome asks after 3 s without a frame, and a still
screen sent none, so it asked every 3 s for as long as the screen stayed
still. Each answer was a fresh IDR of 150 KB at 1080p, at whatever QP
rate control gave it (27 to 34), so a still screen sat at 39 to 42 dB and
was replaced by a new picture of the same thing every 3 s. The stream now
encodes the unchanged picture again every 500 ms. openh264 spends what
the budget has spare on it: at 2 Mbit/s one 62 KB P frame took a still
IDR from 37.5 to 41.5 dB, with background detection on or off. In the
matrix at 1080p over the fast link, without and with: the browser asked
twice and 5 keyframes went out, against none and one; the first still
stretch ended at 42.7 dB and SSIM 0.995, against 52.0 dB and 0.9997;
typing showed at 40.2 dB against 52.4. Settled, a still screen costs
about 30 kbit/s. 250 ms reached the same quality for twice the frames, and
1 s ended the stretch at 44 dB. Calibrated in Chrome, the e2e latency test
now counts what Chrome reports over 19 s of a still screen (direct
scenario): before, 6 keyframe requests, 6 keyframes decoded and 18.2 s
counted as freezes, which is the stand-in's cadence exactly; now none,
none and 3.2 s, with 37 refreshed frames decoded. The keystrokes after it
showed in 15 to 37 ms, against 5 to 27 ms before, one run each and well
inside the test's 400 ms.

The second cause is loss the responder cannot repair. Keyframes went out
in one burst, 150 KB at 1080p and up to 300 KB at 1440p, and a bottleneck
that buffers less than that drops the rest together. With a NACK budget of
a quarter of the target refilled every 100 ms, a burst of 36 packets is
repaired a few at a time, Chrome gives up on the others after ten asks,
holds every later frame behind them, and asks for a keyframe 3 s later,
which overflows the buffer again: at 1080p over the tight link the browser
showed no picture after the first in 24 s. On that run the responder
refused 1357 NACKs for its budget and 423 for age against 189 it answered.
Three changes:

- A pacer of the agent's own (`internal/agent/peer/pacer.go`), in the
  place of Pion's: a packet goes out at once while it fits 20 ms of 1.5
  times the target, and queues behind the others beyond that, but never
  for longer than 500 ms, so a keyframe spreads over about 200 ms and
  steady frames are not delayed. Paced, at 1080p, packets never repaired
  went from 41 to 0 on the fast link, 210 to 0 on the lossy one and 522 to
  145 on the tight one, where pictures shown went from 6 to 208. The bound
  was measured too: over the tight link 250 ms left 243 packets
  unrepaired and keyframes still overflowing, 1 s held scrolling back by
  0.76 s on average, and 500 ms left 8 with the longest freeze 0.33 s.
  This replaces the decision to send unpaced: what Pion's pacer did wrong
  was the unbounded queue, and this one has a bound. Opus packets share
  the queue, so a keyframe can hold sound back by as much; nobody has
  listened for it yet.
- The NACK history held 512 packets, half a second at 10 Mbit/s, while
  Chrome asks for a packet for about a second; it holds 4096.
- When a NACK names a packet the responder no longer holds or that is
  older than a second, the browser is stuck until a keyframe and would ask
  for one only seconds later, so the responder asks the stream for one at
  once, at most once a second. Over the tight link at 1080p that took the
  browser's own requests from 3 to 0 and the longest freeze from 3 s to
  0.76 s. With a gap of 300 ms instead of a second, 1440p over the tight
  link went into a storm: each keyframe was lost in turn, 74 of them.

A NACK budget refilled over a second instead of 100 ms changed nothing
measurable on the tight link (145 and 234 packets unrepaired in two runs),
so it stays. What is left over the tight link comes from congestion
control: Pion's GCC sets the target above the link (4.3 to 5 Mbit/s on 4)
and the encoder fills it during video.

**Keyframes that do not wreck the picture.** openh264 does not size an
IDR to its target: it clamps the IDR's QP to a range picked by bits per
pixel (at most 36 to 40 at 1080p, `RcCalculateIdrQp`), so
`iIdrBitrateRatio` cannot make one smaller, and a 1080p IDR of this
screen is 130 KB even at 1 Mbit/s. The pacer bounds what that does to the
link, and the refresh above brings the picture back after it: an IDR at
QP 34 is followed by P frames of the same picture that raise it 4 dB.
Long-term reference recovery was the other way and does not work with a
browser: openh264 needs the decoder to report which reference it holds,
which a PLI does not carry, and Chrome's H.264 receiver holds every frame
after a gap until a keyframe, since it tracks continuity by sequence
number and the agent sends no dependency descriptor. openh264 has no
intra refresh. Neither is used.

**Rate control for a desktop.** openh264 ran with QP up to 51, so a dip in
the target turned text to blocks. With frame skipping on, a lower ceiling
makes rate control skip frames instead, which is the right trade for a
desktop. At 1080p over the tight link, the worst tile while scrolling and
while dragging, against the frame rate of each:

| ceiling | scroll tile | scroll fps | drag tile | drag fps | video fps |
|---|---|---|---|---|---|
| 51 | 0.398 | 21.7 | 0.674 | 28.7 | 15.0 |
| 44 | 0.791 | 29.0 | 0.971 | 28.3 | 10.7 |
| 40 | 0.771 | 26.3 | 0.966 | 28.3 | 15.3 |
| 36 | 0.905 | 23.0 | 0.916 | 7.0 | 17.7 |

At 36 rate control could not keep frames small enough and the drag went to
7 fps with 7 keyframes. 40 is the default: text stays legible and frame
rate pays only where the link cannot carry the content.

**Defaults.** The 1440p monitor of the report, scaled to 1920x1080 and
stretched back by the browser, scored 26.3 dB against the screen whatever
the link; shown at its own size the same clip scored 41 to 48 dB on the
fast link for about the same bitrate (1709 kbit/s against 1793 before the
changes). Scaling loses the small text, and no encoder setting gets it
back. openh264 at 1440p needs 5.5 to 16.8 ms of CPU a frame (`TestCompare`,
`internal/agent/hwenc`), up to half a core at 30 fps, so `--max-size`
defaults to 2560x1440 on hosts with more than four cores and stays at
1920x1080 on hosts with four or fewer, or that do not report their cores.
The display still follows the browser window below that size, and a
desktop that fits is not scaled at all. `auto` keeps such a session in
software, as the hardware section says. The bitrate ceiling stays at
10 Mbit/s: at 1440p on the fast link only video reached it (9.4 Mbit/s at
QP 31 and 42 dB), while scrolling text took 5.4 Mbit/s at QP 19 and 52 dB.
A 20 Mbit/s ceiling measured the same (48.8 against 48.2 dB over the clip,
41.8 against 41.7 during video): the target climbs about 8% a second and
had not reached 10 Mbit/s when the video ended. High profile on the hardware path stays out. Encoded with this
iGPU's `vaapih264enc` from the same frames at the same VBR settings, High
with CABAC and 8x8 transforms came to 764 KB against 768 KB for
Constrained Baseline at 2 Mbit/s and 1827 against 1933 KB at 5 Mbit/s, at
most 5%, and openh264's decoder held those streams' pictures back, so
their quality could not be scored. Using it would take a second codec in
the agent's SDP and in the page's codec preference, and Firefox decodes
WebRTC H.264 through OpenH264, which takes Constrained Baseline only.

**What is left.** Pion's GCC lowers its target to 0.85 times the rate it
received when it sees overuse, and on a still screen that rate is a few
kbit/s. In some runs a still stretch with a little jitter took the target
from about 3 Mbit/s to 1.2, or on the lossy link to 0.3, so the first
seconds of motion after a pause came at that rate until the target climbed
back. The 1080p lossy run above is one. Pion has no notion of being
limited by the application, and the agent does not pad; it is in the
README's Known limits.

### Microphone and camera: the browser's devices as the host's own

`--mic` and `--camera` put the browser's microphone and camera where the
host's programs look for them. Both are asked for twice, decisions taken
with the owner: the CLI has to offer them, and even then each stays off
until the user switches it on in the page, which is when the browser asks
for permission. Nothing is sent from a switched-off device.

**Negotiated once.** The browser offers and the agent answers, as for
everything else. With the microphone on offer the page makes its sound line
sendrecv from the start, and the camera gets a sendonly video line of its
own, after the screen's and before the data channel, so Pion pairs its own
video track with the first and leaves the camera's to receive. A switch is
then `replaceTrack` on a sender that already exists: no offer, no answer, no
moment without picture. The e2e tests compare the page's local description
before and after switching each device on and off, and it never changes.
Without the flags the page offers what it always has. Switching off also
stops the track, so the browser's own camera light goes out. The senders
belong to one peer connection, so the one a kept session's reconnect
builds comes with switches of its own, off, and the old ones are turned
off with their connection.

**What the agent owes a sender.** Pion's defaults for receiving were never
in the registry, as the agent only sent. With a device on offer it adds two
receive-side interceptors: transport-wide feedback, without which Chrome's
congestion controller has no delay signal for the camera, and a NACK
generator for the camera's lost packets. Both act on remote streams only,
so the sending side keeps its bounded NACK responder and its own pacer. The
camera asks for a keyframe with a PLI when a frame cannot be decoded, at
most every 500 ms, and the page caps the camera at 1.5 Mbit/s, so it leaves
most of a thin link to the desktop. A Pion browser in `internal/agent/peer`
sees a NACK for the one packet it skipped, transport-wide feedback and the
PLI.

**The microphone.** Opus from the browser is decoded with libopus to 48 kHz
mono. A packet lost just before one that arrives is recovered from that
packet's in-band FEC, and the loss before it concealed, up to 100 ms of
made-up sound, so a long gap is a jump rather than a growing delay.
Recovery beats concealment by a wide margin on voiced sound at 24 kbit/s
with 20% planned loss: over 9 dropped frames the error energy against a
lossless decode was 3.3e9 recovered and 6.7e10 concealed. A sound queue of
200 ms sits between the track and the sound server, and drops rather than
waits. Where it goes depends on the server:

| Desktop | Source | Gone when |
|---|---|---|
| spawned | a null sink and `module-remap-source`, loaded by the private pulseaudio's startup script, the source made the default | the server stops |
| attached, PulseAudio | the same two modules, loaded over the protocol with the agent's pid in their names; a playback stream into the sink | the agent unloads them, or the next agent sweeps those of dead pids |
| attached, PipeWire | a `gst-launch-1.0` child, `pipewiresink mode=provide` as an `Audio/Source` | the child exits, killed by Pdeathsig if need be |

The private server takes no modules once it runs
(`--disallow-module-loading`), so the agent tells it at startup. A remap
source rather than the null sink's monitor, because programs hide monitors
from their lists of microphones. Which kind of server is attached comes
from its own name over the protocol, so a host running PipeWire for
screen sharing beside PulseAudio for sound, as Ubuntu 22.04 does, gets the
PulseAudio route that its programs record from.

The PipeWire route took measuring. A source that a stream provides drives
nothing, and pipewire-pulse links its recorders without a driver: a 4 s
recording with `parec` got 0.18 s of a tone on PipeWire 1.0 and 1.4 alike,
while `pw-record`, which brings its own, got all of it.
`priority.driver`, `node.driver`, `node.want-driver` and
`node.always-process` changed nothing. `node.group=pipewire.dummy` puts the
node with PipeWire's dummy driver, and then `parec` got 4.00 s of 4, at
438 Hz for a 440 Hz tone. A leaky queue of 100 ms in the pipeline drops
sound nobody records, rather than letting it back up into the agent. The
node appears only once data fixes its format, so the agent feeds it
silence while the browser sends nothing, and waits for it to show up on
pipewire-pulse before it reports the microphone ready.

On a plain PulseAudio whose user chose no default source, the server picks
the best one itself, and a remap source outranks monitors, so a host with
no microphone of its own takes Robin microphone as its default while it is
there. The tests give the stand-in user server a default of its own and
check it stays.

**The camera.** Chrome sends Constrained Baseline H.264, which the page puts
first and the agent's answer allows alone. The agent decodes it with
openh264 rather than leave it to the host's GStreamer, which seldom has an
H.264 decoder installed, and fits each picture into one 1280x720 frame:
the browser changes its resolution with the link and its CPU, and a
program holding the device needs one size. Nearest-neighbour sampling,
since the frame is watched, not analysed. On flat test pictures decoding
took 0.3 ms at 1280x720 and fitting 0.05 ms (a copy), about 1% of a core
at 30 fps; a 640x360 picture took 0.1 and 0.5 ms (a scale), under 2%
(`BenchmarkDecodeAndFit`); a real camera's detail costs more decoding. The frames go to a `gst-launch-1.0` child as
raw I420 through `rawvideoparse`, which needs that fixed size, and from
there to `pipewiresink` as a `Video/Source` with `media.role=Camera`, or to
`v4l2sink` on a v4l2loopback device.

v4l2loopback comes first when the probe finds one the user can write,
labelled Robin by preference: most programs that are not sandboxed open
`/dev/video*` and never see a PipeWire camera. A spawned desktop has no
camera otherwise, as there is no PipeWire session of the user's to put it
in. rootless podman cannot load the module, so that route is tested as far
as the probe and the pipeline it builds.

While the browser sends no picture the camera shows black, at once and
then every second: the node appears only once a frame fixes its format, a
program opening it then gets a picture, and the last face it showed does
not stay on the host's screen after the switch goes off.

On Fedora 43's PipeWire 1.4, `pipewiresink` in provide mode exits with
"all buffers have been removed" when the program reading a video node lets
go; on Ubuntu 24.04's 1.0 it stays. `internal/agent/gstsink` therefore
starts a child that lasted past its start again, giving up only after 20
such children in a row lived under 5 s each. The test-wayland camera test
reads the node twice so that the second read meets the restarted child.

End to end, Chrome's fake devices play a 440 Hz tone that swells twice a
second (a steady one would be taken for noise and suppressed) and a red
picture of 81/90/240 in Y, U and V. A spawned desktop's default source
recorded 440 Hz with 72% of its 20 ms blocks loud, the PipeWire source
437 Hz with 69%, and the PipeWire camera returned 81/90/240 exactly.

### A private session: zero gamma, a silent output, and a keeper

An attached desktop shows the session to whoever sits at it. `--curtain`
and `--silent`, and the matching switches in the page, hide the picture and
the sound from that room while the stream keeps both. Neither is on unless
asked for, and neither exists for a spawned desktop.

| Curtain | Verdict |
|---|---|
| A black window over every monitor | Capture reads the framebuffer, so the stream would be black as well. Rejected. |
| DPMS off | Any input wakes the monitor, the session's own XTest events included, and no Wayland compositor lets a client switch it. Rejected. |
| A gamma ramp of zeros on every CRTC | The CRTC applies gamma on the way to the monitor, after the framebuffer capture reads. RandR does it on X11, mutter's DisplayConfig on GNOME. **Chosen.** |

The first step was to find an X server in podman that keeps ramps at all.
Xvfb 21.1 on Ubuntu 24.04 does: RandR 1.5, one CRTC with a 256-entry ramp,
RRSetCrtcGamma accepted and read back unchanged, so Xorg's dummy driver was
never needed. Its ramps start as all zeros, which looks exactly like a drawn
curtain, so every test and the e2e desktop set a lit ramp first. Nothing in
a container shows a ramp on glass, though, and what real hardware does with
it is in the parked list below.

RandR sends no event when a ramp changes, and night light or a colour
profile rewrites ramps whenever it likes. A drawn curtain reads every
CRTC's ramp once a second, one round trip each, and darkens any that are not
zero; a lit ramp it finds then becomes the one lifting restores, so the
screen comes back with night light's newest colour rather than the one from
before the session. Every CRTC that drives an output must have a ramp a
client can set, or the curtain is refused with the outputs named: one without
(DisplayLink and other USB screens, some virtual GPUs) would stay lit beside
the dark ones while the page said the screen was black. A check that finds
such a screen plugged in, or a ramp it cannot darken again, is reported under
the switch and the rest stay dark. The state machine is one package,
`internal/agent/gamma`, over the three calls that differ: list the CRTCs,
read a ramp, set one. The curtain has an X connection of its own and joins
RandR on it by opcode only. `randr.Init` also rewrites xgb's global event and
error tables, which the session's own connection reads on every event, and
Go maps read and written at once can crash the process, not only fail a race
test. The keeper, a process with one connection, runs the full Init.

On GNOME under Wayland the same calls exist in mutter: GetCrtcGamma and
SetCrtcGamma on `org.gnome.Mutter.DisplayConfig`, present in mutter 46.2 and
49.8, the interface gnome-settings-daemon drove night light through. The
attach decision above rejected mutter's own API because it takes the screen
without asking. Gamma takes nothing, no portal offers it, and a black screen
at the host is itself the notice that something is going on there, including
the portal's sharing indicator, which goes black with the rest. Headless
mutter lists no CRTCs at all, so test-wayland can only show the interface's
shape and the "no screen" answer; the curtain's calls run against a stand-in
on a private bus with mutter's signatures, refusing stale serials as mutter
does. KWin 6.7 has nothing of the kind, and there the agent reports the
curtain unavailable.

| Silent room | Verdict |
|---|---|
| Mute the default output | Its monitor falls silent with it, and the mute is saved in the user's settings. Rejected. |
| Make another output the default | The default is the user's configuration, and changing it is saved too. Rejected. |
| An output of the agent's own, with every playback stream moved into it as it appears | The default never changes, and the session records the new output's monitor. **Chosen.** |

The plan wanted the output owned by a child, `pipewiresrc mode=provide`, so
that it disappears with the agent whatever happens. pipewiresrc has no `mode`
property in PipeWire 1.0.5 (Ubuntu 24.04) or 1.4.11 (Fedora 43); only
pipewiresink does. `pw-record` with `media.class=Audio/Sink` in its
properties, writing to `/dev/null`, makes an output that pipewire-pulse lists
with a monitor, and when the process is killed WirePlumber moves its streams
back to the default output by itself. That is the owner where the host has
`pw-record`. Elsewhere, PulseAudio or a PipeWire without PipeWire's tools
(the Fedora test images), the output is a `module-null-sink` loaded over the
protocol the agent already speaks, which pipewire-pulse accepts too and keeps
after the loading client disconnects. On PulseAudio it gets `norewinds=1`,
for the reason the private server does.

Moves are remembered, and that decided how they are undone. WirePlumber
0.4.17 writes `target=<output>` for the program into
`~/.local/state/wireplumber/restore-stream` on every move, and deletes the
entry when the stream moves to the default output; 0.5.14 wrote nothing.
PulseAudio's `module-stream-restore` behaves like 0.4: after a move back to
the default a program follows the default again
(`TestSilentRoomLeavesPulseAudioNoPreference`). So the undo moves each stream
back to the output it came from, by name, before the silent output goes,
since letting the server rescue the streams would leave the silent output as
each program's remembered choice. The session's own recording follows the
silent output by opening a new record stream there and dropping the old one,
not by moving itself: the PipeWire test caught a moved recording leaving a
WirePlumber entry for robin. Streams moved out again from the host's sound
settings are taken back, as the curtain takes back its ramps.
PipeWire reuses stream indices, so each moved stream is kept with its client
index, process id and, under PipeWire, its object serial, and the undo moves
only a stream that still matches and, while the silent output is there, sits
in it. A stream in another `robin_silent_` output is left alone: two
sessions silencing one desktop otherwise passed a stream between them as
fast as the server would move it. A stream that refuses to move
(`PA_STREAM_DONT_MOVE`) is reported under the switch.

A new stream is moved 81 to 85 ms after paplay starts on PulseAudio 16 and
with `pw-record` on GNOME 46, and 173 ms through the module on PipeWire.
The room's output heard silence throughout (RMS 0 in every run), since paplay
has not been mixed yet by then; e2e measured RMS 0 at the room's speakers
while silent and 4911 after the switch went off.

Both changes have to come undone even when the agent is killed with
SIGKILL, which runs no deferred cleanup. So a keeper, `robin-agent keeper`,
runs as the agent's child in its own process group with SIGTERM as its
parent-death signal and its stdin from the agent. Before each change the
agent sends it a JSON line saying how to undo it (an id, a kind, the data:
the ramps as found, the streams and where they came from), and after undoing
a change itself it sends the id alone. When stdin closes or a signal comes,
the keeper reads for 250 ms more, for a line written just before the agent
died, then undoes what it still holds, newest first, each undo bounded by
5 s. It starts with the first change, so a session that only watches runs
none. That amends the rule that anything the agent starts dies with it: the
keeper outlives the agent by as long as its undo takes.

The keeper writes a byte to its stdout, a pipe of its own to the agent, once
its signal handlers are in, and one for every line it takes, and the agent
makes a change only after that byte. Without it a review found two holes. A
line written into a dying keeper looked held, since the pipe took it; and a
keeper still in Go's runtime setup, before `signal.NotifyContext`, died of
the default SIGTERM when the agent was killed in that window, with the
change it had been handed never undone. A keeper that dies is started again
at once and handed everything held, not only at the next change, since a
curtain over ramps nobody touches makes none; after five failed starts the
switches say so. Close starts one too when the last died holding steps. The
keeper is the agent's own image, run through `/proc/self/exe`: a deploy of
another version removes every other `robin-agent-*` file, and a keeper
started after that from the path would not exist.

The agent here is a kept session's daemon (above), whose child the keeper
is, so an attached desktop kept with `--keep` stays dark and quiet while it
waits for a controller. Ending the session, by Ctrl+C, a dropped link or
the idle timeout, lifts the curtain without locking the screen. A lock was considered and left out:
which locker answers differs per desktop (logind's `Session.Lock`, which
nothing may be listening to, GNOME's and freedesktop's ScreenSaver), and
locking the owner's own desk when a link drops is the owner's call to make.
The README says the screen comes back unlocked.

The switches change on a goroutine of their own. Control messages arrive on
Pion's read loop, and loading a module or waiting for `pw-record`'s output
to appear takes up to a few hundred milliseconds that every data channel
would otherwise wait out.

### A shared client: the token off command lines, strangers off the tunnel

The first security audit read the client as a machine other users log in
to, who can read every command line in `/proc` and connect to every
loopback port. Two of the CLI's surfaces failed that reading.

- The page URL, token in its fragment, went to `xdg-open` or `open` as an
  argument, and a browser that the opener starts keeps its arguments for
  its whole life, so the token sat in `/proc/<pid>/cmdline` for hours, and
  whoever read it could take the page whenever the user's tab was gone. The
  opener now gets `Server.LaunchURL`, `/open/<secret>` with a second 256-bit
  secret, which answers its first fetch with a bodiless 303 to `/#<token>`
  and every later one with the same bare 404 as a wrong secret. A race
  remains: someone who reads the link and fetches it before the browser
  does gets the page. The user's browser then shows Not Found, the server
  calls `LaunchReused`, and the CLI says on stderr that the link was opened
  again. The link does not expire on a timer: the browser fetches it as it
  starts, and a timer would only add a way for a slow cold start to fail.
  Telling a stranger's loopback connection from the
  user's by uid, through `/proc/net/tcp`, would close the race on Linux
  only and not on Android, which hides that file, so it waits. In Chrome
  (`TestBrowserSmoke`) the launch link lands on the page with its token
  once, the page clears the address bar and reconnects on reload as
  before, and a second fetch is a 404.
- Each connection to the tunnel's loopback port became an SSH channel on the
  user's master and a connection to the agent's ICE-TCP listener, for as
  long as the stranger kept it. The tunnel now reads the first RFC 4571
  frame within 5 s, at most 512 bytes, the buffer the agent's mux reads its
  first message into, and splices only a Binding request whose
  MESSAGE-INTEGRITY verifies with the ICE password of the agent's latest
  answer. The bridge hands the tunnel each answer before the page sees it,
  so the browser cannot check with credentials the tunnel lacks. The
  password, not the ufrag alone: the ufrag crosses the network in clear in
  every UDP check, and the password never leaves SSH and the token-guarded
  websocket. Only the latest answer's: after an ICE restart Pion's mux no
  longer routes the old ufrag either. In the e2e `blocked` and `nomux`
  scenarios every TCP session ran through the check; in `restart`, the
  page was back on the tunnel 4151 ms after UDP was dropped, on a machine
  running seven other tracks' suites (about three seconds when measured
  alone, before the check, which costs one frame read per connection).
- The page server closes a keep-alive connection after 30 s idle and a
  refused request at once, and holds at most 32 connections; a browser
  uses at most six for the page's files and one websocket. Another user can
  still fill those 32 and slow the page's loading: loopback TCP carries no
  peer identity to refuse them by.
- Smaller bounds from the same reading: `sshmux.Master.Output` stops a
  command past 1 MiB (the probe's report is at most 1248 bytes on the
  hosts in `internal/probe/testdata`), a verbose agent log without
  newlines is shown in 8 KiB pieces, `protocol.Conn.Send` refuses a line
  over `MaxLineBytes` instead of writing one the peer must take for a broken
  stream (a page frame of 1 MiB of `<` re-encoded to six times that), the
  forward's target from the agent must be a loopback address and port, as
  the agent's listener has, before it reaches `ssh -L`, which splits the
  spec at colons and expands `${VAR}` in it from the client's environment
  (OpenSSH 8.9's `ssh -G -L '/tmp/a:/tmp/x${USER}y'` shows the user's name
  in the target), and host text the CLI prints has bidi
  controls and line separators escaped, the set the agent refuses in a file
  name.
- Ctrl+C during a reconnect that the daemon keeps answering busy no longer
  runs the agent's end command: another `robin gui` holds the session, or
  this CLI's own lost connection does until the daemon's 20 s idle timeout,
  and the two look the same from here. Leaving a desktop the user meant to
  end costs them a `robin end`; ending one another CLI is using costs its
  user the session.

### STUN: Google's by default, none through the tunnel alone

The default `--stun` stays Google's public server. Both ends send it a
Binding request at the start of every peer connection, which tells it the
public addresses of the host and of the client, and its answers are not
authenticated, so whoever can steer DNS or the path to it can hand either
end a wrong reflexive address. That costs at most a candidate pair or the
direct UDP path: the agent is known by the fingerprint pinned over SSH. The
direct path through NAT is the fast one and needs a reflexive address, so
the default keeps it working out of the box; `--stun none` or a server of
one's own is the user's choice, and the README's Security section names
the cost. With `--transport tunnel`, though, the relay drops every UDP
candidate, so a reflexive address helps nobody, and the CLI gives neither
the agent nor the page a STUN server. Switching to none under a ProxyJump
was considered and left: the CLI cannot see a jump in the user's ssh
config, and a host behind one may still be reachable over UDP. Nothing
here is measured: the e2e network reaches no STUN server, and the choice
rests on what ICE does with the answers.

## Verification parked

Two checks were parked by decision on 2026-10-18. Both needed hardware the
test workstation cannot offer without changes the owner chose to leave for
later; the second has since been closed by hand. A third, the curtain on a
real screen, cannot run anywhere but at a screen someone is looking at.

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
- The curtain on real screens, parked by the plan on 2026-10-24. It is
  verified on Xvfb, which keeps ramps but shows nothing, and against a
  stand-in for mutter, since headless mutter has no CRTCs. Unknown: whether a
  hardware cursor plane escapes the ramp and leaves the pointer visible,
  whether night light visibly fights the once-a-second check, and whether
  mutter applies SetCrtcGamma on real outputs at all. Only the owner checks
  it, on his own screen, when he chooses to: from another machine, `robin gui
  --attach auto --curtain workstation`, then look at every monitor, pointer
  included; switch night light on while the curtain is drawn and watch for
  flashes; lift the curtain from the page and check the colour returns;
  draw it again, kill the agent on the workstation with `pkill -KILL -f
  'robin-agent-.* serve'` and check the screens come back within a second.
  The same with `--attach wayland` in a GNOME session, and with `--silent`
  while something plays: the speakers stay quiet, the page plays it, and
  afterwards `pactl get-default-sink` is unchanged and the program is back on
  its output.
