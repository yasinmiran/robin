#!/bin/sh
# Prints the path to the pinned zig for the build host, fetching it on first use.
set -eu
. "$(dirname "$0")/lib.sh"

case $HOST in
	x86_64-linux) sha=1cbe9df9f27e6b78d14ccbca43b6703a404ef79ef1c463de901d7f088d4e2026 ;;
	aarch64-linux) sha=9e8d11661d4ae3bd57702a3832781e23ad151dde5798e16a5ccd503f65234ff8 ;;
	x86_64-macos) sha=4f9a1c5269aa17ebda5e6d3c2b89d6cbf36f7d2b22a0306e9ab98f25f95529c6 ;;
	aarch64-macos) sha=b607e9b9234790a008116ae5bdb71c6243b84b9fb42a53a9e70fde41c06c536a ;;
esac

name=zig-$HOST-$ZIG_VERSION
dir=$CACHE/host/$name
if [ ! -x "$dir/zig" ]; then
	tarball=$(fetch "https://ziglang.org/download/$ZIG_VERSION/$name.tar.xz" "$sha" "$name.tar.xz")
	unpack "$tarball" "$dir.tmp"
	rm -rf "$dir"
	mv "$dir.tmp" "$dir"
fi
printf '%s\n' "$dir/zig"
