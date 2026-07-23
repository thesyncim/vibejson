#!/bin/sh
set -eu

go_tip_commit=03845e30f7b73d1703bd8c21017297f6eecb76d6
destination=${1:-"$HOME/sdk/slopjson-gotip"}
bootstrap_go=${BOOTSTRAP_GO:-go}

if [ -x "$destination/bin/go" ] &&
	[ "$(git -C "$destination" rev-parse HEAD 2>/dev/null || true)" = "$go_tip_commit" ]; then
	"$destination/bin/go" version
	exit 0
fi

if [ -e "$destination" ]; then
	printf '%s\n' "destination already exists with a different revision: $destination" >&2
	exit 1
fi

git clone --filter=blob:none --no-checkout https://go.googlesource.com/go "$destination"
git -C "$destination" fetch --depth=1 origin "$go_tip_commit"
git -C "$destination" checkout --detach FETCH_HEAD
bootstrap_goroot=$(GOTOOLCHAIN=local "$bootstrap_go" env GOROOT)

(
	cd "$destination/src"
	GOROOT_BOOTSTRAP="$bootstrap_goroot" ./make.bash
)

"$destination/bin/go" version
