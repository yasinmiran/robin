#!/bin/sh
# Runs the X-dependent tests of internal/agent, internal/agent/display and
# internal/agent/x11, the sound server tests of internal/agent/audio, the
# session daemon's lifetime tests of cmd/robin-agent, and the tests of
# internal/agent/daemon and internal/agent/files that need a second account
# on the host, inside a container with Xvfb, pulseaudio and that account.
# The tests run as the container's root. Extra arguments go to every test
# binary:
#
#   internal/agent/x11/testdata/xtest.sh
#   internal/agent/x11/testdata/xtest.sh -test.run=Clipboard
#   XTEST_GOFLAGS= internal/agent/x11/testdata/xtest.sh -test.run='^$' -test.bench=Capture
#
# XTEST_GOFLAGS (default -race) goes to go test -c; clear it to benchmark.
# XTEST_IMAGE (default localhost/robin-xtest) names the image it builds.
# XTEST_COVER, a directory, builds the binaries with coverage of the whole
# module and has each write its profile there, as <binary>.out.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../../.." && pwd)
image=${XTEST_IMAGE:-localhost/robin-xtest}
cover=${XTEST_COVER:-}
coverflags=
if [ -n "$cover" ]; then
	mkdir -p "$cover"
	cover=$(cd "$cover" && pwd)
	coverflags="-cover -coverpkg=./..."
fi
out=$(mktemp -d)
trap 'rm -rf "$out"; podman rm -f "robin-xtest-$$" >/dev/null 2>&1 || true' EXIT

# Run by ID: another checkout may retag the image between build and run.
id=$(podman build -q -t "$image" -f "$here/Containerfile" "$here")
cd "$root"
for pkg in display x11 . daemon files; do
	go test ${XTEST_GOFLAGS--race} $coverflags -c -o "$out/$(basename "$(cd "internal/agent/$pkg" && pwd)").test" "./internal/agent/$pkg"
done
# The audio tests encode with libopus, so they link statically with the
# agent's toolchain, where the race detector is not available. The agent
# command's tests run the agent itself, which needs openh264 too.
target=$(uname -m)-linux-musl
zig=$(toolchain/zig.sh)
prefix=$(toolchain/opus.sh "$target")
h264=$(toolchain/openh264.sh "$target")
export ZIG_GLOBAL_CACHE_DIR=${ZIG_GLOBAL_CACHE_DIR:-$root/.cache/toolchain/zig}
CGO_ENABLED=1 CC="$zig cc -target $target" CGO_CFLAGS="-I$prefix/include" CGO_LDFLAGS="-L$prefix/lib" \
	go test -tags opus $coverflags -c -o "$out/audio.test" ./internal/agent/audio
CGO_ENABLED=1 CC="$zig cc -target $target" CXX="$zig c++ -target $target" \
	CGO_CFLAGS="-I$h264/include -I$prefix/include" CGO_LDFLAGS="-L$h264/lib -L$prefix/lib" \
	go test -tags openh264,opus,netgo,osusergo $coverflags -c \
	-ldflags "-linkmode external -extldflags -static" -o "$out/robin-agent.test" ./cmd/robin-agent
podman run --rm --init --name "robin-xtest-$$" -v "$out:/t:ro" ${cover:+-v "$cover:/cover"} -e "XTEST_COVER=${cover:+/cover}" "$id" sh -c '
	cd /tmp
	status=0
	for t in /t/display.test /t/x11.test /t/agent.test /t/audio.test /t/robin-agent.test /t/daemon.test /t/files.test; do
		echo "== $t"
		profile=
		[ -z "$XTEST_COVER" ] || profile=-test.coverprofile=$XTEST_COVER/$(basename "$t" .test).out
		"$t" -test.v -test.timeout=3m $profile "$@" || status=1
	done
	exit $status' sh "$@"
