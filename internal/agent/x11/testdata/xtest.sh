#!/bin/sh
# Runs the X-dependent tests of internal/agent, internal/agent/display and
# internal/agent/x11, and the sound server tests of internal/agent/audio,
# inside a container with Xvfb and pulseaudio. Extra arguments go to every
# test binary:
#
#   internal/agent/x11/testdata/xtest.sh
#   internal/agent/x11/testdata/xtest.sh -test.run=Clipboard
#   XTEST_GOFLAGS= internal/agent/x11/testdata/xtest.sh -test.run='^$' -test.bench=Capture
#
# XTEST_GOFLAGS (default -race) goes to go test -c; clear it to benchmark.
# XTEST_IMAGE (default localhost/rj-xtest) names the image it builds.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../../.." && pwd)
image=${XTEST_IMAGE:-localhost/rj-xtest}
out=$(mktemp -d)
trap 'rm -rf "$out"; podman rm -f "rj-xtest-$$" >/dev/null 2>&1 || true' EXIT

# Run by ID: another checkout may retag the image between build and run.
id=$(podman build -q -t "$image" -f "$here/Containerfile" "$here")
cd "$root"
for pkg in display x11 .; do
	go test ${XTEST_GOFLAGS--race} -c -o "$out/$(basename "$(cd "internal/agent/$pkg" && pwd)").test" "./internal/agent/$pkg"
done
# The audio tests encode with libopus, so they link statically with the
# agent's toolchain, where the race detector is not available.
target=$(uname -m)-linux-musl
zig=$(toolchain/zig.sh)
prefix=$(toolchain/opus.sh "$target")
ZIG_GLOBAL_CACHE_DIR=${ZIG_GLOBAL_CACHE_DIR:-$root/.cache/toolchain/zig} CGO_ENABLED=1 \
	CC="$zig cc -target $target" CGO_CFLAGS="-I$prefix/include" CGO_LDFLAGS="-L$prefix/lib" \
	go test -tags opus -c -o "$out/audio.test" ./internal/agent/audio
podman run --rm --init --name "rj-xtest-$$" -v "$out:/t:ro" "$id" sh -c '
	cd /tmp
	status=0
	for t in /t/display.test /t/x11.test /t/agent.test /t/audio.test; do
		echo "== $t"
		"$t" -test.v -test.timeout=3m "$@" || status=1
	done
	exit $status' sh "$@"
