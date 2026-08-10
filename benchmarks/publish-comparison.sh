#!/bin/sh
# Publish the compact same-toolchain Go-library comparison snapshot.
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
go_bin=${TIP_GO:-"$HOME/sdk/vibejson-gotip/bin/go"}
bench_time=${BENCHTIME:-300ms}
sample_count=${COUNT:-6}

if [ ! -x "$go_bin" ]; then
	echo "Go toolchain is not executable: $go_bin" >&2
	exit 1
fi
if [ "${ALLOW_DIRTY:-0}" != 1 ] && [ -n "$(git -C "$repo_root" status --porcelain)" ]; then
	echo "comparison publication requires a clean worktree" >&2
	exit 1
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/vibejson-comparison.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM

cd "$repo_root"
GOWORK=off GOTOOLCHAIN=local GOEXPERIMENT= GOMAXPROCS=1 "$go_bin" test \
	-run '^$' -bench '^BenchmarkNumericDecodePublication$' -benchmem \
	-benchtime="$bench_time" -count="$sample_count" -cpu=1 . >"$work/numeric-portable.txt"

GOWORK=off GOTOOLCHAIN=local GOEXPERIMENT=simd GOMAXPROCS=1 "$go_bin" test \
	-run '^$' -bench '^BenchmarkNumericDecodePublication$' -benchmem \
	-benchtime="$bench_time" -count="$sample_count" -cpu=1 . >"$work/numeric-simd.txt"

cd "$script_dir"
GOWORK=off GOTOOLCHAIN=local GOEXPERIMENT= GOMAXPROCS=1 "$go_bin" test \
	-run '^$' -bench '^BenchmarkComparisonCorpus$' -benchmem \
	-benchtime="$bench_time" -count="$sample_count" -cpu=1 . >"$work/portable.txt"

GOWORK=off GOTOOLCHAIN=local GOEXPERIMENT=simd GOMAXPROCS=1 "$go_bin" test \
	-run '^$' \
	-bench '^BenchmarkComparisonCorpus$/^.*$/^(validate|decode-typed-owned|decode-dynamic-owned|encode-owned)$/^vibejson$' \
	-benchmem -benchtime="$bench_time" -count="$sample_count" -cpu=1 . >"$work/simd.txt"

machine=${BENCH_MACHINE:-}
if [ -z "$machine" ] && [ "$(uname -s)" = Darwin ]; then
	machine=$(/usr/sbin/sysctl -n machdep.cpu.brand_string 2>/dev/null || true)
fi
if [ -z "$machine" ] && [ -r /proc/cpuinfo ]; then
	machine=$(sed -n 's/^model name[[:space:]]*:[[:space:]]*//p' /proc/cpuinfo | sed -n '1p')
fi
machine=${machine:-$(uname -m)}

GOWORK=off GOTOOLCHAIN=local "$go_bin" run ./cmd/benchchart \
	-portable "$work/portable.txt" \
	-simd "$work/simd.txt" \
	-numeric-portable "$work/numeric-portable.txt" \
	-numeric-simd "$work/numeric-simd.txt" \
	-json results/comparison.json \
	-numeric-json results/numeric.json \
	-time-chart charts/go-times.svg \
	-bytes-chart charts/go-allocations.svg \
	-simd-chart charts/simd-validation-times.svg \
	-numeric-chart charts/simd-numeric-times.svg \
	-commit "$(git -C "$repo_root" rev-parse HEAD)" \
	-go-version "$("$go_bin" version)" \
	-machine "$machine" \
	-os "$("$go_bin" env GOOS)" \
	-arch "$("$go_bin" env GOARCH)" \
	-samples "$sample_count" \
	-benchtime "$bench_time"

echo "published benchmarks/results/*.json and benchmarks/charts/*.svg"
