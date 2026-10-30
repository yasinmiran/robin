# Robin

Named for Nico Robin, who sprouts eyes and hands on any surface she can
see. This one sprouts yours on any Linux box you can ssh to.

`robin gui <host>` opens a Linux desktop from `<host>` in your browser.
The only thing the host needs beforehand is SSH access you already have: the
CLI connects with your own `ssh`, copies a small static agent over, starts a
headless X session as you, and streams it back over WebRTC. No signaling
server, no ports opened for auth, no root.

![A Robin session: an xterm on the remote desktop, the status tab at the top](docs/img/session.png)

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
linux/amd64 and linux/arm64, and embeds both in `bin/robin`. `make dist`
cross-builds the CLI for macOS, Linux and Windows on amd64 and arm64 into
`dist/`. The CLI itself is pure Go; only the agents need the C toolchain, and
the build downloads it, so there is nothing to install by hand on Linux or
macOS.

## Usage

```sh
robin gui alice@build-box
robin gui build-box -- -p 2222 -i ~/.ssh/other_key   # anything after -- goes to ssh
robin gui --max-size 2560x1440 --fps 60 workstation
robin gui --no-browser lab-node                     # print the URL instead of opening it
robin gui --attach auto workstation                 # the desktop you are logged in to there
robin gui --attach wayland workstation              # your GNOME or Plasma session on Wayland
robin gui --attach auto --curtain --silent workstation  # and keep its own screen dark and speakers quiet
robin gui --share alice build-box                   # and let alice, another account there, watch
robin gui build-box                                 # again later: picks the desktop up where you left it
robin ls build-box                                  # the sessions the host keeps
robin end build-box                                 # end one without opening it
```

