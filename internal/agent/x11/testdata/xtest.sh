#!/bin/sh
# Runs the X-dependent tests of internal/agent, internal/agent/display and
# internal/agent/x11 inside a container with Xvfb. Extra arguments go to every
# test binary:
#
#   internal/agent/x11/testdata/xtest.sh
#   internal/agent/x11/testdata/xtest.sh -test.run=Clipboard
#   XTEST_GOFLAGS= internal/agent/x11/testdata/xtest.sh -test.run='^$' -test.bench=Capture
#
# XTEST_GOFLAGS (default -race) goes to go test -c; clear it to benchmark.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../../.." && pwd)
image=localhost/rj-xtest
out=$(mktemp -d)
trap 'rm -rf "$out"; podman rm -f "rj-xtest-$$" >/dev/null 2>&1 || true' EXIT

podman build -q -t "$image" -f "$here/Containerfile" "$here" >/dev/null
cd "$root"
for pkg in display x11 .; do
	go test ${XTEST_GOFLAGS--race} -c -o "$out/$(basename "$(cd "internal/agent/$pkg" && pwd)").test" "./internal/agent/$pkg"
done
podman run --rm --init --name "rj-xtest-$$" -v "$out:/t:ro" "$image" sh -c '
	cd /tmp
	status=0
	for t in /t/display.test /t/x11.test /t/agent.test; do
		echo "== $t"
		"$t" -test.v -test.timeout=3m "$@" || status=1
	done
	exit $status' sh "$@"
