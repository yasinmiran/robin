#!/bin/sh
# Runs the tests of internal/agent/wayland, the agent session test that
# attaches to Wayland and the capture of PipeWire's sound, against a headless
# GNOME session (mutter, PipeWire, xdg-desktop-portal-gnome) in a rootless
# podman container. Extra arguments go to every test binary:
#
#   internal/agent/wayland/testdata/wltest.sh
#   internal/agent/wayland/testdata/wltest.sh -test.run=Attach
#
# WLTEST_GOFLAGS (default -race) goes to go test -c.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../../.." && pwd)
image=localhost/rj-wltest
name=rj-wltest-$$
out=$(mktemp -d)
cleanup() {
	podman rm -f "$name" >/dev/null 2>&1 || true
	rm -rf "$out"
}
trap cleanup EXIT

podman build -q -t "$image" -f "$here/Containerfile" "$here" >/dev/null
cd "$root"
go test ${WLTEST_GOFLAGS--race} -c -o "$out/wayland.test" ./internal/agent/wayland
go test ${WLTEST_GOFLAGS--race} -c -o "$out/agent.test" ./internal/agent
# The audio test encodes with libopus, so it links statically with the
# agent's toolchain, where the race detector is not available.
target=$(uname -m)-linux-musl
zig=$(toolchain/zig.sh)
prefix=$(toolchain/opus.sh "$target")
ZIG_GLOBAL_CACHE_DIR=${ZIG_GLOBAL_CACHE_DIR:-$root/.cache/toolchain/zig} CGO_ENABLED=1 \
	CC="$zig cc -target $target" CGO_CFLAGS="-I$prefix/include" CGO_LDFLAGS="-L$prefix/lib" \
	go test -tags opus -c -o "$out/audio.test" ./internal/agent/audio
cp "$here/probe.py" "$here/session.sh" "$out/"
# The session runs as the image's unprivileged user, not the owner of $out.
chmod -R a+rX "$out"

podman run -d --name "$name" --init --shm-size=512m -v "$out:/t:ro" "$image" sleep infinity >/dev/null
# There is no logind or systemd here: mutter falls back to its dummy login
# manager once /run/systemd/seats is gone, and wants a system bus of some kind.
podman exec -u root "$name" sh -c '
	rm -rf /run/systemd/seats
	mkdir -p /run/user/1000 /run/dbus
	chown 1000:1000 /run/user/1000
	chmod 700 /run/user/1000
	dbus-daemon --config-file=/etc/rj-system-bus.conf --fork'
podman exec "$name" /t/session.sh

status=0
for t in "wayland.test" "agent.test -test.run=Wayland" "audio.test -test.run=PipeWire"; do
	echo "== $t"
	# shellcheck disable=SC2086 # $t is a binary and its own arguments
	podman exec -w /tmp -e XDG_RUNTIME_DIR=/run/user/1000 -e RJ_WAYLAND_PROBE=/t/probe.py -e RJ_PIPEWIRE_PULSE=1 \
		"$name" /t/$t -test.v -test.timeout=5m "$@" || status=1
done
if [ "$status" != 0 ]; then
	podman exec "$name" sh -c 'tail -n 20 /tmp/session-logs/*.log' >&2 || true
fi
exit "$status"
