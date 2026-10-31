#!/bin/sh
# Publishes a built site to the gh-pages branch as one orphan commit.
#
#   publish-pages.sh commit <site dir> <git dir> <source commit>
#   publish-pages.sh push <git dir> <remote URL>    (TOKEN in the environment)
#
# The site dir is a build artifact, so git treats it as data only: its git
# dir lives outside it, git reads no config but what this script passes and
# runs no hooks, and a tree holding any .git is refused, since a .git there
# could carry config or hooks that run commands with the token in reach.
set -eu

die() {
	echo "publish-pages: $*" >&2
	exit 1
}

# No system or global config, no hooks, no fsmonitor: nothing outside this
# script decides what git runs.
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
unset GIT_CONFIG_PARAMETERS GIT_CONFIG_COUNT GIT_WORK_TREE GIT_INDEX_FILE
git_isolated() {
	git -c core.hooksPath=/dev/null -c core.fsmonitor= "$@"
}

commit() {
	[ $# -eq 3 ] || die "usage: commit <site dir> <git dir> <source commit>"
	[ -d "$1" ] || die "no site dir $1"
	site=$(cd "$1" && pwd -P)
	[ ! -e "$2" ] && [ ! -L "$2" ] || die "git dir $2 already exists"
	parent=$(cd "$(dirname "$2")" && pwd -P) || die "no parent dir for $2"
	gitdir=$parent/$(basename "$2")
	case $gitdir/ in
	"$site"/*) die "git dir $2 is inside the site" ;;
	esac
	found=$(find "$site" -iname .git -print -quit)
	[ -z "$found" ] || die "refusing a site that holds $found"

	touch "$site/.nojekyll"
	export GIT_DIR="$gitdir" GIT_WORK_TREE="$site"
	git_isolated init -q --template= -b gh-pages
	git_isolated add -A
	git_isolated \
		-c user.name="github-actions[bot]" \
		-c user.email="41898282+github-actions[bot]@users.noreply.github.com" \
		commit -q -m "docs: publish $3"
}

push() {
	[ $# -eq 2 ] || die "usage: push <git dir> <remote URL>"
	[ -n "${TOKEN:-}" ] || die "TOKEN is not set"
	[ -d "$1" ] || die "no git dir $1"
	export GIT_DIR="$1"
	auth=$(printf 'x-access-token:%s' "$TOKEN" | base64 -w0)
	if [ "${GITHUB_ACTIONS:-}" = true ]; then
		echo "::add-mask::$auth"
	fi
	# Through the environment, so the header never shows in the process list.
	export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=http.extraheader \
		GIT_CONFIG_VALUE_0="AUTHORIZATION: basic $auth"
	git_isolated push -q --force "$2" HEAD:gh-pages
}

[ $# -ge 1 ] || die "usage: publish-pages.sh commit|push ..."
cmd=$1
shift
case $cmd in
commit) commit "$@" ;;
push) push "$@" ;;
*) die "unknown command $cmd" ;;
esac
