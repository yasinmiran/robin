#!/bin/sh
# Runs the tests of internal/agent/wayland against a headless GNOME session
# (mutter, PipeWire, xdg-desktop-portal-gnome) in a rootless podman
# container. Extra arguments go to the test binary:
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
(cd "$root" && go test ${WLTEST_GOFLAGS--race} -c -o "$out/wayland.test" ./internal/agent/wayland)
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
podman exec -w /tmp -e XDG_RUNTIME_DIR=/run/user/1000 -e RJ_WAYLAND_PROBE=/t/probe.py \
	"$name" /t/wayland.test -test.v -test.timeout=5m "$@" || status=$?
if [ "$status" != 0 ]; then
	podman exec "$name" sh -c 'tail -n 20 /tmp/session-logs/*.log' >&2 || true
fi
exit "$status"
