#!/bin/sh
# Publish one normalized benchmark record from a clean measurement run.
set -eu

dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH= cd -- "$dir/.." && pwd)
: "${TIP_GO:?set TIP_GO to the pinned Go binary; see benchmarks/README.md}"

dirty=$(git -C "$root" status --porcelain --untracked-files=normal)
if [ -n "$dirty" ]; then
	echo "refusing to publish from a dirty tree" >&2
	exit 1
fi

benchtime=${BENCHTIME:-300ms}
count=${COUNT:-6}
work=$(mktemp -d "${TMPDIR:-/tmp}/simdjson-benchmarks.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM

BENCHTIME="$benchtime" COUNT="$count" TIP_GO="$TIP_GO" \
	"$dir/run-comparison.sh" >"$work/main.txt" 2>&1

(
	cd "$root"
	printf 'benchmark-variant=hooks\n'
	GOTOOLCHAIN=local GOEXPERIMENT=simd "$TIP_GO" test -run='^$' \
		-bench='^(BenchmarkHookDecodeSmall|BenchmarkHookDecodeLarge|BenchmarkHookEncodeSmall|BenchmarkHookEncodeLarge)$' \
		-benchmem -benchtime="$benchtime" -count="$count" -cpu=1 .
) >"$work/hooks.txt" 2>&1

TIP_GO="$TIP_GO" CONTRACT_ONLY=1 "$dir/crosslang/run.sh" >"$work/crosslang.txt" 2>&1

(
	cd "$root"
	GOTOOLCHAIN=local "$TIP_GO" run ./internal/cmd/benchpublish \
		-input "$work" -count "$count" -benchtime "$benchtime" -write
)
