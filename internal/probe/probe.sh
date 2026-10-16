#!/bin/sh
# Reports what this host offers remotejump as key=value lines on stdout.
#
#   sh -s -- <agent file name>... < probe.sh
#
# Runs unprivileged on dash, bash and busybox ash. Every optional tool is
# guarded, so a missing one only leaves its keys out.

kv() {
	printf '%s=%s\n' "$1" "$2"
}

have() {
	command -v "$1" >/dev/null 2>&1
}

# limit runs a command that can hang, killing it after two seconds when
# timeout(1) is usable.
limit() {
	if [ -n "$timeout_ok" ]; then
		timeout 2 "$@"
	else
		"$@"
	fi
}

# lsfields sets st_mode and st_uid from lstat(2) of $1, so a symlink reports
# itself and type, owner and mode come from one atomic look.
lsfields() {
	st_mode='' st_uid=''
	ls_out=$(ls -ldn "$1" 2>/dev/null) || return 1
	set -f
	# shellcheck disable=SC2086 # split into ls columns
	set -- $ls_out
	set +f
	st_mode=$1 st_uid=$3
}

trim_leading() {
	trimmed=${1#"${1%%[! 	]*}"}
}

os_release() {
	f=/etc/os-release
	[ -r "$f" ] || f=/usr/lib/os-release
	[ -r "$f" ] || return
	while IFS= read -r line || [ -n "$line" ]; do
		case $line in
		ID=*) key=distro_id ;;
		VERSION_ID=*) key=distro_version ;;
		ID_LIKE=*) key=distro_like ;;
		PRETTY_NAME=*) key=distro_name ;;
		*) continue ;;
		esac
		v=${line#*=}
		case $v in
		\"*\") v=${v#\"} v=${v%\"} ;;
		\'*\') v=${v#\'} v=${v%\'} ;;
		esac
		kv "$key" "$v"
	done <"$f"
}

cpus() {
	if have nproc; then
		n=$(nproc 2>/dev/null)
	elif have getconf; then
		n=$(getconf _NPROCESSORS_ONLN 2>/dev/null)
	else
		n=0
		while IFS= read -r line; do
			case $line in processor*) n=$((n + 1)) ;; esac
		done </proc/cpuinfo
	fi
	kv cpus "$n"
}

memory() {
	[ -r /proc/meminfo ] || return
	while read -r k v _; do
		if [ "$k" = MemTotal: ]; then
			kv memory_kb "$v"
			return
		fi
	done </proc/meminfo
}

# sessions reports this user's logind sessions, all fetched in one
# show-session call because a shared host can have hundreds.
sessions() {
	have loginctl || return
	ids=$(limit loginctl list-sessions --no-legend --no-pager </dev/null 2>/dev/null |
		while read -r sid suid _; do
			[ "$suid" = "$uid" ] && printf '%s ' "$sid"
		done)
	[ -n "$ids" ] || return
	# shellcheck disable=SC2086 # ids is a list of session ids
	limit loginctl show-session $ids --no-pager -p Id -p Name -p Type -p State \
		-p Active -p Remote -p Display -p Desktop -p Class </dev/null 2>/dev/null |
		{
			id='' rest=''
			while :; do
				IFS= read -r line
				eof=$?
				case $line in
				Id=*) id=${line#Id=} ;;
				Name=*) rest="${rest}session_name=${line#*=}
" ;;
				Type=*) rest="${rest}session_type=${line#*=}
" ;;
				State=*) rest="${rest}session_state=${line#*=}
" ;;
				Active=*) rest="${rest}session_active=${line#*=}
" ;;
				Remote=*) rest="${rest}session_remote=${line#*=}
" ;;
				Display=*) rest="${rest}session_display=${line#*=}
" ;;
				Desktop=*) rest="${rest}session_desktop=${line#*=}
" ;;
				Class=*) rest="${rest}session_class=${line#*=}
" ;;
				esac
				# A blank line ends one session's block; EOF ends the last.
				if [ -z "$line" ] || [ "$eof" != 0 ]; then
					if [ -n "$id" ]; then
						kv session "$id"
						printf '%s' "$rest"
					fi
					id='' rest=''
				fi
				[ "$eof" = 0 ] || break
			done
		}
}

displays() {
	for s in /tmp/.X11-unix/X*; do
		[ -S "$s" ] || continue
		kv x11 ":${s##*/X}"
		lsfields "$s"
		kv x11_uid "$st_uid"
		owner=$(ls -ld "$s" 2>/dev/null) && {
			set -f
			# shellcheck disable=SC2086 # split into ls columns
			set -- $owner
			set +f
			kv x11_owner "$3"
		}
	done
	for s in "${XDG_RUNTIME_DIR:-/run/user/$uid}"/wayland-*; do
		[ -S "$s" ] && kv wayland "$s"
	done
}

