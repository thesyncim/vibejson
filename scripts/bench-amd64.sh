#!/bin/sh
# Interleaved native amd64 evidence. Shared hosted-runner timings are directional;
# use a dedicated runner before treating small differences as a regression gate.
set -eu

go_bin=${1:?usage: bench-amd64.sh go baseline output-directory}
baseline=${2:?baseline ref required}
results=${3:?output directory required}
rounds=${BENCHCOUNT:-6}
benchtime=${BENCHTIME:-200ms}
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
export GOWORK=off GOTOOLCHAIN=local GOEXPERIMENT=simd GOMAXPROCS=1
export GOAMD64=${GOAMD64:-v3}
if [ "$("$go_bin" env GOHOSTOS)/$("$go_bin" env GOHOSTARCH)" != linux/amd64 ]; then
 echo 'Native Linux/amd64 required; emulated results must not be published as native.' >&2
 exit 1
fi
baseline_commit=$(git rev-parse --verify "$baseline^{commit}")
work=$(mktemp -d "${TMPDIR:-/tmp}/vibejson-amd64.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM
mkdir -p "$work/baseline" "$results"
git archive "$baseline_commit" | tar -x -C "$work/baseline"
# Identical test-only fixtures let the base revision measure new workloads.
cp field_hash_contract_test.go "$work/baseline/"
cp x/kernels/stage1_stream_test.go "$work/baseline/x/kernels/"
{
 "$go_bin" version
 uname -a
 lscpu
 printf 'baseline=%s\nhead=%s\nGOAMD64=%s\nGOEXPERIMENT=%s\nrounds=%s\nbenchtime=%s\n' "$baseline_commit" "$(git rev-parse HEAD)" "$GOAMD64" "$GOEXPERIMENT" "$rounds" "$benchtime"
} > "$results/environment.txt"
for mode in old new; do
 directory=$root
 if [ "$mode" = old ]; then directory=$work/baseline; fi
 (cd "$directory" && "$go_bin" test -c -o "$work/$mode-root.test" .)
 (cd "$directory" && "$go_bin" test -c -o "$work/$mode-kernels.test" ./x/kernels)
done
"$work/new-root.test" -test.short
VIBEJSON_REQUIRE_AVX2=1 "$work/new-kernels.test"
root_pattern='^(BenchmarkDecodeUint64Array16|BenchmarkDecodeSharedPrefixFields|BenchmarkDecodeLargeReused|BenchmarkDecodeLargeShuffledKeys|BenchmarkEncodeLarge|BenchmarkValidLarge|BenchmarkBuildIndexLarge|BenchmarkNumericDecodePublication|BenchmarkHookDecodeSmall|BenchmarkHookDecodeLarge)$'
kernel_pattern='^(BenchmarkStage1Block|BenchmarkStage1Chunk32)$'
for mode in old new; do
 : > "$results/$mode-root.txt"
 : > "$results/$mode-kernels.txt"
done
round=0
while [ "$round" -lt "$rounds" ]; do
 order='old new'
 if [ $((round % 2)) -ne 0 ]; then order='new old'; fi
 for mode in $order; do
  "$work/$mode-root.test" -test.run '^$' -test.bench "$root_pattern" -test.benchmem -test.benchtime "$benchtime" -test.cpu 1 >> "$results/$mode-root.txt"
  "$work/$mode-kernels.test" -test.run '^$' -test.bench "$kernel_pattern" -test.benchmem -test.benchtime "$benchtime" -test.cpu 1 >> "$results/$mode-kernels.txt"
 done
 round=$((round+1))
done
"${BENCHSTAT:-benchstat}" "$results/old-root.txt" "$results/new-root.txt" | tee "$results/root-comparison.txt"
"${BENCHSTAT:-benchstat}" "$results/old-kernels.txt" "$results/new-kernels.txt" | tee "$results/kernel-comparison.txt"
