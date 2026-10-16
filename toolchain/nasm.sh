#!/bin/sh
# Prints the path to nasm built from pinned source for the build host.
# Only x86_64 targets need it: openh264's x86 asm is nasm syntax.
set -eu
. "$(dirname "$0")/lib.sh"

prefix=$CACHE/host/nasm-$NASM_VERSION-$HOST
if [ ! -x "$prefix/bin/nasm" ]; then
	zig=$("$TOOLCHAIN_DIR/zig.sh")
	src=$(fetch "https://www.nasm.us/pub/nasm/releasebuilds/$NASM_VERSION/nasm-$NASM_VERSION.tar.xz" \
		"$NASM_SHA256" "nasm-$NASM_VERSION.tar.xz")
	build=$CACHE/build/nasm-$NASM_VERSION-$HOST
	unpack "$src" "$build"
	log "build nasm $NASM_VERSION for $HOST_TARGET"
	(
		cd "$build"
		# zig cc turns -Wdate-time into an error; nasm embeds __DATE__.
		./configure CC="$zig cc -target $HOST_TARGET" AR="$zig ar" RANLIB="$zig ranlib" \
			CFLAGS="-O2 -Wno-error=date-time"
		make -j"$(ncpu)" nasm
	) >&2
	mkdir -p "$prefix/bin"
	cp "$build/nasm" "$prefix/bin/nasm"
fi
printf '%s\n' "$prefix/bin/nasm"
