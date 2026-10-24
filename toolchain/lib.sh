# Sourced by the toolchain scripts: pinned inputs and shared helpers.
# shellcheck disable=SC2034 # the variables are read by the sourcing scripts

# The builder's environment is not an input. openh264's Makefile appends to
# whatever CFLAGS it inherits, opus's configure reads CPPFLAGS, LDFLAGS and
# LIBS, make passes its own flags and the top make's command-line variables
# down through MAKEFLAGS, and ZIG_LIB_DIR moves zig's libc. Every -j below
# is explicit.
unset CFLAGS CXXFLAGS CPPFLAGS LDFLAGS ASFLAGS LIBS MAKEFLAGS MFLAGS MAKELEVEL ZIG_LIB_DIR ZIG_LOCAL_CACHE_DIR

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

# prefix_maps BUILD prints the flags that rename every path under the cache
# and under the build directory BUILD, as spelled and as resolved, in
# whatever a __FILE__ still keeps. They name this cache, so they stay out of
# the library key: a cache shared between checkouts is reached by a
# different spelling from each.
prefix_maps() {
	mkdir -p "$CACHE"
	physical=$(cd "$CACHE" && pwd -P)
	printf -- '-ffile-prefix-map=%s=/robin-toolchain/build' "$1"
	printf -- ' -ffile-prefix-map=%s=/robin-toolchain' "$CACHE"
	[ "$physical" = "$CACHE" ] || printf -- ' -ffile-prefix-map=%s=/robin-toolchain' "$physical"
	echo
}

# library_key NAME VERSION SOURCE_SHA256 CFLAGS [TOOL...] prints the
# directory under $CACHE/<target> that holds the library built from exactly
# these inputs: the source, the flags, zig, the extra TOOL versions, and
# the text of lib.sh and of the calling script. A library built from
# anything else lives elsewhere, so no builder's agent links a cache that
# outlived a flag or a script change, and a build in one checkout never
# replaces what another checkout links.
library_key() {
	name=$1 version=$2 source=$3 flags=$4
	shift 4
	want="$name $version $source zig $ZIG_VERSION $* $flags $(sha256 "$TOOLCHAIN_DIR/lib.sh") $(sha256 "$0")"
	printf '%s\n' "$want" | sha256 | cut -c1-16
}

# install_prefix TMP OUT moves a library built into TMP to OUT, and keeps
# the one already there if a concurrent build won: both were built from the
# same key, so they are the same bytes.
install_prefix() {
	[ -d "$2" ] || mv "$1" "$2" 2>/dev/null || true
	rm -rf "$1"
	[ -d "$2" ] || die "install into $2 failed"
}

# sha256 [FILE] prints the hex digest of FILE, or of stdin.
sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$@" | cut -d' ' -f1
	else
		shasum -a 256 "$@" | cut -d' ' -f1
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

# build_dir NAME makes a fresh, private directory under the cache for one
# library build and prints it. Its random suffix is renamed away by
# prefix_maps, so two builders' build directories compile alike.
build_dir() {
	mkdir -p "$CACHE/build"
	mktemp -d "$CACHE/build/$1.XXXXXX"
}
