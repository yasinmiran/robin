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
| openh264 via cgo | BSD-2. Emits Constrained Baseline, which every browser decodes in WebRTC. Has a real-time screen-content mode and runtime bitrate and IDR control, which is exactly what congestion control needs. **Chosen.** |
| GStreamer on the host | Needs packages on the host. Rejected. |
| ffmpeg subprocess | Needs ffmpeg on the host, or shipping a ~80 MB static build. Rejected. |

The C/C++ toolchain is a pinned `zig cc` fetched by the build, targeting
`x86_64-linux-musl` and `aarch64-linux-musl`. The agent is fully static and
cross-compiles from Linux or macOS with no containers or root.

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
the install command. Attach mode (Phase 2) needs none of it.

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
