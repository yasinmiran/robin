#!/bin/sh
# Builds static libopus.a and its headers for a target into
# .cache/toolchain/<target>/opus-<key>/{lib,include/opus} and prints that
# directory. The key names every input of the build, so the directory is
# only ever reused for the same bytes.
set -eu
. "$(dirname "$0")/lib.sh"

target=${1:-}
case $target in
	x86_64-linux-musl | aarch64-linux-musl) ;;
	*) die "usage: $0 x86_64-linux-musl|aarch64-linux-musl" ;;
esac

cflags="-O2 $FIXED_CFLAGS"
out=$CACHE/$target/opus-$(library_key opus "$OPUS_VERSION" "$OPUS_SHA256" "$cflags")
if [ ! -d "$out" ]; then
	zig=$("$TOOLCHAIN_DIR/zig.sh")
	src=$(fetch "https://github.com/xiph/opus/releases/download/v$OPUS_VERSION/opus-$OPUS_VERSION.tar.gz" \
		"$OPUS_SHA256" "opus-$OPUS_VERSION.tar.gz")
	build=$(build_dir "opus-$OPUS_VERSION-$target")
	unpack "$src" "$build"
	log "build opus $OPUS_VERSION for $target"
	# --host differs from the build triplet even for x86_64, so configure
	# never tries to run what it compiles.
	(cd "$build" && ./configure --host="$target" --prefix="$out.tmp.$$" \
		--disable-shared --enable-static --disable-doc --disable-extra-programs \
		CC="$zig cc -target $target" AR="$zig ar" RANLIB="$zig ranlib" \
		CFLAGS="$cflags $(prefix_maps "$build")" >&2 &&
		make -j"$(ncpu)" install >&2)
	rm -rf "$build"
	install_prefix "$out.tmp.$$" "$out"
fi
printf '%s\n' "$out"