Your `~/.ssh/config`, agent, `ProxyJump`, `ProxyCommand`, certificates and
password or 2FA prompts all work as they do for plain `ssh`, because it is
plain `ssh`: one ControlMaster connection carries the probe, the agent upload,
the agent itself and the tunnel. Windows' OpenSSH has no ControlMaster, so
there each of them opens a connection of its own (see
[What the client needs](#what-the-client-needs)). The browser tab can be
reloaded; the remote session survives until you press Ctrl+C in the
terminal.

The desktop survives more than that. A desktop Robin started stays on the
host when the connection drops, for `--keep` (8 hours by default), and the
CLI keeps redialing meanwhile: the page says Reconnecting, then comes back to
the same windows. Close the laptop, change networks, lose the VPN, and the
session is where you left it. The CLI itself may die, or you may choose to
leave: the page's status tab has a Disconnect, keep desktop button beside
End session, and a later `robin gui <host>` resumes the one kept desktop by
default (`--new` starts another beside it; with more than one kept, the CLI
lists them and asks for `--id`). `robin ls <host>` shows what the host
keeps, with when each kept desktop ends on its own, and `robin end <host>
[id]` ends one. Ctrl+C in the CLI, the page's End session button and an
error that stops the CLI once the desktop is up all end the session,
desktop included, even when the CLI has to reconnect to say so; the page
reports the end only once the host confirmed it, and says so when it could
not. An attached desktop is not kept by default: nothing of yours would be
lost, so `--keep` defaults to 0 there and the session ends with the
connection as before. Name a `--keep` and it is kept like any other.

A resumed session is the same session: the agent on the host kept its
identity, so the fingerprint the CLI pinned still holds, and a CLI that
carries a different agent than the one running refuses to resume, before it
uploads anything, and says to run `robin end`, since the daemon runs code
this CLI does not carry. Started beside it with `--new`, the upload leaves
the kept session's agent in place, so `robin-send` in that desktop keeps
working.

A new `robin gui` that resumes a desktop gets what its own command line
names, or is told why not. `--curtain` and `--silent` switch on the curtain
and the silent room of a kept attached desktop before the page connects,
as they would for a new one, and a switch the page had turned on stays on;
when one cannot go on, the CLI says why and leaves the desktop as it was,
still kept. The microphone and camera are the ones the desktop started
with: a resume that offers one it lacks is refused, with how to get it
(resume without the flag, or `robin end` and start again), and a resume
without `--mic` or `--camera` offers neither to the page. The flags that
shape the desktop itself (`--max-size`, `--fps`, `--max-kbps`, `--encoder`,
`--monitor`, `--audio`, `--session`) apply to a new desktop only. The same
CLI redialing after a drop asks for nothing, so a switch the page turned
off stays off.

With `--attach`, nothing new is started: the agent streams a desktop that is
already running on the host, usually the one you are logged in to at the
machine. `wayland` takes your Wayland session through the desktop's screen
sharing portal, which asks first (see
[Attaching to a Wayland desktop](#attaching-to-a-wayland-desktop)). The rest
of this section is about X11. `auto` picks the Wayland session when that is
the active one, else the display of your active X11 session, or the one X
server you own when the session does not name one (GDM never does), and `:1`
names a display outright. The cookie comes from wherever startx, LightDM, GDM, mutter
or SDDM left it; failing that, the agent gets in the way an
`xhost +si:localuser:$USER` grant allows.

That desktop stays shared: whoever sits at the machine sees the pointer move,
unless you draw the curtain (see [A private session](#a-private-session)).
It keeps its own resolution, too. Your browser window scales the picture
instead of resizing the screen, and a desktop larger than `--max-size` (a 4K
monitor) is scaled down to fit, with the pointer mapped back onto the
full-size screen. Scaled text is soft: a 2560x1440 monitor squeezed into
1920x1080 and stretched back by the browser loses its small text, which
is why 2560x1440 is the default where the host has the cores for it, and
why sharp text draws still parts of the screen at the desktop's own size
(see [Sharp text and actual size](#sharp-text-and-actual-size)).

A desktop of several monitors shows one of them at a time, starting with the
primary one; `--monitor` picks another by name (`--monitor DP-2`) or by its
number in `xrandr --listmonitors` order (`--monitor 2`), and `--monitor all`
shows every monitor as one picture. While connected, the status tab at the
top of the page lists the monitors, with their sizes, and switches between
them. Three monitors side by side in one picture would be shrunk until text
is unreadable, which is why one monitor is the default. Ending the session closes the agent's X
connection and nothing else; autorepeat and the keys it bound for characters
missing from the keymap go back to how they were, and clipboard text pasted
from the browser is handed to the desktop's clipboard manager if one runs.
The desktop's sound comes along. A browser only starts a video by itself
while it is muted, so the page opens silent and the status tab shows a
speaker: click it to hear the session, and again after a reload. When the
host cannot provide sound the session runs without it, and the CLI says why.

`--mic` and `--camera` offer your microphone and camera to the host's
programs, as Robin microphone and Robin camera (see
[Microphone and camera](#microphone-and-camera)). Both start off. The status
tab gets a switch for each the host could take; switching one on asks the
browser for the device, and a red dot shows while it is sent. The CLI says
once, at the start, where each went on the host or why it could not.

| Flag | Default | |
|---|---|---|
| `--transport` | `auto` | `direct` for UDP only, `tunnel` to force the SSH path |
| `--max-size` | `2560x1440` | largest remote resolution; the display follows your window up to this, and a larger attached desktop is scaled down to it. 1920x1080 on hosts with four cores or fewer, 1280x720 with two or fewer |
| `--fps` | `30` | frame rate cap (20 on small hosts) |
| `--hidpi` | off | size the remote display in the browser's device pixels: sharp text on Retina screens, about four times the pixels to encode. Raise `--max-size` to go fully native |
| `--max-kbps` | `10000` | video bitrate ceiling; congestion control works below it |
| `--encoder` | `auto` | `auto` encodes on the host's GPU where it pays and in software elsewhere; `software`, `vaapi` or `nvenc` insist on one; see [Hardware encoding](#hardware-encoding) |
| `--session` | lightest found | command to run in the display, e.g. `startxfce4` |
| `--attach` | | stream a desktop already running on the host: `auto`, `wayland` for your Wayland session, or an X display such as `:1` |
| `--monitor` | `primary` | the monitor of an attached X11 desktop to show first: a name such as `DP-2`, its number, or `all`; the page switches between them. Wayland shows the monitor picked in its sharing dialog |
| `--keep` | `8h`, `0` with `--attach` | how long the host keeps the desktop after the connection drops or the page detaches; `0` ends it with the connection |
| `--id` | the one kept session | which kept session to resume |
| `--new` | | start a new desktop even when a kept one could be resumed |
| `--share` | | invite this account on the host to join as a guest, who watches until you give them control; repeat for more (see [Sharing a desktop](#sharing-a-desktop)) |
| `--join` | | join another account's session as a guest, with the code its owner was given, over your own login to the host |
| `--audio` | `true` | stream the desktop's sound; `--audio=false` leaves it out |
| `--mic` | off | offer your microphone to the host's programs; it stays off until you switch it on in the page |
| `--camera` | off | offer your camera to the host's programs, through PipeWire or a v4l2loopback device; off until switched on |
| `--curtain` | off | attached desktops: black out the host's screen while the page still shows it, from the start or, with `--id`, as the CLI resumes a kept one. The keyboard and mouse at the host still work; see [A private session](#a-private-session) |
| `--silent` | off | attached desktops: keep the host's speakers quiet while the page still plays the sound, from the start or as the CLI resumes |
| `--stun` | Google's public server | STUN servers for NAT traversal, comma-separated, or `none`; the browser and the agent each ask them for their public address (see [Security](#security)), except with `--transport tunnel`, which asks none |
| `--no-browser` | | print the page URL on stdout instead of opening a browser |
| `--listen` | `127.0.0.1:0` | address for the local page; loopback only |
| `--ssh` | `ssh` | the ssh client to drive |
| `--ssh-mux` | `true`, `false` on Windows | carry everything over one ControlMaster connection; `--ssh-mux=false` gives the probe, the upload, the agent and the tunnel an ssh connection each, as on Windows |
| `-v` | | log the CLI and the agent to stderr |

`robin ls` and `robin end` take `--ssh`, `--ssh-mux` and `-v` as well.

## Sharp text and actual size

H.264 as browsers decode it keeps colour at a quarter of the picture's
resolution, which smears coloured text and softens the rest, and a picture
the browser scales by a fraction blurs it again. The status tab has a switch
for each.

**Sharp text**, on by default, sends the parts of the screen that have been
still for 300 ms again without loss: 64x64 tiles, compressed to PNG on the
host within a quarter of one core and drawn on a layer over the video, at
the desktop's own resolution even where the video is scaled down to
`--max-size`. A tile goes the moment its part of the screen changes, before
the frame that changes it shows, so what moves is plain video and text you
stop at sharpens about half a second later. A screen full of small text
costs about 700 KB once at 2560x1440, and nothing more while it stays still.
A tile that keeps changing soon after it was sent, as one with a blinking
caret does, waits longer each time and soon stays video rather than flicker.
Sharp text needs an X11 desktop and a browser that says which frame it shows
(the RTP timestamp in `requestVideoFrameCallback`, which Chrome, Edge and
Firefox give); otherwise the switch is greyed out with the reason under it.

**Actual size** shows each desktop pixel as a whole number of your screen's
pixels instead of fitting the picture to the window: one, or two on a screen
of twice the density, so text never goes through a fractional scale. A
desktop larger than the window pans, and the status tab says how: hold the
pointer near an edge of the window, or on a touch screen drag with two
fingers. Pointer and touch input land where the picture shows them either
way.

## Files and the clipboard

Files go to the host when you drop them anywhere on the page or pick them with
Upload files in the status tab. Files you paste are listed there too, and each
goes only when you click its Upload, since the clipboard can still hold files
you copied long ago. They land in your download folder there:
`$XDG_DOWNLOAD_DIR`, else the one `~/.config/user-dirs.dirs` names, else
`~/Downloads`, which is created if it is missing. A download folder that is
your home directory itself counts as unset, which is what xdg-user-dirs-update
leaves once `~/Downloads` is gone, so uploads then go to `~/Downloads` too. A
file whose name starts with a dot is refused, so no upload is hidden. Nothing
on the host is ever replaced; a second `report.pdf` becomes `report (1).pdf`,
and the status tab shows the name it got.

Getting a file from the host starts on the host:

```sh
robin-send report.pdf build/*.log                # in a terminal of the desktop Robin started
~/.cache/robin/robin-agent-* send report.pdf     # anywhere else on the host, an attached desktop or ssh
```

The page lists each offer by name and size, and nothing downloads until you
click Save; Dismiss turns it down. Save takes the file as it is by then, but
never more than the size shown: a file that grew since its offer is refused,
and both the page and `robin-send` say so. `robin-send` stays until every file it
offered has been saved or declined, prints which, and exits non-zero unless
all were saved. Ctrl+C there withdraws whatever is still on offer. Programs in
a spawned desktop have `robin-send` on their `PATH` and `ROBIN_FILES_SOCK`
naming their session; anywhere else the agent offers to the one session it
finds running, and refuses to guess between several.

The clipboard carries plain text, HTML and PNG images both ways, up to
16 MiB at a time, so a screenshot copied on one side pastes as a picture on
the other. One PNG pasted into the page goes to the host's clipboard as an
image. Any other file you paste there becomes an upload waiting for its
click instead, because a browser's clipboard has no way to hand a file on as
a file, and the browser only takes what the host copies while the page has
focus.

## Keys and macros

The keys button in the status tab, beside full screen, opens a section
that sends what your keyboard cannot get to the desktop: Ctrl+Alt+Delete, Alt+Tab, Alt+F4, Super, Ctrl+W, Ctrl+T, Ctrl+N,
Ctrl+Q and Print Screen. Outside full screen the browser or your own
system keeps these for itself (Ctrl+W closes the tab); in full screen
Chrome's keyboard lock passes most of them on, and the Keys row covers the
rest and the other browsers. There are no Ctrl+Alt+F keys: on an attached
desktop they would switch the host's console away from the session. The
tab stays out while the section is open, and the section starts closed in
every new page.

Type clipboard leads the Macros row. It reads this computer's clipboard
when you click it and types the text as keys, up to 16 KiB, for prompts
that take no paste: a lock screen, a VM console, a password dialog. On a
Wayland desktop that holds only for ASCII. Any other character makes the
agent paste the text through the desktop's clipboard instead (see
[Security](#security)), where a clipboard manager may keep it and where a
lock screen, which takes no paste, gets nothing.

Macros are your own. New macro opens an editor for a name and steps, each
a chord, a run of text or a pause. Record keys takes what you press and
sends none of it to the desktop until you stop: a key that types one
character, alone or with Shift, joins the run of text before it, so a typed
command is one step, and any other key is a chord with the modifiers held
as it went down. A click on a key in the Keys row while recording adds
that key, which is how a macro gets Ctrl+W outside full screen. Text and
pauses are added below. Click a macro to play it and again to stop it. It
plays as the same key and text messages the keyboard sends, holds nothing
down when it ends, and stops when the page loses the keyboard or the
connection. Delete in the editor asks for a second click.

The CLI keeps the macros, not the browser, since the page's address is a
new port every session and the browser's storage for it goes with it. They
live in one file, `macros.json` in your config directory, shared by every
session on this computer:

| Client | File |
|---|---|
| Linux | `$XDG_CONFIG_HOME/robin/macros.json`, else `~/.config/robin/macros.json` |
| macOS | `~/Library/Application Support/robin/macros.json` |
| Windows | `%AppData%\robin\macros.json` |

A new macro is pinned to the host argument as you typed it, so it shows
only in sessions to that host; untick Only on in its editor to show it on
every host. `alice@box` and `box` are two hosts.
Text in a macro is stored as plain text, and the editor says so where you
type it. A file Robin cannot read, one that breaks its rules, a link, or
one that is not yours or that others can write, is reported in the
terminal and in the page and left as it is, and Robin saves no macro until
it reads again.

## What the host needs

Linux on x86_64 or arm64, an SSH server you can log in to, and for the
headless session: `Xvfb` plus something to run in it, either a desktop
(`xfce4-session`) or a window manager (`openbox`, `fluxbox`, `icewm`, `twm`)
with a terminal (`xterm`). If anything is missing the CLI says what, with the
install line for that distro, for example:

```
sudo apt install xvfb openbox xterm
```

Attaching to a running desktop needs none of that, only the desktop, plus
GStreamer's tools for a Wayland one.

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

The agent goes to `~/.cache/robin` (or `$XDG_RUNTIME_DIR/robin`, or
`/tmp/robin-$UID` when the home directory is mounted `noexec`), checked
by hash and reused across sessions. It runs as you, starts its own display on
a free number, and when the session ends it kills everything it started
and removes its files. The session lives in a daemon of its own, in its own
process session (`setsid`), so sshd closing the channel does not reach it;
where logind is set to kill user processes on logout (`KillUserProcesses=yes`,
not the default), the daemon runs under `systemd-run --user` instead, and the
CLI says so when that would still not outlive your last login (it does once
`loginctl enable-linger` is on). Where neither is possible the desktop ends
with the connection, and the CLI says that too.

## Attaching to a Wayland desktop

`--attach wayland` streams the GNOME or KDE Plasma session you are logged in
to on the host instead of starting a new one. It goes through
xdg-desktop-portal, the same screen sharing a video call app uses, which
means the desktop asks before anyone sees it. `--attach auto` picks it when
the session logind reports as active is a Wayland one.

The first time, a dialog comes up on the host's own screen and someone sitting
there has to answer it. On GNOME, switch on Allow Remote Interaction before
pressing Share, or the session is view only: the page shows the screen and
still trades the clipboard both ways, but sends no keys, pointer or uploads.
The CLI says so if that happens, and also says, while it waits, what to
approve and for how long (two minutes).
KDE Plasma asks once for both the picture and the input, and Approve grants
them together.
Approval gets the agent a restore token from the portal. The agent keeps it
next to itself on the host, in `wayland-restore-token`, readable only by you,
and every later session starts with no dialog at all. Delete the file to be
asked again. A view-only approval is never remembered, so the next run asks
once more.

Ending the session gives the screen back as it was. Stopping the share from
the host's own indicator ends the remote session too. Sound comes along as
it does on X11, recorded from PipeWire's PulseAudio server, which current
GNOME and Plasma desktops run.

The agent reads the screen through GStreamer, so the host needs
`gst-launch-1.0` and the PipeWire, base and good plugin sets. A desktop
install usually has everything but the tools package; the CLI checks and
prints the line for your distribution, for example:

```
sudo apt install gstreamer1.0-tools gstreamer1.0-pipewire gstreamer1.0-plugins-base gstreamer1.0-plugins-good
```

Streaming a fully animated 1280x720 screen at 30 fps costs `gst-launch-1.0`
about 6% of one core, on top of the encoder. An idle screen costs next to
nothing: repeated frames are not encoded.

## Microphone and camera

`robin gui --mic --camera host` lets the host's programs, a video call in
the remote desktop say, use the microphone and camera of the machine you
sit at. Nothing is sent until you switch a device on in the status tab, and
the browser asks for permission the first time. Switching off stops the
device, so the browser's own camera light goes out too.

Where they appear on the host:

| Desktop | Microphone | Camera |
|---|---|---|
| spawned | the default source of the session's private pulseaudio, "Robin microphone" | a v4l2loopback device, when you have one |
| attached, PipeWire | a PipeWire source, "Robin microphone", that goes when the session ends | a PipeWire camera, "Robin camera", or a v4l2loopback device when you have one |
| attached, PulseAudio | a source, "Robin microphone", on your PulseAudio, unloaded when the session ends | a v4l2loopback device |

The microphone needs what sound needs (`pulseaudio` for a spawned desktop),
and on PipeWire also `gst-launch-1.0` with GStreamer's PipeWire plugin, as
Wayland attach does. Attached, the microphone is one more source to pick
in a program's settings; robin does not make it your default. The camera
is 1280x720 whatever your camera sends: a picture of another shape is
centred on black. While it is off, or the browser sends nothing, it shows
black, so the last picture never stays on the host's screen.

A PipeWire camera is seen by programs that use PipeWire for cameras, such
as GNOME Snapshot, OBS and browsers set to use the camera portal. Others
open `/dev/video*` and need v4l2loopback, which is also the only way to give
a spawned desktop a camera:

```
sudo apt install v4l2loopback-dkms
sudo modprobe v4l2loopback card_label="Robin camera" exclusive_caps=1
```

The probe finds the loopback devices you can write and the camera takes the
one labelled Robin, else the first, in preference to PipeWire.

## A private session

An attached desktop shows everything you do to whoever sits in front of
it, and plays its sound through their speakers. `--curtain` blacks out the
screen at the host and `--silent` keeps its speakers quiet, while the page
still shows the picture and plays the sound. Both are off unless you ask,
and the status tab has a switch for each that works mid-session; when you
attach without them, the CLI says the room can see and hear you and names
the two flags. A switch clicked while the page reconnects waits, busy,
and goes to the host once the page is back. `robin gui --id <id>
--curtain` turns the curtain on as it resumes a desktop kept with
`--keep`, and fails with the reason, the desktop still kept, when it
cannot. A desktop robin starts has nobody in front of it, so there the
flags are refused.

The curtain hides the screen without locking it. The keyboard and mouse at
the host keep working, so someone there can still type into your session,
only blind. On X11 the curtain sets the gamma ramp of every screen to zero:
gamma is applied on the way to the monitor, after the picture the agent
captures. On GNOME under Wayland it does the same through mutter, and KDE
Plasma has no way for a program to set gamma, so there the switch is
missing and `--curtain` fails with the reason. Every screen needs a ramp a
program can set: one without (some USB and DisplayLink monitors, virtual
GPUs) would stay lit, so there the switch is missing too, and the reason
names the screen. Night light and colour profiles set those ramps as well.
The curtain checks once a second and darkens again, and lifting it restores
whichever ramp was set last. A screen it finds lit and cannot darken, such
as a monitor without a ramp plugged in meanwhile, shows as an error under
the switch, which stays on for the screens it does cover.

The silent room gives the desktop's sound server one more output, listed as
Robin silent room in its sound settings while the room is silent, and moves
every program's sound into it the moment the program starts to play. The
session records that output, so it hears every program, including the ones
that were playing somewhere other than the default output. The default output
itself is never changed. Under PipeWire the new output belongs to a
`pw-record` process (PipeWire's tools, `pipewire-bin` on Debian and Ubuntu,
`pipewire-utils` on Fedora) when the host has it, and goes away with that
process; otherwise it is a PulseAudio `module-null-sink`, which works through
`pipewire-pulse` too. Switching it off or ending the session moves each
program back to the output it was on. A program that asks its sound server
never to move its stream goes on playing at the host, and the switch names
it until it stops. Sound already in another session's silent output stays
there.

Ending the session for any reason, Ctrl+C, a dropped connection or the
idle timeout among them, lifts the curtain and gives the speakers back. A
desktop you attach to with `--keep` stays dark and quiet while the host
keeps it for you, until it ends. The screen comes back unlocked, with
whatever you left open on it: robin does not lock it for you.

Before it changes anything, the agent hands a child of its own, `robin-agent
keeper`, what would undo the change. When the agent ends normally it undoes
everything itself and the keeper exits with nothing to do; when the agent is
killed, the keeper gives the room back its screens and speakers.

## Sharing a desktop

```sh
robin gui --share alice build-box                    # your desktop, which alice may join
robin gui --join a1b2c3d4-1000-k3j4... alice@build-box  # alice, from her own computer
```

`--share alice` invites another account on the host to watch your session.
Once the desktop is up the CLI prints the command that account runs, with a
code that works once, for that account only, for 10 minutes; repeat
`--share` for more accounts. The status tab has a Share part that does the
same for a name typed there, lists the invitations still open with a Copy
button beside each command, and lists every guest. A new invitation for the
same account replaces its old code, and three wrong codes from that account
withdraw it.

The guest runs `robin gui --join <code> alice@host` on their own computer,
over their own SSH login: their CLI puts its own agent in their own home
directory, and that agent knocks on your session's door on the host. Their
page shows your desktop, pointer and sound, and says whose desktop it is.
It is view only until you click Give control on the guest's row: then
their keys, pointer and paste reach the desktop, the host's clipboard
reaches them, they see the files `robin-send` offers and may upload, and
with `--mic`, in a session that started with `--mic`, their microphone is
heard on the host. One guest has control at a time: giving it to another
takes it from the first. You keep yours throughout, and closing or
reloading your page takes control from every guest. Take back control
ends theirs at once: what they held down is let go, text still being
typed is dropped, the files on offer leave their page, a transfer of
theirs still under way is dropped, and so is a file or a copy on its way
to them. Remove closes their peer connection and
their page at once, and their CLI exits saying why. A guest leaves with
Ctrl+C or the page's Leave button.

Everyone watches one stream. Each frame is encoded once, at the bitrate the
slowest page's link allows, and sent to every page, so a guest on a poor
link makes the picture coarser for you too; a page that loses a picture
asks for a keyframe, which every page gets. What shapes the session stays
yours: a guest's window does not resize the desktop, and the monitor
shown, the curtain and the silent room, sharp text, the camera, keeping and
ending the desktop are yours alone.

A guest never outlives your session. When your CLI goes, by Ctrl+C, a
dropped connection or Disconnect, keep desktop, every guest is let go and
every invitation withdrawn, and nobody joins until you are back; ending the
session ends every guest's CLI with it. A guest whose connection drops does
not come back on its own: the code it joined with is spent, so it needs
another invitation.

## Hardware encoding

```sh
robin gui --max-size 3840x2160 build-box   # auto: the GPU, if build-box has one
robin gui --encoder vaapi build-box        # Intel or AMD through VA-API, or fail
robin gui --encoder nvenc gpu-box          # NVIDIA, or fail
robin gui --encoder software build-box     # never the GPU
```

By default the CLI picks the encoder from the probe. It takes the GPU when
the host can drive it and the GPU pays for the latency it adds: on hosts
with four cores or fewer, and for sessions above 2560x1440 when ffmpeg can
drive the GPU. VA-API comes before NVENC. Everywhere else it stays in
software. If the GPU it picked fails on the host, the session carries on in
software, and `-v` shows which encoder was picked, why, and why it fell back.
An encoder named with `--encoder` is used or the session fails.

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

That is why `auto` leaves a 1080p or 1440p session on a bigger host in
software: on a reasonably fast CPU the GPU saves little, and openh264
needs 5 to 17 ms of CPU for a 1440p frame. On an i7-13800H with its iGPU,
a session scrolling a terminal cost about a fifth of a core at 1440x900 and
about one core at 3840x2160 with either encoder: copying each frame out to
the tool eats most of what the GPU saves. It pays where openh264 is the
bottleneck, on slow CPUs and on busy 4K content (28 ms of CPU per frame for
openh264 against 12 ms through ffmpeg). GStreamer's route spends 31 to 34
ms on a 2160p frame, a whole frame at 30 fps, so `auto` uses it on small
hosts only. What the GPU costs:

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
- Run through the same quality harness as openh264, an Intel iGPU's
  `vaapih264enc` showed every frame but spent about a third of its target
  on desktop content, and scored 5 to 11 dB below openh264 at 1080p and
  1440p, still screens included.

The measurements and the reasons behind the design are in the
[ADR](docs/adr/0001-architecture.md#hardware-h264-through-a-host-tool-picked-where-it-pays).

## What the client needs

OpenSSH and a browser that decodes H.264 in WebRTC, on one of:

- **macOS or Linux**, where one ControlMaster connection carries the whole
  session, and `/bin/sh` starts that master with Ctrl+C ignored, so the
  keystroke ends the session through Robin rather than by cutting the
  connection. With `--ssh-mux=false` every ssh starts that way.
- **Windows 10 or 11**, with the OpenSSH client Windows ships
  (`ssh.exe` on `PATH`). It has no ControlMaster, so the CLI runs the probe,
  the upload, the agent and the tunnel as an ssh each, and each logs in on
  its own: with a key or `ssh-agent` that goes unnoticed, while a password or
  2FA prompt comes three times a session, five when the agent is uploaded.
  Each ssh starts in a process group of its own, which Ctrl+C does not
  reach, so the keystroke ends the session over the agent's connection
  and asks for no further login; a redial after a dropped network logs in
  again, as at the start.
  The tunnel's forward listens on a loopback TCP port, as Windows' OpenSSH
  forwards no unix sockets. `make dist` builds
  `robin-windows-amd64.exe` and `robin-windows-arm64.exe`; the page opens in
  the default browser.
- **Android**, in [Termux](https://termux.dev) with its `openssh` package:
  `dist/robin-linux-arm64` runs there as it is, and the page opens in
  Android's browser through `termux-open-url` (from `termux-tools`). The page
  takes touch: a tap clicks, a long press right clicks, one finger drags,
  two fingers scroll, and pinching zooms and pans the picture on the phone
  without resizing the desktop. The keyboard button in the status tab raises
  the soft keyboard.

iOS and iPadOS cannot be clients. The CLI has to run beside the browser,
because the page is served on loopback only, and nothing on iOS lets a user
run a program that drives `ssh` and listens on loopback for the browser.

Input methods work in any of these browsers: the keyboard's focus rests in
a hidden text box, so text an input method composes (CJK, or an accent
built from a dead key) goes to the remote desktop once it is committed, and
shows under the status tab while it is being composed. What the end-to-end
tests prove for each browser:

- **Chrome** runs every scenario: picture, input, latency, sound,
  microphone and camera, clipboard, files, resize, transports, attach and
  Wayland. Edge shares its engine.
- **Firefox** passes every check of the direct scenario, latency and sound
  included, given Cisco's OpenH264 plugin. Firefox downloads it by itself
  on first use (it shows under Add-ons and themes, Plugins); a Firefox that
  cannot fetch it offers no H.264, and the page stays black.
- **Safari** on macOS shows the picture and takes keyboard and pointer
  input, checked by hand from an Apple silicon Mac on 2026-10-24. In the
  automated tests it is stood in for by Playwright's WebKit on Linux, which
  shares Safari's WebRTC stack: it negotiates Constrained Baseline H.264,
  decodes it, plays the sound, carries keyboard, clipboard and resize, and
  tears down cleanly. That build renders no remote WebRTC video at all (a VP8
  loopback in a blank page stays black too), so the e2e suite cannot check
  Safari's picture and the hand check is the only one.

## How it works

1. `ssh -M` opens the one connection everything else uses. Without
   ControlMaster, on Windows, each step below opens its own.
2. A POSIX shell probe reports the distro, architecture, live sessions, GPU,
   which spawn-mode tools exist, and a directory where the agent can run.
3. The agent for that architecture is uploaded over SFTP, verified by
   SHA-256 on the host and renamed into place.
4. The agent starts on an exec channel. Its stdin and stdout carry signaling
   as JSON lines, with a heartbeat both ways so a dead connection is noticed
   within seconds. The exec channel only runs a proxy: the session itself
   lives in a daemon the proxy started, reached through a unix socket in the
   session's run directory, so the daemon outlives the channel and a later
   proxy picks it up. Without a controller the daemon closes the peer
   connection, keeps the desktop for `--keep`, and ends with the same
   teardown as bye when the window runs out.
5. It starts Xvfb and a session, or attaches to the X display already
   running, and captures with MIT-SHM and DAMAGE. A Wayland desktop is
   reached through xdg-desktop-portal instead, its frames read from PipeWire
   by `gst-launch-1.0`. Frames are encoded with openh264, or with the GPU
   through the host's ffmpeg or GStreamer where the probe finds that it
   pays, and sent over a Pion peer connection. Sound comes from the monitor of a
   PulseAudio or PipeWire sink, read over the PulseAudio protocol in Go, and
   goes out as Opus from libopus. The browser's microphone comes back as
   Opus, decoded with libopus into a source on that server or PipeWire,
   and its camera as H.264, decoded with openh264 into a PipeWire node or a
   v4l2loopback device. Keyboard, pointer, clipboard and resize
   come back over a data channel and go in through XTest and RandR, or the
   portal on Wayland. Files, images and anything else too big for that
   channel travel in a transfer layer of their own, a data channel each and
   a SHA-256 checked at the far end.
6. The CLI serves the page on loopback, hands it the macros it keeps for
   you, and relays offer, answer and candidates between it and the agent,
   adding one passive TCP candidate that leads into an SSH forward to the
   agent.

The decisions behind each of these, and what measurement changed along the
way, are in [docs/adr/0001-architecture.md](docs/adr/0001-architecture.md).

## Security

The agent's DTLS fingerprint reaches the browser only through SSH and the
local page, and the CLI pins it for the session, so nobody who can merely
reach the UDP or TCP path can stand in for the agent. The page and its
websocket listen on 127.0.0.1 and require a random 256-bit token from the URL
fragment, an exact `Origin` and `Host`, and allow one browser at a time; the
token never goes on a command line (see the client side below). On
the host, the agent's ICE-TCP listener binds loopback, hangs up at once on any
connection unless a process of yours, or root's, still holds its other end
open, which it asks the kernel, lets go within 5 s of any that has not started
ICE, and completes ICE only with the session's credentials; the X display is
protected by a fresh cookie in a 0600 file, which the agent hands only to the
Xvfb it started, checked through the socket's peer credentials, whatever else
listens at the display's socket names; the sound server of a desktop robin
starts listens only on a socket inside the run directory; the agent and its
run directory are 0700. The probe counts only `X<digits>` in `/tmp/.X11-unix`,
which any user can write, and prints nothing that would span lines, so no file
name there can add to what it reports. `robin-agent end` and the sweep of old
run directories take only a process of yours for a session's daemon.
A session also takes guests, but only those you invite (see the guests'
door below).
The page can do on the host what a keyboard, a pointer and a clipboard can do
in that session, give it the sound and picture of a microphone or camera you
switched on (with `--mic` or `--camera`), flip the two switches of a private
session on an attached desktop, say two words about the session itself (End
session, which the CLI carries out as it does Ctrl+C, and Disconnect, keep
desktop, which leaves the desktop for `--keep`), and move files, within
these bounds.
A view-only Wayland grant takes the keyboard, the pointer and uploads away,
and leaves the clipboard and the files the host offers.

The guests' door. A session daemon takes guests on an abstract unix
socket, `@robin-share-<id>`, which has no file and so no permissions: any
account on the host can connect to it. Before it reads a byte, the daemon
asks the kernel who connected (`SO_PEERCRED`) and hangs up unless that uid
holds an open invitation. The invited account then has 5 s to present the
invitation's code, which the daemon compares in constant time, spends on
first use and lets expire after 10 minutes; three wrong ones withdraw the
invitation. A code is 128 random bits beside the session's id and your
uid, and it never goes on a command line: the guest's CLI sends it over
its agent's stdin. At most 8 invitations are open at once, 4 connections
may be presenting a code at once, and a session takes 8 guests, each
getting one new peer connection a second; a guest that stops reading
what the session sends it is dropped within 10 s. The
guest's agent checks that the socket belongs to the uid the code names, so
an account that takes the name once your daemon is gone cannot pose as
your session, and the guest's CLI pins the agent's DTLS fingerprint as
yours does. A guest's ICE-TCP forward runs as the guest, so the tunnel
listener also admits the uid of a guest connected now, and only while it
is; the ICE password of that guest's own peer still decides what a
connection reaches. Without control a guest's page can do nothing to the
desktop: its input, clipboard, transfers and microphone are dropped. With
control it can do what yours can, short of what shapes the session (the
size, the monitor shown, the curtain and the silent room, sharp text, the
camera, keeping or ending the desktop). Either way it sees everything on
your screen and hears its sound. Who was invited, who joined and each
account's first refusal, with its uid, go to the session's log; later
refusals of the same account log only at debug, so knocking over and over
cannot fill it.

On your own computer the page can ask the CLI to write one file, your
macros (see [Keys and macros](#keys-and-macros)), and nothing else. It
names no path: the CLI writes `macros.json` in your config directory and
nowhere else. The page asks for one change at a time, to add or replace
one macro or to delete one, and the CLI checks it before writing: a name of
up to 64 bytes of printable text, 1 to 100 steps, each a chord of up to 8
keys, up to 16 KiB of text or a pause of up to 60 s, and at most 100 macros
and 256 KiB in the file. It reads the file afresh for every change, refuses
the change while the file does not read, and writes the whole file anew to
a temporary file, 0600 in a 0700 directory, which it then renames into
place, holding a lock on `macros.json.lock` beside it from the read to the
rename so two CLIs never undo each other's change. On Linux and macOS it
reads the file through one open that follows no link and then checks what
it opened: a regular file of yours that group and others cannot write. The
directory has to be yours and no link before the CLI makes it 0700. A
macro pinned to another host is out of the page's reach: it can neither
see, replace nor delete it. Within that, whatever drives the page can read
the macros of this host and every unpinned one, text included, type them,
and replace or delete them.

What a page can make the agent hold is bounded too. It gets one control
channel at a time; a second closes the first. A bulk channel that has not
sent its header within 10 s is closed, and past 8 such channels at once a
new one is refused. The agent takes media only on the microphone and camera
lines the session asked for, and the answer turns down every other line the
page adds; what the page sends on the agent's own video or sound line, which
the answer cannot turn down, is read and dropped. Of the ICE candidates the
page names, the agent counts 256 per ICE restart, drops those at the host's
loopback or a link-local address, never asks the host's network to resolve
a `.local` name, and never connects out over TCP. It refuses an offer whose
sections disagree on their ICE credentials, or whose credentials hold
anything but the letters, digits, `+` and `/` RFC 8839 allows. So the most
the page can aim at other addresses is ICE's own connectivity checks over
UDP, carrying only those credentials.

Text from an input method or a soft keyboard, from Type clipboard and from
a macro's text steps types what keys could type: through spare keycodes on
X11 and, on Wayland, as keysyms for ASCII and as a paste for the rest, so
one character outside ASCII sends the whole text that way. That paste puts the text on the desktop's clipboard
for about a second, where any program on the desktop can read it, before
the clipboard gets back what it held: what was copied on the desktop or set
from the page during the session, in each format the agent reads, or the
text that was there before it; where the agent knows of nothing there, it
gets empty text. A clipboard manager there reads the paste as it would any
copy and keeps it in its history.

- **Writing files**, into the download folder and nowhere else, and never
  into your home directory itself, even where xdg-user-dirs names it as the
  download folder. A name has to be a single path component of UTF-8, at
  most 255 bytes, not starting with a dot, so an upload can be neither
  hidden nor a startup file such as `.bash_login`, with no control
  characters and none of the invisible marks that reorder text. A file
  pasted into the page goes only once you click its Upload. The bytes go to
  a new file of a temporary name, opened so that it can be neither an
  existing file nor a symlink, and the file gets its name only after every
  byte arrived and the SHA-256 matched, through a rename that fails rather
  than replace anything. An upload bigger than the free space less 256 MiB
  is refused before a byte is written, four at most run at once, and one
  that is cancelled, fails, or is still running when the session ends is
  removed, unless the disk under it stopped answering (see Known limits).
- **Reading files**, only those a program running as you on the host offered
  with `robin-send`, and only after a click on Save in the page, which takes
  one transfer for each click. `robin-send` opens each file itself and hands
  the session its descriptor over a socket in the 0700 run directory, which
  also checks that the caller's uid is yours, so the page never names a path
  on the host and the session reads nothing its sender could not. Offers end
  when `robin-send` exits, and at most 256 files are on offer at once, from
  every `robin-send` together; past that a request is refused whole.
- **The clipboard**, as text, HTML or a PNG image, up to 16 MiB, held in
  memory, two transfers at a time each way.

File names from the host appear in the page as text, never as markup. Words
from the page that reach a terminal on the host, a refusal's reason printed by
`robin-send`, have their control characters replaced first, and a file name it
prints that holds any, or a mark that reorders text, is printed quoted with
escapes, as `ls` does. Bulk transfers of any kind the agent has not registered
are refused. A transfer runs at most 512 KiB ahead of what its receiver has
taken, and the page fails one from the agent that runs past that, so a
stalled transfer holds back neither the keyboard nor the page's memory.

The page holds the host to bounds of its own. A host may have two clipboard
transfers open to the page at once, a newer copy pushing out the oldest,
and a download only for each click on Save; what arrives is kept as it
comes, never reserved ahead for the size a header claims. A download may be no
larger than the offer you clicked, and the host can withdraw only the files
it offered, never your uploads. The page keeps at most 128 pointer images
of at most 256 KiB each, and puts nothing from the host into the pointer's
CSS but base64 and whole-pixel hot spots.

Sharp text brings pictures of the screen into the page a second way: tile
transfers from the agent, taken only while the switch is on, at most 4 MiB
each and four at once, open or still being decoded. Each atlas's PNG header
must give the size its tiles take, at most 1024x1024, before the browser's
own image decoder reads it, and a tile manifest or batch that strays outside
its grid drops every tile instead of growing what the page keeps. Tiles are
drawn on a canvas and nowhere else; the page sends nothing new because of
them but the switch itself.

A kept session is reached through a unix socket in its 0700 run directory,
and the daemon accepts a connection only from a process of its own uid
(`SO_PEERCRED`), one controller at a time. Resuming goes through the same
SSH login as starting did, and the daemon keeps its DTLS certificate for its
whole life, so the fingerprint the CLI pinned at the start still has to
match after a reconnect: anything else answering in the session's place ends
it. A fresh `robin gui` that resumes pins the fingerprint of the daemon it
finds, trusting SSH and the uid check as the first run did. While a session
is kept, its X server, its sound socket and its loopback ICE-TCP listener
stay up in the 0700 run directory for the whole keep window, reachable by
anything running as the same user on the host, as they are while a CLI is
connected.

The microphone and camera reach every program that can record on the host
as you: your sound server and PipeWire are yours alone, but a v4l2loopback
device is readable by whoever its permissions admit, usually the `video`
group, so on a shared host the camera's pictures go to them too. Nothing
leaves the browser while a switch is off, and the agent never turns one on.

A private session changes the attached desktop for as long as it lasts: the
gamma of its screens and, with `--silent`, the user's own sound server, which
gets an extra output and has the user's programs moved into it. PulseAudio
and WirePlumber 0.4 remember those moves, and the undo moves each program
back to clear them. A keeper process undoes both if the agent is killed, but
not if the keeper is killed along with it. Then a module-made silent output
stays until it is unloaded (`pactl list short modules | grep robin_silent`,
then `pactl unload-module` with its number), and the screens stay black
until something sets their gamma. On X11 that is `xrandr --output NAME
--gamma 1:1:1 --brightness 1`; on GNOME under Wayland, switching Night Light
on and off, or logging out. The person at the desk would be typing blind, so
do it from another machine over SSH or from a text console.

The curtain is no lock. Anyone at the machine can type and click while it is
drawn, and on GNOME it also blacks out the indicator that says the screen
is being shared, which leaves the black screen itself as the only sign.
Whatever ends the session, a dropped connection included, lifts the curtain
and leaves the desktop unlocked in front of whoever is there. The curtain
refuses a desktop with a screen it cannot darken rather than leave that
screen lit, but a screen plugged in while it is drawn stays lit, with an
error under the switch, and so does a program that refuses to move out of
the room's speakers.

On the client, other users of the same machine can read your processes'
command lines and connect to any loopback port, so the CLI puts nothing on
the first that would let them in through the second. To open your browser
it hands `open`, `xdg-open`, `rundll32` on Windows or `termux-open-url` on
Android a launch link, not the page URL: the link holds a second random
secret, sends the browser on to the page with its token once, and is not
found after, though a browser the opener starts keeps it on its own command
line for as long as it runs. Someone who reads the link and opens it before
your browser does gets the page; your browser then shows Not Found, and the
CLI says on stderr that the link was opened again, so end the session with
Ctrl+C. The CLI also says when a browser first opens the page, so a tab of
yours still loading after that line means the same. `--no-browser` prints
the page URL, token included, on stdout instead, and an opener that fails to
start has it printed on stderr. The page server closes a connection that
sits idle for 30 s or sends no request within 2 s, and holds at most 32 at
once: past that it makes room for a new one by closing the oldest that is
idle or has sent nothing, never the page's websocket. Another user can
neither run the CLI out of file descriptors through it nor keep your
browser waiting behind connections they hold.

The ICE-TCP tunnel's client end, where the browser connects, is a loopback
TCP port in every mode, so another user of the client machine can connect to
it too. The CLI passes a connection on into SSH only once its first message,
within 5 s, is a connectivity check signed with the ICE password of the
agent's latest answer, which only the page has seen; anything else is closed
without an SSH channel opened for it, and the DTLS fingerprint is pinned as
before. At most 16 connections wait for that check at once, and past that
the oldest is closed, so connections that say nothing cannot run the CLI out
of file descriptors either. Behind that port the SSH forward is a unix socket in a private
directory, with ControlMaster or without, except on Windows, whose OpenSSH
forwards no unix sockets: there it is a second loopback port, which ssh
binds only once it has logged in. The CLI uses it only once the system's
table of listeners shows ssh holding it, so another user who takes the port
first gets the tunnel refused, not connected to them. The forward's target
comes from the agent and is refused unless it is a loopback address and port,
as the agent's listener has, so a host can neither slip other fields into
ssh's `-L` nor have ssh expand `${VAR}` in it from your environment.

Text from the host that the CLI prints, the agent's log and errors, the
probe's findings and what ssh writes, has its control characters, the
invisible marks that reorder text and the Unicode line separators replaced
with escapes, so a host can neither send your terminal commands nor make a
path or a command read as another. The CLI keeps at most 1 MiB of what a
command on the host prints, and shows a log line with no end in 8 KiB
pieces, so a host cannot fill its memory either.

The browser and the agent each ask the STUN server that `--stun` names,
Google's public one by default, for their public address at the start of
every peer connection, which tells that server the public addresses of the
host and of your machine. Its answers are not authenticated: whoever can
steer DNS or the path to it can give either end a wrong address, which costs
a candidate pair or the direct UDP path, never the session, since the agent
is known by the fingerprint pinned over SSH. `--stun none`, or a server of
your own, avoids the disclosure at the cost of direct UDP through NAT;
`--transport tunnel` asks no STUN server at all.

## Testing

```sh
make test     # unit and integration tests; the encoders against real openh264 and libopus on linux/amd64
make test-js  # the page's pure modules under node --test (make test runs them too where node is installed)
make test-x   # capture, input, display, sound server and agent session tests against Xvfb and pulseaudio, in podman
make test-wayland  # Wayland attach against headless GNOME 46, GNOME 49 and KDE Plasma 6.7, in podman
make e2e      # the whole thing: podman host, Playwright and Chrome client
make check    # all four
make cover    # all four again with coverage, merged, checked against coverage-baseline.txt
make repro    # builds the agents, the CLI and dist twice, from two paths, and compares them
```

`make cover` runs every suite with coverage and merges it in
`.cover/`: the unit tests (the cgo-tagged encoder packages included),
the test-x and test-wayland binaries, and e2e with the CLI and the agent
built with `-cover`. Every Go suite instruments the whole module, so a
session test in `internal/agent` counts for `x11` and `peer` too. It prints
statement coverage per file and per package (`summary.txt`, with
`merged.out` and `cover.html` beside it), and line coverage per script of
the page (`page.txt`), from Chrome in e2e and from `make test-js`. It fails
when a package falls more than 0.5 points below its line in
`coverage-baseline.txt`; `make cover-baseline` rewrites that file from the
last run, which only main does. `make cover-unit cover-report` gives a quick
look without containers. A suite that fails still hands in what it
covered, and `make cover` fails after the report.

`make test` also streams a 15 s clip of a 1080p screen of small text,
still and moving, through the agent's stream loop, peer and openh264 to a
stand-in for Chrome's video receiver (`internal/agent/vqtest`), and fails
if the browser would have had to ask for a keyframe. `ROBIN_VQ=1` with
`go test -tags openh264 -run TestVideoQualityMatrix ./internal/agent/`
(in the toolchain `make test` sets up) runs the longer clip at 1080p and
1440p over three shaped links and prints, per stretch of the clip, what
was sent, what was shown and how it scored against the screen.
`ROBIN_VQ=all` adds the variants that put back each earlier policy, and
the `vaapi` variant, which streams through the host's VA-API encoder where
one works, and `-run` picks cells by size, link and variant, as in
`-run 'TestVideoQualityMatrix/^1440p$/tight/unpaced$'`. `make test` also
scrolls, sits still for 20 s with sound playing and scrolls again over the
tight link (`TestVideoQualityAfterStill`), and fails when the first second
after the still shows under a third of the settled frame rate or went out
at under half the settled target; `ROBIN_VQ=1` with
`-run TestVideoQualityAfterStillMatrix` does it with a minute still over
every link, as the agent does and as it did before it held its target or
sent sound first, and prints how long sound took to arrive. In
`internal/agent/peer`, `TestCongestionControlHoldsThroughAStillScreen`
holds a shaped link still for 20 s (`ROBIN_FEED_WHILE_LIMITED=1` feeds
congestion control as Pion would, to see it fail), and
`TestCongestionControlProbesOnTheWayOut` narrows the link while the screen
is still and checks that the motion after it finds the narrower link.

The docs website in `site/` builds and tests apart from these, with Node and npm: see [site/README.md](site/README.md).

`make test-wayland` boots three desktops, each in its own container with a
virtual monitor, PipeWire and the desktop's own portal: mutter 46 on Ubuntu
24.04, mutter 49 on Fedora 43, and kwin_wayland 6.7 with
xdg-desktop-portal-kde on Fedora 43 (`WLTEST_DESKTOPS` picks fewer). Nobody
sits at those screens, so the harness answers the consent dialog itself:
through mutter's own remote desktop interface on GNOME, and by pressing
Approve through the accessibility bus on KDE, paths only the tests use. It
checks that the first session asks and the second restores without asking,
that frames arrive and change, pointer, keys and held-key repeat, text
typed as keysyms or pasted with the clipboard put back after, the
clipboard both ways in each of its MIME types (text, `text/html` and an
`image/png` bigger than a pipe holds), view-only grants (GNOME only; KDE has none), and that
no `gst-launch-1.0` or portal session outlives the agent, whether it closes
or is killed. A tone played into each session's PipeWire must come out of
the agent's capture through `pipewire-pulse`, and the other way, a tone the
browser's microphone sends must be recorded from the agent's PipeWire
source, and pictures sent as the camera must be read from its node, twice
over, with both nodes gone when the agent closes or is killed. `make
test-x` does the same for the microphone on a spawned desktop's pulseaudio
and on a PulseAudio of the user's own, where a dead agent's leftovers must
be swept. KWin screencasts only when it
composites with OpenGL, so the KDE container borrows the host's GPU render
node, and is skipped, with a line saying so, on a machine without one.

`make e2e` runs four scenarios against a fresh host container each: direct
UDP, UDP blocked on the host from the start, UDP blocked mid-session, and
`--attach auto` against a two-monitor desktop the test user already has
running, where the page has to start on the primary monitor and switch to
the other and to both. A fifth, `nomux`, is the blocked one with
`--ssh-mux=false`, the Windows way, and checks no ControlMaster ran, no ssh
outlives the CLI, and Ctrl+C ends the session with no reconnect announced
and no end command over a fresh login. Each
asserts that decoded frames are not black and change after typing, that
keystrokes after a 20 s pause reach the screen within 400 ms, that typed
text runs in the remote shell, as does text an input method commits in
Chrome, pointer position, clipboard both ways, resize,
that sound played on the host is heard once the speaker is on, which
transport ICE picked, and that nothing survives the CLI; for the attached
desktop that means a resize leaves its screen alone and it outlives the session
with the same processes, autorepeat, keymaps, gamma and sinks. There the
curtain must black out the room's screen (xrandr reads its brightness) while
the page's picture stays lit, and the silent room must keep the room's
speakers quiet while the page still hears the noise; then, kept with
`--keep`, a curtain switch clicked while the page waits out an SSH cut must
reach the host once it is back, a new `robin gui --id --mic` must be
refused for a desktop that has no microphone, and `--id --curtain
--silent` must darken and quiet the room before the page connects.
`rejoin` refuses a `--mic` resume of a spawned desktop the same way.
`direct` and `attach`
also upload files from the status tab and by a drop, twice under one name
so the second lands as `(1)`, offer one from the host (`robin-send` typed
in the spawned xterm, `robin-agent send` over ssh for the attached desktop)
under a name full of markup that the page must show as text and save only
on a click, and paste an image and HTML into the host's clipboard and back.
They check sharp text too: once the screen is still, the page's frame
composed with its tiles matches `xwd -root` on the host in every pixel,
where the video alone does not, also with every monitor shown and the video
scaled down; a trace of every frame shown while typing finds no stale tile,
with the control channel held 300 ms behind the video as well; and actual
size lays the attached monitor out pixel for pixel, pans at the window's
edge and still lands the pointer within 2 px.
`direct` then records a macro whose keys must not reach the xterm while
recording, which a line typed after it proves by arriving alone, checks
the CLI wrote `macros.json` 0600 in a 0700 directory,
reloads the page, finds the macro and plays it into the xterm, sends
Ctrl+W from the Keys row into a shell line, and, in Chrome, types the
local clipboard there with Type clipboard.
A fifth scenario, `wayland`,
runs the same CLI and browser against the headless GNOME session of
`make test-wayland` over sshd: `--attach auto` picks it, the CLI waits while
the harness answers the consent dialog, and Chrome then has to show the
monitor with the remote pointer drawn in, land motion, clicks and typed text
in a probe window, pass the clipboard both ways through the portal, play the
session's PipeWire sound, and meet the same 400 ms latency bound. In
`direct` and `wayland` the CLI offers the microphone and camera, and Chrome
plays a tone and a picture of one colour as its fake devices: off, the host
hears and sees nothing; switched on, the session's default source or its
PipeWire source carries the tone and its camera node the colour; switched
off, silence and black again, and no new offer in between. SIGINT
must leave no agent, `gst-launch-1.0` or portal session and a 0600 restore
token, and a second run must start without asking. The `firefox` and
`webkit` scenarios repeat the direct one in Playwright's own Firefox and
WebKit builds; see the client section above for what each of them proves.
The `touch` scenario runs Chrome as a touch screen: taps and a pinch-zoomed
tap must put the X pointer within 2 px of the pixel under the finger, two
fingers must scroll the xterm back, and the keyboard button's soft keyboard
must type a command that runs.
The `systemd` scenario boots systemd in the host container, with logind
killing what a login started once it ends and the test user lingering, as
on a host with `KillUserProcesses=yes`. A `setsid` process must die with
its login there, while the kept session's daemon has to start as a
`systemd-run --user` unit, outlive the CLI's SIGKILL in it, show in `robin
ls`, be resumed by a new `robin gui` and be gone, unit and run directory,
after `robin end`.
See [e2e/README.md](e2e/README.md). Rootless containers cannot shape bandwidth,
so congestion control is tested separately over a shaped virtual link
(`go test ./internal/agent/peer -run Congestion`).

A private session is tested piece by piece against real servers in
podman, and nowhere near the workstation's own screen or speakers. Xvfb
keeps a 256-entry gamma ramp per CRTC and hands back what was set, though it
shows nothing, so in `make test-x` the curtain has to zero every ramp, darken
it again after a stand-in for night light sets a new one, and on lifting put
that newest ramp back. The silent room there runs against a pulseaudio in the
user's place: a tone played on the desktop must reach the silent output's
monitor while the room's own output hears nothing at all, and play in the
room again once the room is no longer silent, with the default output
unchanged and, with `module-stream-restore` loaded, nothing remembered for
the program. Then an agent with both switched on is killed with SIGKILL, and
its keeper alone has to light the screens, remove the silent output and
move the program back. `make test-wayland` repeats the silent room and the
SIGKILL against each desktop's PipeWire, with `pw-record` on GNOME 46 and the
module everywhere, and reads WirePlumber's state file afterwards for any
output it kept for the program. Mutter's gamma calls are tested against a
stand-in on a private bus, whose signatures the test compares with the real
mutter 46 and 49; headless mutter has no screen to darken, so the session
tests there only see the curtain reported as unavailable, and on KDE as
impossible. In `make e2e` the attach scenario flips both switches from the
page while it watches the host's screen and speakers, and leaves both on
for the teardown, which compares the desktop's gamma and outputs with how
they were before.

Bulk transfers run between two Pion peers in one process, both ways, along
with the ones that must fail: oversized, corrupt, truncated, refused and
cancelled, including a cancel while the sender waits for credit, and a
sink that stops taking chunks while control messages cross beside it,
which must arrive at once (`go test ./internal/agent/peer -run Bulk`). One
goes through the shaped link while video streams, the screen moving and
still, to measure how the two share it. In `make e2e` the direct and
attach scenarios type while a 256 MiB upload moves, and the keys must show
within the 400 ms the still-screen test allows. Uploads have
units of their own in `internal/agent/files`: every name and collision
rule, a symlink in the way, a full disk (writes to `/dev/full`), a disk
that stops (the transfer fails instead of holding the read loop), cancel
and the end of a session, and the offer socket's checks. `go test
./internal/agent` sends good, corrupt, truncated, cancelled and badly named
uploads and fetches an offer over a real peer, and `make test-x` runs them
again through a whole agent session, with the rich clipboard and the X11
targets, INCR both ways, against `xclip`. test-x also runs as root in its
container, which has a second account: a copy of the test binary run as that
account is refused by the 0700 run directory at `ctl.sock` and
`files.sock`, and with the directory opened up by the peer check, before a
byte of what it sent is read; its `robin-agent end`, aimed at another
user's kept session, leaves the session running, and the owner's end takes
no process of that account's for a daemon, whatever its command line. `go test
./internal/web` runs the page's DOM-free modules under `node --test`, from
`internal/web/static/test`, and, with the browser smoke test's dependencies
installed, sends bulk transfers both ways between Chrome and the agent's own
peer, which is where the page's SHA-256 and flow control meet the real
thing. It also drives an input method in desktop Chrome and taps, a long
press, a two-finger scroll, a pinch and the soft keyboard in Chrome as a
phone, and checks what the page sends for each.

The hardware encoders are tested against stand-in tools everywhere, and
against the real GPU wherever `make test` runs on a host that has one along
with a tool to drive it (a GPU with neither ffmpeg nor a GStreamer encoder
plugin skips): every route the host offers must produce a Baseline IDR
first, P slices after, an IDR on request and the target bitrate on noise,
and ffmpeg, where it is installed, must decode the result. Set
`ROBIN_HWENC_REQUIRE=vaapi` (or `nvenc`) to make a missing route fail the run
instead of skipping it, and `ROBIN_HWENC_COMPARE=1` with the openh264 build tag
to print the comparison the ADR quotes.

## Verifying a release

Every binary the build ships is reproducible: the same commit, built from
any checkout path on any Linux x86_64 machine with the pinned toolchain,
gives the same bytes. A shipped `dist/robin-*` can therefore be checked by
anyone with a clone, and so can the agent a CLI uploads, since its name on
the host is the hash of its content.

```sh
git clone https://github.com/yasinmiran/robin && cd robin
git checkout v1.2.3
make dist VERSION=v1.2.3
sha256sum dist/*
```

Compare the sums with the published ones. The build downloads the pinned
zig, openh264 and libopus by hash, and the Makefile exports the Go
toolchain `go.mod` names as `GOTOOLCHAIN`, so `go` fetches and uses that
release whatever is installed; a build by any other Go fails. `make dist`
takes a `VERSION`, or a commit at a release tag: the string is linked into
every binary, so a clone that would describe the commit differently (a
shallow one without the tag, say), or a tree with local changes, which
`git describe` marks `-dirty`, cannot match. The builder's Go environment
is pinned too (`GOENV=off`, `GOFLAGS`, `GOEXPERIMENT`, `GOAMD64`,
`GOARM64`), and the library builds drop `CFLAGS`, `LDFLAGS` and the rest
of the environment's compiler flags, so what is saved with `go env -w` or
exported in a shell does not reach a binary.

`make repro` is the check that keeps this true. It builds the agents, the
CLI and `dist/` from the checkout, then again from a copy of the tree in a
temp directory outside any repository, with its own home, temp directory,
Go build cache and zig, and fails on the first byte that differs, with the
`strings` of both for a start on finding the cause. `REPRO_TOOLCHAIN=1
make repro` also rebuilds openh264 and libopus for the copy, sharing only
the downloads, which checks the C side from source and takes a few minutes
longer; that is the mode a release is verified with. `REPRO_SELFTEST=1
make repro` builds the copy with another `VERSION` and passes only when the
comparison notices, so the check is known not to be vacuous. `make
test-agents`, which `make check` and `make repro` run, builds the agents
and scans them for the checkout, the toolchain cache, the home directory,
Go's own directories, a Go other than the pinned one and any build
setting the Makefile does not set. `make test` runs the same test but
skips it when no agents have been built, so it stays free of the
toolchain.

macOS as a build host is expected to produce the same bytes, since zig
carries its own libc and the Go side is pure, but that has not yet been
verified against a Linux build.

## Known limits

- A guest's camera is never taken: the camera stays the owner's. The
  host has one microphone, and the first page whose microphone sends holds
  it until that page's peer connection closes, so a guest with control is
  heard only while no other page's microphone has started. A guest holds
  it only while it has control: without control its microphone takes no
  turn, and losing control gives the turn up.
- Sharp text follows the owner's page alone; a guest sees the video as it
  is. Every page gets the one stream, encoded at the bitrate the slowest
  page's link allows.
- Guests go when the owner's CLI goes, even from a kept desktop, and a
  guest whose connection drops does not come back on its own: its code is
  spent, so it needs another invitation.
- Every page with control types into one queue, so the owner's keys and a
  guest's interleave as they arrive; nothing stops two people typing at
  once.
- The agent finds an account by name in `/etc/passwd`, then through the
  host's `getent`; an account neither knows cannot be invited. A guest
  whose login lands in another network namespace than the session (a
  container) cannot reach the guests' door, which is an abstract socket.
- Sharing is tested between two accounts of one host in the e2e `share`
  scenario and in test-x; never across a directory service or with a guest
  on a client other than Chrome.
- `--attach auto` never picks an X display inside a Wayland session, because
  the X server you own there is Xwayland, whose screen holds none of the
  desktop. It takes the Wayland session itself when the probe finds its
  socket and session bus.
- Attaching to Wayland needs someone at the host's screen to approve it the
  first time. It works through the portal's remote desktop interface, which
  GNOME and KDE Plasma have; wlroots compositors such as Sway do not.
- On Wayland, a key only types if the host's keyboard layout has its
  character: the compositor drops any other keysym, and there is no spare
  keycode to bind one to as on X11. Text from an input method or a soft
  keyboard goes in as keysyms when it is all ASCII and as a paste with
  Ctrl+V otherwise, so it lands wherever Ctrl+V pastes: not in a terminal,
  which pastes with Ctrl+Shift+V, and only as text the clipboard held is put
  back a second later; an image there is lost. A host layout without Latin
  letters drops the ASCII keysyms too.
- GNOME drops the first key event of each remote session, and now and then
  the first pointer event. The agent spends both on input that does
  nothing, so you should not notice, but it is why mutter logs `No keycode
  found for keyval ffffff` when a session starts.
- An attached Wayland desktop keeps its own size and shows one monitor. A
  screen larger than `--max-size` is scaled down, and a scaled (HiDPI)
  screen streams at its logical size, so text is softer than on the host.
- On Wayland the pointer is drawn into the video, so it moves at the
  video's pace, a round trip behind your hand; the browser hides its own
  pointer over the picture.
- Held keys on Wayland repeat at the host's own rate, and the browser's
  repeats are not sent. A held key during a network stall keeps repeating
  on the host until the key-up gets through.
- The Wayland tests run against GNOME 46, GNOME 49 and KDE Plasma 6.7.
  Other versions and distributions are untested.
- KDE Plasma shares its screen only while KWin composites with OpenGL. With
  KWin's QPainter fallback, used where it finds no GPU to render with, the
  portal refuses to start and the session ends with "Start failed".
- An attached X11 desktop shows one monitor at a time in one browser tab;
  there is no window per monitor. With `all`, every monitor arrives as one
  picture, gaps included, scaled to fit `--max-size`, so small text gets
  hard to read. Monitors come from RandR 1.5; an X server older than that
  shows the whole desktop.
- Sharp text works on X11 desktops only. Wayland's portal hands the agent
  its frames already at 4:2:0, so there the switch is greyed out with the
  reason under it.
- Sharp text needs the browser to report the RTP timestamp of the frame it
  shows, which Chrome, Edge and Firefox do; in a browser that does not, the
  switch is greyed out with the reason under it. Safari is unchecked, since
  the tests cannot see its picture.
- When the message that says which tiles a frame changes is held up behind
  a lost packet, the frame can reach the screen first. The page then hides
  every tile until the message arrives, so the whole picture is plain video
  for that long: a round trip or two.
- A tile that changes again within 2 s of being sent waits twice as long the
  next time, up to 4.8 s, so a blinking caret, a clock or a progress bar
  stays plain video.
- In the fit view the tiles go through the browser's scaling as the video
  does; only actual size shows them pixel for pixel. Actual size pans with
  the pointer at the window's edge, or with two fingers moved apart and
  along on a touch screen, and does not zoom.
- Attached to X11, autorepeat is off for the whole display, keys held at the
  machine itself included, until the session ends.
- Text pasted from the browser into an attached desktop outlives the session
  only if that desktop runs a clipboard manager.
- The curtain hides the host's screen but does not lock it: the keyboard and
  mouse there still reach your session, and when the session ends, for
  whatever reason, the screen comes back unlocked.
- The curtain is tested on Xvfb and against a stand-in for mutter, never on
  a real screen. Some hardware may draw the pointer on a plane its gamma
  does not cover, leaving the pointer visible on the black screen; night
  light or a monitor plugged in shows for up to a second before the curtain
  darkens it again; and GNOME on real hardware is unverified.
- No curtain on KDE Plasma or wlroots compositors, which give programs no
  way to set gamma.
- A program that asks for its stream never to be moved
  (`PA_STREAM_DONT_MOVE`) plays in the room while it is silent; the switch
  names it.
- A program that starts playing while the room is silent plays in the room
  until it is moved. In the tests that took 80 to 180 ms and none of the
  sound reached the room's output, but a program that is loud from its first
  buffer may be heard for that long.
- A program that stops playing while the room is silent is remembered by
  PulseAudio, and by WirePlumber 0.4, as playing on the silent output. It
  plays on the default output next time, as before, but an output chosen
  for it earlier is forgotten.
- Sound in a spawned session needs `pulseaudio` on the host. Current Fedora
  and Ubuntu desktops ship PipeWire without it, and their spawned sessions
  are silent.
- The page starts muted, because browsers autoplay only silent video: one
  click on the speaker, again after each reload.
- A PipeWire camera is invisible to programs that open `/dev/video*`
  themselves, which is most of those not sandboxed; they need v4l2loopback.
  A spawned desktop has no camera without v4l2loopback, which rootless
  containers cannot load, so that route is tested only as far as the probe
  and the pipeline it builds.
- The microphone and camera are tested in Chrome only. Firefox and Safari
  should work the same way through the same page, but nothing checks them.
- Attached to a plain PulseAudio, an agent killed outright leaves its
  microphone loaded until the next robin session of yours sweeps it, and a
  PulseAudio with no default source you chose, on a host with no
  microphone of its own, takes Robin microphone as its default while it is
  there.
- GStreamer's `pipewiresink` in PipeWire 1.4 quits when the last program
  reading the camera lets go. The agent starts it again; the camera is gone
  for under a second meanwhile, and a program that lists cameras in that
  moment misses it.
- The camera is always 1280x720 at 30 fps. The browser lowers its own
  resolution when the link or its CPU are short, and the host then sees a
  softer picture of the same size.
- In-band FEC protects speech only. Opus sends speech in hybrid mode, whose
  SILK layer repeats the previous frame at low rate; music stays in CELT,
  which has no such redundancy, so a lost music packet is concealed by the
  browser instead.
- Attached, the sound is the monitor of whatever output was the default when
  the session started, and the host's own speakers keep playing it unless
  the room is silent (`--silent`).
- A kept desktop survives its connection only where logind leaves user
  processes alone on logout (the default) or a user systemd manager can
  hold it (`KillUserProcesses=yes` with `systemd-run` and, for your last
  login, `loginctl enable-linger`). Elsewhere the CLI warns and the desktop
  ends with the connection. The e2e `systemd` scenario checks the
  systemd-run path with lingering on; without it, where the unit stops
  with the user's last login, only the warning is tested.
- While a kept session has a controller, a second `robin gui` for it is
  refused as busy. A CLI that lost its network keeps the daemon's socket
  busy until the daemon's 20 s idle timeout notices, so a fresh `robin gui`
  started sooner has to retry.
- Ctrl+C while the CLI redials a session the host reports busy leaves the
  session running, since another `robin gui` may be using it. The host
  cannot tell that from this CLI's own lost connection, which keeps the
  session busy for up to 20 s, so Ctrl+C in those seconds leaves the
  desktop too; the CLI says so, and `robin end` ends it.
- Reconnecting keeps the desktop, not the picture: the browser builds a
  fresh peer connection each time, so the first second after a reconnect is
  a new keyframe, anything held down in the page is released, and a
  microphone or camera that was on is off until switched on again.
- The clipboard carries plain text, HTML and PNG images, 16 MiB in all at
  a time; anything else on it (RTF, JPEG, a file manager's file list) stays
  behind. An image bigger than that is left out and its text still goes.
  Copied on X11, `TEXT` alone is not read, since it may come as
  `COMPOUND_TEXT`.
- A single PNG pasted into the page becomes an image on the host's
  clipboard, even when it was a file named `something.png` copied in a file
  manager; any other pasted file is uploaded instead.
- The browser puts what the host copies on its own clipboard only while the
  page has focus; copied while you are in another window, it waits for the
  next copy.
- Recording a macro takes only the keys the browser hands the page:
  outside full screen Ctrl+W, Alt+Tab and the like never reach it, so click
  them in the Keys row while recording instead.
- A macro plays at its own pace: each chord is held 20 ms and steps are
  30 ms apart, with no wait for the desktop beyond the pauses you add, so a
  macro that opens a window needs a pause before it types into it.
- A change to the macros waits up to 2 s for another session's change to
  the file, then gives up and says so; save again.
- Type clipboard needs the browser to hand over the clipboard: Chrome asks
  once, and Firefox and Safari show a Paste button of their own at each
  click, which only Chrome's run of the tests covers. It types 16 KiB at
  most.
- Files and clipboard content from the host go under the video's
  congestion target: what the picture leaves of it, and a quarter of it
  while the picture fills it. On a 3 Mbit/s link a 2 MiB download takes
  about 7 s over a still screen and 30 s while the screen moves, and the
  picture keeps its rate. The target is held while the screen is still, so
  a download started before anything has moved goes at the starting
  2 Mbit/s, and none goes faster than `--max-kbps`, even on a faster link.
  Uploads are not paced, since the page has no target to go by, only held
  to 512 KiB in flight each, which bounds one to about 40 Mbit/s over a
  100 ms round trip. A newer copy on the host cancels the transfer of an
  older one still under way.
- A file is at most 4 GiB either way. The page holds a download in memory
  until the last byte is in, then hands it to the browser's downloads, so
  the browser has to have room for it; uploads stream from disk.
- A file that grows after `robin-send` offered it, a log being written say,
  is refused when you click Save, as larger than the offer, and stays on
  offer as it was; offer it again to get what it holds now. One that shrank
  comes as it is.
- The page does not bound the data channels a host opens and never sends a
  transfer's header on. Each costs the browser a channel and the page a
  listener or two, never a buffer.
- An upload is not flushed to disk (no fsync) before it gets its name, so a
  power cut in the seconds after one can leave it short.
- An agent killed with SIGKILL during an upload leaves the partial file in
  the download folder as `.robin-upload-<random>`, and so does a session
  that ends while the disk under an upload has stopped answering: the agent
  waits 5 s for it and exits. Every other end of a session removes it.
- On a filesystem without `RENAME_NOREPLACE` (some FUSE and network ones),
  uploads are named through a hard link instead, and where that is missing
  too they fail.
- In an attached desktop `robin-send` is not on the `PATH`; run the agent
  itself, `~/.cache/robin/robin-agent-* send FILE` (or wherever the agent
  directory is, see What the host needs). With two sessions on the host it
  has to run inside the one it means.
- Files in Safari are untested: the WebKit the tests drive cannot show the
  picture to click through, so the file and rich clipboard checks skip
  there. Firefox runs them, with the pasted image handed to the page
  directly, since Firefox keeps no script's `DataTransfer` in a synthetic
  paste event.
- Keys map by character, so text arrives right whatever the client layout.
  An input method's text arrives when it is committed: the composition shows
  in the page, not in the remote program, which gets no preedit of its own.
  On X11, committed text with more than 16 characters the keymap lacks
  types at about 16 such characters every 50 ms, while the keycodes it
  borrows settle.
- Touch maps onto one pointer: one finger is the mouse, so panning a zoomed
  picture takes two fingers, and there is no momentum or double tap. A soft
  keyboard has no Ctrl, Alt, Esc or arrow keys to send.
- The Android client has been checked in Chrome's phone emulation only, and
  the Windows client in cross-builds, `go vet` and the `nomux` scenario's
  forced mode on Linux. Neither has run on the real thing; in particular
  the check that ssh holds the forward's port on Windows has run only in
  its Linux form, against `/proc`, and keeping Ctrl+C away from each ssh
  (a process group of its own, where Linux ignores the signal) has not
  run on Windows at all.
- Without ControlMaster a dropped network is noticed only when the agent's
  connection goes quiet for its idle timeout, not at once, and every
  `LocalForward` in your ssh config for that host comes up with the
  tunnel's own ssh; one that fails takes the tunnel with it.
- The status tab reads about 2 fps on an idle screen. A still picture is
  sent again every half second, a few hundred bytes each, so the browser
  never times out waiting for one; that is not a stall.
- Motion after a still screen starts at the bitrate the screen went still
  with, which the agent tries the link with again: if the link narrowed
  meanwhile, that motion overflows it for about a second before congestion
  control comes down (from 3 to 1 Mbit/s in the agent's measurements, in
  1.1 s). Its first second can also show half the frames at a coarser
  quantiser: openh264's rate control, after some still spells, skips every
  other frame for that long at the same target. A session starts at
  2 Mbit/s, and nothing tries the link beyond that until the screen moves,
  so its first motion on a fast link is softer than later motion.
- Below about 4 Mbit/s a busy 1440p picture drops to a few frames a second,
  by design (text stays legible rather than the frame rate high), and
  congestion control can still overshoot the link during video, which
  costs a freeze of up to about a second before a keyframe repairs it.
- Sound goes out ahead of the picture, but the link's own buffer is
  shared: on a 4 Mbit/s link that buffers 100 ms, a keyframe still delays
  sound by up to that much.
- Hardware encoding stays at Constrained Baseline, which every browser
  decodes; High profile measured at most 5% smaller on an Intel iGPU.
- Through the SSH tunnel video is TCP inside TCP, so a lossy link shows
  head-of-line stalls that direct UDP would not.
- The agent asks the kernel, through `sock_diag`, who opened an ICE-TCP
  connection. Where it cannot ask (a sandbox that blocks netlink), it
  refuses every connection to its tunnel listener and the session has no
  TCP path; so it does for one from an IPv6 socket, which sshd never opens
  for the forward's IPv4 address. The SSH server has to make the forward's
  connection as you or as root, as OpenSSH's does.
- The page can still send media on the agent's own video and sound lines,
  which the answer cannot turn down without its own sending; the agent
  reads and drops it, so it costs the link and nothing else.
- Safari's picture is checked by hand only: the WebKit the tests drive on
  Linux renders no remote WebRTC video (see What the client needs).
- Firefox shows no picture until it has fetched its OpenH264 plugin, which
  needs a network connection the first time.
- The macOS client has run on an Apple silicon Mac, in Chrome and in
  Safari, attached to an X11 desktop over Tailscale. Building the agents
  from a Mac is untried.
- One browser per session; a second tab is refused until the first closes.
- On a client shared with other users, the launch link the browser opener
  gets is on a command line they can read. One who opens it before your
  browser does gets the page, and your browser shows Not Found; the CLI
  says the link was opened again. `--no-browser` leaves nothing to read.
- With `--transport tunnel`, a resumed session that was started without it
  still has the agent ask the STUN servers it started with; the page asks
  none.
- `make cover` counts only runs that end normally. A test binary that
  times out or panics, and a CLI or agent that is killed, hand in nothing,
  so a failing e2e scenario lowers the numbers, and without a GPU render
  node the KDE run of test-wayland adds none. The page is measured in
  Chrome and Node only; the Firefox and WebKit scenarios count for the Go
  side.
- Hardware encoding answers a keyframe request about 35 ms later than
  software and adds a few milliseconds to every frame, which is the price on
  the hosts and sizes where `auto` picks it. The NVENC route is untested on
  real hardware; see [Hardware encoding](#hardware-encoding).
