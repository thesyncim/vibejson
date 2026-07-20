#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
: "${TIP_GO:?set TIP_GO to the pinned Go binary; see benchmarks/README.md}"
tip_go=$TIP_GO
legacy_go=${LEGACY_GO:-go}
legacy_toolchain=${LEGACY_GOTOOLCHAIN:-go1.26.4}
benchtime=${BENCHTIME:-300ms}
count=${COUNT:-6}
jsonv2_bench=${JSONV2_BENCH:-^BenchmarkParseTypedJSONV2}

repo=$(CDPATH= cd -- "$root/.." && pwd)
commit=$(git -C "$repo" rev-parse HEAD)
dirty=$(git -C "$repo" status --porcelain --untracked-files=normal)
if [ -n "$dirty" ] && [ "${ALLOW_DIRTY:-0}" != 1 ]; then
	echo "refusing to publish from a dirty tree; commit the candidate or set ALLOW_DIRTY=1 for a development run" >&2
	exit 1
fi
printf 'repository commit=%s dirty=%s\n' "$commit" "$([ -n "$dirty" ] && printf true || printf false)"
"$tip_go" version
printf 'go-commit=%s\n' "$(git -C "$("$tip_go" env GOROOT)" rev-parse HEAD)"
printf 'legacy-go='
GOTOOLCHAIN="$legacy_toolchain" "$legacy_go" version

# BENCH keeps the broad exploratory runner available. The default publication
# path below isolates every corpus contract in a fresh process so allocator and
# GC state from dynamic decoding cannot perturb later DOM or encode groups.
if [ -n "${BENCH:-}" ]; then
	bench=$BENCH
	printf '\nGo tip pure Go exploratory benchmarks\n'
	(
		cd "$root"
		GOEXPERIMENT=nosimd "$tip_go" test -run='^$' -bench="$bench" -benchmem -benchtime="$benchtime" -count="$count"
	)

	printf '\nGo tip SIMD exploratory benchmarks\n'
	(
		cd "$root"
		GOEXPERIMENT=simd "$tip_go" test -run='^$' -bench="$bench" -benchmem -benchtime="$benchtime" -count="$count"
	)

	printf '\nGo tip encoding/json/v2 exploratory benchmarks\n'
	(
		cd "$root"
		GOEXPERIMENT=simd,jsonv2 "$tip_go" test -run='^$' -bench="$jsonv2_bench" -benchmem -benchtime="$benchtime" -count="$count"
	)

	printf '\nGo 1.26 native compatibility exploratory benchmarks\n'
	(
		cd "$root/legacy"
		GOTOOLCHAIN="$legacy_toolchain" "$legacy_go" test -run='^$' -bench="$bench" -benchmem -benchtime="$benchtime" -count="$count"
	)
	exit 0
fi

printf '\nGo tip SIMD exact corpus, one process per contract\n'
printf 'benchmark-variant=simd\n'
for group in valid dynamic-owned dom typed-reused encode; do
(
	cd "$root"
	GOEXPERIMENT=simd "$tip_go" test -run='^$' \
		-bench="^BenchmarkStdlibCorpus$/^.*$/${group}$" \
		-benchmem -benchtime="$benchtime" -count="$count" -cpu=1
)
done

printf '\nGo tip pure Go exact corpus, one process per contract\n'
printf 'benchmark-variant=pure\n'
for group in valid dynamic-owned dom typed-reused encode; do
(
	cd "$root"
	GOEXPERIMENT=nosimd "$tip_go" test -run='^$' \
		-bench="^BenchmarkStdlibCorpus$/^.*$/${group}$" \
		-benchmem -benchtime="$benchtime" -count="$count" -cpu=1
)
done

printf '\nReusable native parsers, SIMD and pure Go\n'
(
	cd "$root"
	printf 'benchmark-variant=index-simd\n'
	GOEXPERIMENT=simd "$tip_go" test -run='^$' \
		-bench='^BenchmarkStdlibCorpusNativeParse$' \
		-benchmem -benchtime="$benchtime" -count="$count" -cpu=1
	printf 'benchmark-variant=index-pure\n'
	GOEXPERIMENT=nosimd "$tip_go" test -run='^$' \
		-bench='^BenchmarkStdlibCorpusNativeParse$' \
		-benchmem -benchtime="$benchtime" -count="$count" -cpu=1
)

printf '\nGo tip encoding/json/v2 exact corpus, SIMD and pure Go\n'
for mode in pure simd; do
printf 'benchmark-variant=jsonv2-%s\n' "$mode"
experiment=jsonv2
if [ "$mode" = simd ]; then
	experiment=simd,jsonv2
fi
for group in dynamic-owned typed-reused encode; do
(
	cd "$root"
	GOEXPERIMENT="$experiment" "$tip_go" test -run='^$' \
		-bench="^BenchmarkStdlibCorpusJSONV2$/^.*$/${group}$" \
		-benchmem -benchtime="$benchtime" -count="$count" -cpu=1
)
done
done

printf '\nGo 1.26 native Sonic exact corpus\n'
printf 'benchmark-variant=sonic\n'
for group in valid dynamic-owned typed-reused encode; do
(
	cd "$root/legacy"
	GOTOOLCHAIN="$legacy_toolchain" "$legacy_go" test -run='^$' \
		-bench="^BenchmarkStdlibCorpusNativeSonic$/^.*$/${group}$" \
		-benchmem -benchtime="$benchtime" -count="$count" -cpu=1
)
done
