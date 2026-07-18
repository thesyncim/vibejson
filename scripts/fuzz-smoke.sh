#!/bin/sh
# Discover every Go fuzz target in every package and run one deterministic
# shard. GOEXPERIMENT, GOFLAGS, and other build settings are inherited, so the
# same driver covers portable, SIMD, and tagged integrity builds.
set -eu

go_bin=${1:-go}
fuzz_time=${2:-${FUZZ_TIME:-1000x}}
shard_index=${3:-${FUZZ_SHARD_INDEX:-0}}
shard_count=${4:-${FUZZ_SHARD_COUNT:-1}}
parallel=${FUZZ_PARALLEL:-4}

case $shard_index:$shard_count:$parallel in
*[!0-9:]* | :* | *::* | *:0:* | *:0)
	echo "invalid fuzz shard or parallel setting: index=$shard_index count=$shard_count parallel=$parallel" >&2
	exit 2
	;;
esac
if [ "$shard_index" -ge "$shard_count" ]; then
	echo "fuzz shard index $shard_index is outside shard count $shard_count" >&2
	exit 2
fi
if [ ! -x "$go_bin" ]; then
	echo "Go toolchain is not executable: $go_bin" >&2
	exit 1
fi

targets=$(mktemp "${TMPDIR:-/tmp}/simdjson-fuzz-targets.XXXXXX")
trap 'rm -f "$targets"' EXIT HUP INT TERM

for package in $("$go_bin" list ./...); do
	"$go_bin" test -list '^Fuzz' "$package" |
		awk -v package="$package" '/^Fuzz[[:alnum:]_]+$/ { print package, $0 }' >>"$targets"
done

total=$(wc -l <"$targets" | tr -d ' ')
if [ "$total" -eq 0 ]; then
	echo "no fuzz targets discovered" >&2
	exit 1
fi

selected=0
ordinal=0
while read -r package target; do
	if [ $((ordinal % shard_count)) -eq "$shard_index" ]; then
		selected=$((selected + 1))
		echo "fuzz [$selected] $package $target ($fuzz_time)"
		"$go_bin" test -run '^$' -fuzz "^${target}$" -fuzztime "$fuzz_time" -parallel "$parallel" "$package"
	fi
	ordinal=$((ordinal + 1))
done <"$targets"

if [ "$selected" -eq 0 ]; then
	echo "fuzz shard $shard_index/$shard_count selected no targets out of $total" >&2
	exit 1
fi
echo "fuzz shard $shard_index/$shard_count passed $selected of $total discovered targets"
