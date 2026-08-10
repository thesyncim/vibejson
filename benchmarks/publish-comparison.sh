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
case "$sample_count" in
	''|*[!0-9]*|0)
		echo "COUNT must be a positive integer: $sample_count" >&2
		exit 1
		;;
esac
if [ "${ALLOW_DIRTY:-0}" != 1 ] && [ -n "$(git -C "$repo_root" status --porcelain)" ]; then
	echo "comparison publication requires a clean worktree" >&2
	exit 1
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/vibejson-comparison.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM

compile_benchmark() (
	cd "$1"
	GOWORK=off GOTOOLCHAIN=local GOEXPERIMENT="$2" "$go_bin" test -c -o "$3" .
)

run_benchmark() {
	GOMAXPROCS=1 "$1" -test.run '^$' -test.bench "$2" -test.benchmem \
		-test.benchtime "$bench_time" -test.cpu 1 >>"$3"
}

numeric_pattern='^BenchmarkNumericDecodePublication$'
comparison_pattern='^BenchmarkComparisonCorpus$'
simd_comparison_pattern='^BenchmarkComparisonCorpus$/^.*$/^(validate|decode-typed-owned|decode-dynamic-owned|encode-owned)$/^vibejson$'

compile_benchmark "$repo_root" '' "$work/numeric-portable.test"
compile_benchmark "$repo_root" simd "$work/numeric-simd.test"
compile_benchmark "$script_dir" '' "$work/comparison-portable.test"
compile_benchmark "$script_dir" simd "$work/comparison-simd.test"

: >"$work/numeric-portable.txt"
: >"$work/numeric-simd.txt"
: >"$work/portable.txt"
: >"$work/simd.txt"

# Alternate each portable/SIMD pair by round so thermal or background drift
# cannot systematically favor the mode that happens to run first.
round=1
while [ "$round" -le "$sample_count" ]; do
	if [ $((round % 2)) -eq 1 ]; then
		run_benchmark "$work/numeric-portable.test" "$numeric_pattern" "$work/numeric-portable.txt"
		run_benchmark "$work/numeric-simd.test" "$numeric_pattern" "$work/numeric-simd.txt"
		run_benchmark "$work/comparison-portable.test" "$comparison_pattern" "$work/portable.txt"
		run_benchmark "$work/comparison-simd.test" "$simd_comparison_pattern" "$work/simd.txt"
	else
		run_benchmark "$work/numeric-simd.test" "$numeric_pattern" "$work/numeric-simd.txt"
		run_benchmark "$work/numeric-portable.test" "$numeric_pattern" "$work/numeric-portable.txt"
		run_benchmark "$work/comparison-simd.test" "$simd_comparison_pattern" "$work/simd.txt"
		run_benchmark "$work/comparison-portable.test" "$comparison_pattern" "$work/portable.txt"
	fi
	round=$((round + 1))
done

machine=${BENCH_MACHINE:-}
if [ -z "$machine" ] && [ "$(uname -s)" = Darwin ]; then
	machine=$(/usr/sbin/sysctl -n machdep.cpu.brand_string 2>/dev/null || true)
fi
if [ -z "$machine" ] && [ -r /proc/cpuinfo ]; then
	machine=$(sed -n 's/^model name[[:space:]]*:[[:space:]]*//p' /proc/cpuinfo | sed -n '1p')
fi
machine=${machine:-$(uname -m)}

cd "$script_dir"
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
