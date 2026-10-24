#!/bin/sh
# Builds the agents, the CLI and dist from this checkout, then again from a
# copy of it at another path, and fails if any artifact differs. The copy is
# the tree as git sees it, tracked and untracked files alike, so uncommitted
# changes are checked too.
#
# The copy gets as little of this build as possible: it sits outside any
# repository, with its own home, temp directory, Go build cache and zig
# (unpacked again, with its libc compiled again), and the downloads and
# the built openh264 and libopus are the only things it shares.
# REPRO_TOOLCHAIN=1 rebuilds those two for the copy as well, sharing only
# the downloads. REPRO_DIR is the directory the copy is made under, by
# default the temp directory. REPRO_SELFTEST=1 builds the copy with another
# VERSION and succeeds only if the comparison sees the difference.
set -eu
. "$(dirname "$0")/lib.sh"

root=$(dirname "$TOOLCHAIN_DIR")
repro=$(mktemp -d "${REPRO_DIR:-${TMPDIR:-/tmp}}/robin-repro.XXXXXX")
# A different depth and length from $root, so an embedded path cannot hide
# behind an equal-sized one.
copy=$repro/other/checkout
targets="agents cli dist"
version=$(cd "$root" && git describe --tags --match 'v[0-9]*' --abbrev=12 --always --dirty 2>/dev/null || echo dev)
other_version=$version
[ "${REPRO_SELFTEST:-}" != 1 ] || other_version="$version-selftest"

build() {
	log "repro: build in $1 (VERSION=$2)"
	make -C "$1" --no-print-directory VERSION="$2" $targets
}

mkdir -p "$copy" "$repro/gocache" "$repro/home" "$repro/tmp"
(cd "$root" && git ls-files -z --cached --others --exclude-standard | tar -cf - --null -T -) | tar -xf - -C "$copy"

build "$root" "$version"
make -C "$root" --no-print-directory test-agents

cache=$(cd "$CACHE" && pwd -P)
mkdir -p "$copy/.cache/toolchain"
ln -s "$cache/dl" "$copy/.cache/toolchain/dl"
if [ "${REPRO_TOOLCHAIN:-}" != 1 ]; then
	for t in "$cache"/*-linux-musl; do
		[ ! -d "$t" ] || ln -s "$t" "$copy/.cache/toolchain/$(basename "$t")"
	done
fi
# The modules are already verified by go.sum, and the Go build cache is
# fresh; everything else of this user's environment stays out.
GIT_CEILING_DIRECTORIES=$repro/other HOME=$repro/home TMPDIR=$repro/tmp \
	GOCACHE=$repro/gocache GOMODCACHE=$(cd "$root" && go env GOMODCACHE) \
	build "$copy" "$other_version"

status=0
cd "$root"
set -- bin/robin-agent-linux-* internal/agentbin/agents/*.gz bin/robin dist/*
log "repro: VERSION=$version"
for f in "$@"; do
	if cmp -s "$f" "$copy/$f"; then
		printf '%s  %s\n' "$(sha256 "$f")" "$f"
		continue
	fi
	status=1
	log "repro: $f differs"
	log "  $(sha256 "$f") $(wc -c <"$f") bytes here"
	log "  $(sha256 "$copy/$f") $(wc -c <"$copy/$f") bytes in the copy"
	if command -v strings >/dev/null 2>&1; then
		strings -a "$f" >"$repro/a.strings"
		strings -a "$copy/$f" >"$repro/b.strings"
		diff "$repro/a.strings" "$repro/b.strings" | head -20 >&2 || true
	fi
done
if [ "${REPRO_SELFTEST:-}" = 1 ]; then
	if [ "$status" = 1 ]; then
		log "repro: self-test passed, the differing VERSION was seen"
		status=0
	else
		log "repro: self-test failed, two different VERSIONs compared equal"
		status=1
	fi
fi
if [ "$status" = 0 ]; then
	rm -rf "$repro"
else
	log "repro: the copy is kept in $copy"
fi
[ "$status" != 0 ] || [ "${REPRO_SELFTEST:-}" = 1 ] || log "repro: every artifact is identical"
exit "$status"
