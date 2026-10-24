#!/bin/sh
# Runs the tests of internal/agent/wayland, the agent session test that
# attaches to Wayland and the capture of PipeWire's sound, against headless
# Wayland desktops in rootless podman containers. Extra arguments go to every
# test binary:
#
#   internal/agent/wayland/testdata/wltest.sh
#   internal/agent/wayland/testdata/wltest.sh -test.run=Attach
#   WLTEST_DESKTOPS=gnome49 internal/agent/wayland/testdata/wltest.sh
#
# WLTEST_DESKTOPS (default: all of them) picks the desktops:
#   gnome46  mutter 46 and xdg-desktop-portal-gnome on Ubuntu 24.04 (Containerfile)
#   gnome49  mutter 49 and xdg-desktop-portal-gnome on Fedora 43 (fedora.Containerfile)
#   kde      kwin_wayland 6.7 and xdg-desktop-portal-kde on Fedora 43 (kde.Containerfile);
#            KWin screencasts only with OpenGL, so this one borrows the host's
#            render node and is skipped, saying so, where there is none to open
# WLTEST_GOFLAGS (default -race) goes to go test -c.
# WLTEST_COVER, a directory, builds the binaries with coverage of the whole
# module and has each write its profile there, as <desktop>/<binary>.out.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../../.." && pwd)
desktops=${WLTEST_DESKTOPS:-gnome46 gnome49 kde}
cover=${WLTEST_COVER:-}
coverflags=
if [ -n "$cover" ]; then
	mkdir -p "$cover"
	cover=$(cd "$cover" && pwd)
	coverflags="-cover -coverpkg=./..."
fi
out=$(mktemp -d)
name=
cleanup() {
	[ -z "$name" ] || podman rm -f "$name" >/dev/null 2>&1 || true
	rm -rf "$out"
}
trap cleanup EXIT

cd "$root"
go test ${WLTEST_GOFLAGS--race} $coverflags -c -o "$out/wayland.test" ./internal/agent/wayland
go test ${WLTEST_GOFLAGS--race} $coverflags -c -o "$out/agent.test" ./internal/agent
# The audio test encodes with libopus, so it links statically with the
# agent's toolchain, where the race detector is not available.
target=$(uname -m)-linux-musl
zig=$(toolchain/zig.sh)
prefix=$(toolchain/opus.sh "$target")
ZIG_GLOBAL_CACHE_DIR=${ZIG_GLOBAL_CACHE_DIR:-$root/.cache/toolchain/zig} CGO_ENABLED=1 \
	CC="$zig cc -target $target" CGO_CFLAGS="-I$prefix/include" CGO_LDFLAGS="-L$prefix/lib" \
	go test -tags opus $coverflags -c -o "$out/audio.test" ./internal/agent/audio
cp "$here/probe.py" "$here/session.sh" "$out/"
# The session runs as the image's unprivileged user, not the owner of $out.
chmod -R a+rX "$out"

# run_desktop builds a desktop's image, boots its session and runs every
# test binary against it.
run_desktop() {
	desktop=$1
	shift
	case $desktop in
	gnome46) file=Containerfile image=localhost/robin-wltest ;;
	gnome49) file=fedora.Containerfile image=localhost/robin-wltest-gnome49 ;;
	kde) file=kde.Containerfile image=localhost/robin-wltest-kde ;;
	*)
		echo "wltest.sh: no desktop $desktop" >&2
		return 1
		;;
	esac
	uid=1000 gid=1000 runopts=
	if [ "$desktop" = kde ]; then
		# KWin screencasts only when it composites with OpenGL, which takes a
		# render node. keep-id makes the session user this user, whom the
		# node's permissions admit.
		node=
		for n in /dev/dri/renderD*; do
			if [ -r "$n" ] && [ -w "$n" ]; then
				node=$n
				break
			fi
		done
		if [ -z "$node" ]; then
			echo "==== kde: SKIPPED: no render node $(id -un) can open, and KWin screencasts only with OpenGL"
			return 0
		fi
		uid=$(id -u) gid=$(id -g)
		runopts="--userns=keep-id --user $uid:$gid --device $node -e HOME=/tmp/home"
	fi
	echo "==== $desktop"
	# Run by ID: another checkout may retag the image between build and run.
	id=$(podman build -q -t "$image" -f "$here/$file" "$here")
	name=robin-wltest-$desktop-$$
	profiles=
	if [ -n "$cover" ]; then
		profiles=$cover/$desktop
		mkdir -p "$profiles"
		# The session user is uid 1000 in there, a subordinate uid out here.
		chmod 0777 "$profiles"
	fi
	# shellcheck disable=SC2086 # $runopts holds options without spaces
	podman run -d --name "$name" --init --shm-size=512m $runopts -v "$out:/t:ro" ${profiles:+-v "$profiles:/cover"} \
		"$id" sleep infinity >/dev/null
	# There is no logind or systemd here: mutter falls back to its dummy login
	# manager once /run/systemd/seats is gone, and wants a system bus of some kind.
	podman exec -u root "$name" sh -c "
		rm -rf /run/systemd/seats
		mkdir -p /run/user/$uid /run/dbus /tmp/home
		chown $uid:$gid /run/user/$uid /tmp/home
		chmod 700 /run/user/$uid
		dbus-daemon --config-file=/etc/robin-system-bus.conf --fork"
	podman exec "$name" /t/session.sh

	ok=0
	for t in "wayland.test" "agent.test -test.run=Wayland" "audio.test -test.run=PipeWire"; do
		echo "== $desktop: $t"
		profile=
		[ -z "$cover" ] || profile=-test.coverprofile=/cover/${t%%.test*}.out
		# shellcheck disable=SC2086 # $t is a binary and its own arguments
		podman exec -w /tmp -e XDG_RUNTIME_DIR=/run/user/$uid -e ROBIN_WAYLAND_PROBE=/t/probe.py -e ROBIN_PIPEWIRE_PULSE=1 \
			"$name" /t/$t -test.v -test.timeout=5m $profile "$@" || ok=1
	done
	if [ "$ok" != 0 ]; then
		podman exec "$name" sh -c 'tail -n 20 /tmp/session-logs/*.log' >&2 || true
	fi
	podman rm -f "$name" >/dev/null 2>&1 || true
	name=
	return "$ok"
}

status=0
failed=
for desktop in $desktops; do
	if ! run_desktop "$desktop" "$@"; then
		status=1
		failed="$failed $desktop"
	fi
done
[ -z "$failed" ] || echo "wltest.sh: failed on$failed" >&2
exit "$status"
