#!/bin/sh
# Boots a headless GNOME Wayland session as the container user: session bus,
# PipeWire with its PulseAudio server and a null sink for speakers,
# WirePlumber and mutter with one virtual monitor. The portals start
# on demand through D-Bus activation. Logs go to $LOG.
set -eu
SIZE=${SIZE:-1280x720}
LOG=${LOG:-/tmp/session-logs}
export XDG_RUNTIME_DIR="/run/user/$(id -u)"
export DBUS_SESSION_BUS_ADDRESS="unix:path=$XDG_RUNTIME_DIR/bus"
export XDG_CURRENT_DESKTOP=GNOME XDG_SESSION_TYPE=wayland XDG_SESSION_DESKTOP=gnome

wait_for() {
	i=0
	while [ ! -S "$1" ]; do
		i=$((i + 1))
		if [ $i -gt 100 ]; then
			echo "session.sh: $1 did not appear" >&2
			return 1
		fi
		sleep 0.1
	done
}

mkdir -p "$LOG"
dbus-daemon --session --address="$DBUS_SESSION_BUS_ADDRESS" --nofork --nopidfile >"$LOG/dbus.log" 2>&1 &
wait_for "$XDG_RUNTIME_DIR/bus"
pipewire >"$LOG/pipewire.log" 2>&1 &
wait_for "$XDG_RUNTIME_DIR/pipewire-0"
wireplumber >"$LOG/wireplumber.log" 2>&1 &
pipewire-pulse >"$LOG/pipewire-pulse.log" 2>&1 &
wait_for "$XDG_RUNTIME_DIR/pulse/native"
# A desktop has speakers; this one plays into a null sink.
pactl load-module module-null-sink sink_name=rj_speakers >/dev/null
pactl set-default-sink rj_speakers
mutter --headless --wayland --no-x11 --virtual-monitor "$SIZE" >"$LOG/mutter.log" 2>&1 &
wait_for "$XDG_RUNTIME_DIR/wayland-0" || {
	cat "$LOG/mutter.log" >&2
	exit 1
}
# Activated services, the portal dialogs among them, need the display.
dbus-update-activation-environment WAYLAND_DISPLAY=wayland-0 XDG_CURRENT_DESKTOP XDG_SESSION_TYPE XDG_SESSION_DESKTOP XDG_RUNTIME_DIR
