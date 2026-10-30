#!/bin/sh
# Boots a headless Wayland session as the container user: session bus,
# PipeWire with its PulseAudio server and a null sink for speakers,
# WirePlumber, and the compositor with one virtual monitor: mutter for GNOME,
# or kwin_wayland for KDE Plasma, whichever the image has. The portals start
# on demand through D-Bus activation. Logs go to $LOG.
set -eu
SIZE=${SIZE:-1280x720}
LOG=${LOG:-/tmp/session-logs}
export XDG_RUNTIME_DIR="/run/user/$(id -u)"
export DBUS_SESSION_BUS_ADDRESS="unix:path=$XDG_RUNTIME_DIR/bus"
export XDG_SESSION_TYPE=wayland
if command -v kwin_wayland >/dev/null; then
	compositor=kwin
	export XDG_CURRENT_DESKTOP=KDE XDG_SESSION_DESKTOP=KDE KDE_FULL_SESSION=true QT_QPA_PLATFORM=wayland
	# Test only: Qt shows its widgets to AT-SPI when asked, which is how the
	# harness answers the consent dialog.
	export QT_LINUX_ACCESSIBILITY_ALWAYS_ON=1
	activation="KDE_FULL_SESSION QT_QPA_PLATFORM QT_LINUX_ACCESSIBILITY_ALWAYS_ON"
else
	compositor=mutter
	export XDG_CURRENT_DESKTOP=GNOME XDG_SESSION_DESKTOP=gnome
	activation=
fi

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
pactl load-module module-null-sink sink_name=robin_speakers >/dev/null
pactl set-default-sink robin_speakers
# WLTEST_LAYOUT gives mutter a keyboard of that layout with US after it, as
# GNOME Shell applies input sources: mutter's default plugin takes the
# layout from localed as it starts, and GNOME's settings name the source.
if [ -n "${WLTEST_LAYOUT:-}" ] && [ $compositor = mutter ]; then
	python3 -I "$(dirname "$0")/localed.py" "$WLTEST_LAYOUT,us" >"$LOG/localed.log" 2>&1 &
	i=0
	until grep -q ready "$LOG/localed.log"; do
		i=$((i + 1))
		if [ $i -gt 100 ]; then
			echo "session.sh: localed did not start" >&2
			cat "$LOG/localed.log" >&2
			exit 1
		fi
		sleep 0.1
	done
	gsettings set org.gnome.desktop.input-sources sources "[('xkb', '$WLTEST_LAYOUT'), ('xkb', 'us')]"
fi
case $compositor in
mutter) mutter --headless --wayland --no-x11 --virtual-monitor "$SIZE" >"$LOG/compositor.log" 2>&1 & ;;
kwin) kwin_wayland --virtual --width "${SIZE%x*}" --height "${SIZE#*x}" --no-lockscreen --socket wayland-0 \
	>"$LOG/compositor.log" 2>&1 & ;;
esac
wait_for "$XDG_RUNTIME_DIR/wayland-0" || {
	cat "$LOG/compositor.log" >&2
	exit 1
}
# Activated services, the portal dialogs among them, need the display.
# shellcheck disable=SC2086 # $activation is a list of names
dbus-update-activation-environment WAYLAND_DISPLAY=wayland-0 XDG_CURRENT_DESKTOP XDG_SESSION_TYPE XDG_SESSION_DESKTOP XDG_RUNTIME_DIR $activation