gpu() {
	render=
	for n in /dev/dri/card* /dev/dri/renderD*; do
		[ -c "$n" ] || continue
		kv dri "$n"
		# Open it for real: some shells' test -r/-w judge by mode bits alone and
		# miss the ACL logind grants the seat's user.
		if (: <>"$n") 2>/dev/null; then
			kv dri_rw yes
			case $n in */renderD*) [ -n "$render" ] || render=$n ;; esac
		else
			kv dri_rw no
		fi
	done

	if have vainfo; then
		kv vainfo "$(command -v vainfo)"
		if [ -n "$render" ]; then
			limit vainfo --display drm --device "$render" </dev/null 2>&1 | {
				h264=no
				while IFS= read -r line; do
					case $line in
					*"Driver version:"*)
						trim_leading "${line#*Driver version:}"
						kv vainfo_driver "$trimmed"
						;;
					*VAProfileH264*VAEntrypointEncSlice*) h264=yes ;;
					esac
				done
				kv vainfo_h264_encode "$h264"
			}
		fi
	fi

	have nvidia-smi && kv nvidia_smi "$(command -v nvidia-smi)"
	# The loaded driver names its GPUs here; nvidia-smi can take seconds.
	for f in /proc/driver/nvidia/gpus/*/information; do
		[ -r "$f" ] || continue
		while IFS= read -r line; do
			case $line in
			Model:*)
				trim_leading "${line#Model:}"
				kv nvidia_gpu "$trimmed"
				break
				;;
			esac
		done <"$f"
	done
}

commands() {
	for c in Xvfb xkbcomp \
		xfce4-session startxfce4 openbox-session openbox fluxbox icewm-session twm \
		xterm x-terminal-emulator xfce4-terminal \
		dbus-run-session dbus-launch; do
		p=$(command -v "$c" 2>/dev/null) && kv "cmd.$c" "$p"
	done
}

reject() {
	kv agentdir_rejected "$1:$2"
}

# try_dir accepts $1 as the agent directory only if nobody else can write to
# it and files in it can be executed. $2 set means create missing parents.
try_dir() {
	d=$1
	if [ ! -e "$d" ] && [ ! -L "$d" ]; then
		[ -n "$2" ] && mkdir -p "${d%/*}" 2>/dev/null
		# Fails if someone creates it first, so a racing /tmp squatter loses.
		mkdir -m 700 "$d" 2>/dev/null || {
			reject "$d" mkdir
			return 1
		}
	fi
	lsfields "$d" || {
		reject "$d" stat
		return 1
	}
	case $st_mode in
	d*) ;;
	l*)
		reject "$d" symlink
		return 1
		;;
	*)
		reject "$d" notdir
		return 1
		;;
	esac
	if [ "$st_uid" != "$uid" ]; then
		reject "$d" owner
		return 1
	fi
	case $st_mode in
	?????w* | ????????w*)
		reject "$d" perms
		return 1
		;;
	esac
	t=$d/.probe-$$
	if ! printf '#!/bin/sh\nexit 42\n' 2>/dev/null >"$t"; then
		rm -f "$t"
		reject "$d" unwritable
		return 1
	fi
	chmod 700 "$t" 2>/dev/null
	"$t" </dev/null >/dev/null 2>&1
	rc=$?
	rm -f "$t"
	if [ "$rc" != 42 ]; then
		reject "$d" noexec
		return 1
	fi
	agentdir=$d
	kv agentdir "$d"
}

pick_agentdir() {
	agentdir=
	cache=${XDG_CACHE_HOME:-}
	[ -z "$cache" ] && [ -n "${HOME:-}" ] && cache=$HOME/.cache
	[ -n "$cache" ] && try_dir "$cache/remotejump" parents && return
	[ -n "${XDG_RUNTIME_DIR:-}" ] && try_dir "$XDG_RUNTIME_DIR/remotejump" "" && return
	try_dir "/tmp/remotejump-$uid" ""
}

sha256() {
	case $sha_tool in
	sha256sum) sum=$(sha256sum 2>/dev/null <"$1") ;;
	shasum) sum=$(shasum -a 256 2>/dev/null <"$1") ;;
	busybox) sum=$(busybox sha256sum 2>/dev/null <"$1") ;;
	openssl)
		sum=$(openssl dgst -sha256 2>/dev/null <"$1")
		sum=${sum##* }
		;;
	esac
	sum=${sum%% *}
}

agents() {
	sha_tool=none
	if have sha256sum; then
		sha_tool=sha256sum
	elif have shasum; then
		sha_tool=shasum
	elif have openssl; then
		sha_tool=openssl
	elif have busybox && busybox sha256sum </dev/null >/dev/null 2>&1; then
		sha_tool=busybox
	fi
	kv sha256_tool "$sha_tool"
	if [ -z "$agentdir" ] || [ "$sha_tool" = none ]; then
		return
	fi
	for name in "$@"; do
		case $name in '' | . | .. | */*) continue ;; esac
		[ -f "$agentdir/$name" ] || continue
		sha256 "$agentdir/$name"
		[ -n "$sum" ] && kv agent_sha256 "$name $sum"
	done
}

main() {
	# The script arrives on stdin; keep every child off it.
	exec </dev/null

	kv probe 1
	timeout_ok=
	have timeout && timeout 2 true 2>/dev/null && timeout_ok=1
	uid=$(id -u 2>/dev/null)

	kv os "$(uname -s 2>/dev/null)"
	kv kernel "$(uname -r 2>/dev/null)"
	kv machine "$(uname -m 2>/dev/null)"
	os_release
	kv user "$(id -un 2>/dev/null)"
	kv uid "$uid"
	kv home "${HOME:-}"
	cpus
	memory

	kv env.XDG_SESSION_TYPE "${XDG_SESSION_TYPE:-}"
	kv env.DISPLAY "${DISPLAY:-}"
	kv env.WAYLAND_DISPLAY "${WAYLAND_DISPLAY:-}"
	kv env.XDG_CURRENT_DESKTOP "${XDG_CURRENT_DESKTOP:-}"
	kv env.XDG_RUNTIME_DIR "${XDG_RUNTIME_DIR:-}"

	sessions
	displays
	gpu
	commands
	pick_agentdir
	agents "$@"
}

main "$@"
