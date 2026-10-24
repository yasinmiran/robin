# Sourced by the toolchain scripts: pinned inputs and shared helpers.
# shellcheck disable=SC2034 # the variables are read by the sourcing scripts

ZIG_VERSION=0.17.0
NASM_VERSION=2.16.03
NASM_SHA256=1412a1c760bbd05db026b6c0d1657affd6631cd0a63cddb6f73cc6d4aa616148
OPENH264_VERSION=2.6.0
OPENH264_SHA256=558544ad358283a7ab2930d69a9ceddf913f4a51ee9bf1bfb9e377322af81a69
OPUS_VERSION=1.5.2
OPUS_SHA256=65c1d2f78b9f2fb20082c38cbe47c951ad5839345876e46941612ee87f9a7ce1

TOOLCHAIN_DIR=$(cd "$(dirname "$0")" && pwd)
CACHE=$(dirname "$TOOLCHAIN_DIR")/.cache/toolchain

# zig compiles libc++ and musl from source on first use; keep that with the
# rest of the toolchain instead of in ~/.cache/zig.
: "${ZIG_GLOBAL_CACHE_DIR:=$CACHE/zig}"
export ZIG_GLOBAL_CACHE_DIR

# HOST is zig's name for the build machine; HOST_TARGET is what host tools
# are compiled for. Linux host tools target musl so they are static and do
# not depend on the glibc of whichever machine runs the build.
case $(uname -s)-$(uname -m) in
	Linux-x86_64) HOST=x86_64-linux HOST_TARGET=x86_64-linux-musl ;;
	Linux-aarch64 | Linux-arm64) HOST=aarch64-linux HOST_TARGET=aarch64-linux-musl ;;
	Darwin-x86_64) HOST=x86_64-macos HOST_TARGET=x86_64-macos ;;
	Darwin-arm64) HOST=aarch64-macos HOST_TARGET=aarch64-macos ;;
	*) printf 'error: unsupported build host %s %s\n' "$(uname -s)" "$(uname -m)" >&2 && exit 1 ;;
esac

log() { printf '%s\n' "$*" >&2; }
die() { log "error: $*" && exit 1; }

ncpu() { getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4; }

# The flags that keep a library build independent of where the cache is:
# no debug info (it would record the compile directory and zig's own
# headers) and no sanitizer (zig turns UBSan on in a debug build, and it
# records source paths).
FIXED_CFLAGS="-g0 -fno-sanitize=undefined"

# prefix_maps prints the flags that rename every path under the cache, as
# spelled and as resolved, in whatever a __FILE__ still keeps. They name
# this cache, so they stay out of the stamp: a cache shared between
# checkouts is reached by a different spelling from each.
prefix_maps() {
	mkdir -p "$CACHE"
	physical=$(cd "$CACHE" && pwd -P)
	printf -- '-ffile-prefix-map=%s=/robin-toolchain' "$CACHE"
	[ "$physical" = "$CACHE" ] || printf -- ' -ffile-prefix-map=%s=/robin-toolchain' "$physical"
	echo
}

# built STAMP WANT says whether the library STAMP sits beside was built from
# WANT, its version and flags. A library built another way is rebuilt,
# since a cache that outlives a flag change would make this builder's
# agent differ from everyone else's.
built() { [ -f "$1" ] && [ "$(cat "$1")" = "$2" ]; }

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

# fetch URL SHA256 NAME downloads URL into the cache as NAME once, verifies
# it, and prints the local path.
fetch() {
	dest=$CACHE/dl/$3
	if [ ! -f "$dest" ]; then
		mkdir -p "$CACHE/dl"
		log "fetch $1"
		curl -fsSL --retry 3 -o "$dest.part" "$1"
		mv "$dest.part" "$dest"
	fi
	got=$(sha256 "$dest")
	if [ "$got" != "$2" ]; then
		rm -f "$dest"
		die "sha256 mismatch for $1: got $got, want $2"
	fi
	printf '%s\n' "$dest"
}

# unpack ARCHIVE DIR extracts ARCHIVE into a fresh DIR, dropping the
# top-level directory.
unpack() {
	rm -rf "$2"
	mkdir -p "$2"
	tar -xf "$1" -C "$2" --strip-components=1
}
