# Sourced by the toolchain scripts: pinned inputs and shared helpers.
# shellcheck disable=SC2034 # the variables are read by the sourcing scripts

ZIG_VERSION=0.17.0
NASM_VERSION=2.16.03
NASM_SHA256=1412a1c760bbd05db026b6c0d1657affd6631cd0a63cddb6f73cc6d4aa616148
OPENH264_VERSION=2.6.0
OPENH264_SHA256=558544ad358283a7ab2930d69a9ceddf913f4a51ee9bf1bfb9e377322af81a69

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
