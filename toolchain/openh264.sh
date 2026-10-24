#!/bin/sh
# Builds static libopenh264.a and its headers for a target into
# .cache/toolchain/<target>/{lib,include/wels} and prints that directory.
set -eu
. "$(dirname "$0")/lib.sh"

target=${1:-}
case $target in
	x86_64-linux-musl) arch=x86_64 ;;
	aarch64-linux-musl) arch=aarch64 ;;
	*) die "usage: $0 x86_64-linux-musl|aarch64-linux-musl" ;;
esac

out=$CACHE/$target
cflags="-O3 $FIXED_CFLAGS"
want="openh264 $OPENH264_VERSION $cflags"
cflags="$cflags $(prefix_maps)"
stamp=$out/lib/libopenh264.a.built
if ! built "$stamp" "$want"; then
	zig=$("$TOOLCHAIN_DIR/zig.sh")
	set --
	if [ "$arch" = x86_64 ]; then
		set -- ASM="$("$TOOLCHAIN_DIR/nasm.sh")"
	fi
	src=$(fetch "https://github.com/cisco/openh264/archive/refs/tags/v$OPENH264_VERSION.tar.gz" \
		"$OPENH264_SHA256" "openh264-$OPENH264_VERSION.tar.gz")
	build=$CACHE/build/openh264-$OPENH264_VERSION-$target
	unpack "$src" "$build"
	# zig reads -march=armv8-a as cpu "armv8" minus feature "a" and rejects it.
	# armv8-a is already the aarch64 baseline, so drop the flag.
	mk=$build/build/platform-gnu-chain.mk
	sed 's/ -march=armv8-a//' "$mk" >"$mk.tmp" && mv "$mk.tmp" "$mk"
	log "build openh264 $OPENH264_VERSION for $target"
	# OS and ARCH default to uname of the build host, so both are always set.
	# install-static-lib builds only the .a; the default target would also
	# link a shared library we never ship. The git ceiling stops openh264's
	# version script from stamping the enclosing repo's HEAD into the library.
	# CFLAGS_OPT is the one variable the Makefile appends to CFLAGS as is,
	# so it carries the reproducibility flags; setting CFLAGS itself would
	# drop every flag the Makefile adds.
	GIT_CEILING_DIRECTORIES=$CACHE/build make -C "$build" -j"$(ncpu)" \
		OS=linux ARCH="$arch" PREFIX="$out" CFLAGS_OPT="$cflags" \
		CC="$zig cc -target $target" CXX="$zig c++ -target $target" AR="$zig ar" \
		"$@" install-static-lib >&2
	printf '%s\n' "$want" >"$stamp"
fi
printf '%s\n' "$out"
