#!/bin/sh
# Builds the agents, the CLI and dist from this checkout, then again from a
# copy of it at another path with an empty Go build cache, and fails if any
# artifact differs. The copy is the tree as git sees it, tracked and
# untracked files alike, so uncommitted changes are checked too.
#
# The copy shares .cache with the checkout, so the openh264 and libopus
# builds are not repeated; REPRO_TOOLCHAIN=1 gives the copy a cache of its
# own with only the downloads shared, which rebuilds both from source and
# checks them as well. REPRO_DIR moves the copy and the Go cache out of
# .repro.
set -eu
. "$(dirname "$0")/lib.sh"

root=$(dirname "$TOOLCHAIN_DIR")
repro=${REPRO_DIR:-$root/.repro}
# A different depth and length from $root, so an embedded path cannot hide
# behind an equal-sized one.
copy=$repro/other/checkout
targets="agents cli dist"
version=$(cd "$root" && git describe --tags --always --dirty 2>/dev/null || echo dev)

build() {
	log "repro: build in $1"
	make -C "$1" --no-print-directory VERSION="$version" $targets
}

rm -rf "$repro"
mkdir -p "$copy" "$repro/gocache"
(cd "$root" && git ls-files -z --cached --others --exclude-standard | tar -cf - --null -T -) | tar -xf - -C "$copy"

build "$root"

cache=$(cd "$CACHE" && pwd -P)
if [ "${REPRO_TOOLCHAIN:-}" = 1 ]; then
	mkdir -p "$copy/.cache/toolchain"
	ln -s "$cache/dl" "$copy/.cache/toolchain/dl"
else
	ln -s "$(dirname "$cache")" "$copy/.cache"
fi
GOCACHE=$repro/gocache build "$copy"

status=0
cd "$root"
set -- bin/robin-agent-linux-* internal/agentbin/agents/*.gz bin/robin dist/*
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
if [ "$status" = 0 ]; then
	log "repro: every artifact is identical"
	rm -rf "$repro"
else
	log "repro: the copy is kept in $copy"
fi
exit "$status"
