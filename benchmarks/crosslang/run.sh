#!/bin/sh
# Cross-language corpus benchmark over the exact repository-pinned Go
# encoding/json test corpus. Only parse+semantic-digest is a direct comparison;
# representation-specific C++ and Rust rows are diagnostics.
set -eu

dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH= cd -- "$dir/../.." && pwd)
corpus=${CORPUS:-"$dir/corpus"}
compressed=${COMPRESSED_CORPUS:-"$root/tests/stdlib/testdata"}
build=${BUILD_DIR:-"$dir/.build"}
cache=${CACHE_DIR:-"${XDG_CACHE_HOME:-$HOME/.cache}/simdjson-crosslang"}
tip_go=${TIP_GO:-"$HOME/sdk/gotip/bin/go"}
cpp_tag=v4.6.4
cpp_commit=1bcf71bd85059ab6574ea1159de9298dcc1212c5
cpp_src="$cache/simdjson-$cpp_tag"

required_tools="clang++ zstd awk git"
if [ "${CONTRACT_ONLY:-0}" != 1 ]; then
	required_tools="$required_tools cargo"
fi
for tool in $required_tools; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		echo "required tool is unavailable: $tool" >&2
		exit 1
	fi
done
if [ ! -x "$tip_go" ]; then
	echo "pinned Go toolchain is not executable: $tip_go (set TIP_GO to override)" >&2
	exit 1
fi

commit=$(git -C "$root" rev-parse HEAD)
dirty=$(git -C "$root" status --porcelain --untracked-files=normal)
if [ -n "$dirty" ] && [ "${ALLOW_DIRTY:-0}" != 1 ]; then
	echo "refusing to publish from a dirty tree; commit the release candidate or set ALLOW_DIRTY=1 for a development run" >&2
	exit 1
fi
printf 'repository commit=%s dirty=%s\n' "$commit" "$([ -n "$dirty" ] && printf true || printf false)"
"$tip_go" version
clang++ --version | sed -n '1p'
printf 'crosslang-samples=6 crosslang-min-time=250ms\n'

mkdir -p "$corpus" "$build" "$cache"
found_corpus=0
for f in "$compressed"/*.json.zst; do
	[ -f "$f" ] || continue
	found_corpus=1
	out="$corpus/$(basename "$f" .zst)"
	zstd -d -q -f "$f" -o "$out"
done
if [ "$found_corpus" -ne 1 ]; then
	echo "no compressed JSON corpus files found under $compressed" >&2
	exit 1
fi

if [ ! -d "$cpp_src/.git" ]; then
	git clone --depth 1 --branch "$cpp_tag" https://github.com/simdjson/simdjson.git "$cpp_src"
fi
actual_commit=$(git -C "$cpp_src" rev-parse HEAD)
if [ "$actual_commit" != "$cpp_commit" ]; then
	echo "C++ simdjson checkout is $actual_commit, want $cpp_commit" >&2
	exit 1
fi

cppflags="-O3 -DNDEBUG -march=native -std=c++20"
clang++ $cppflags -I"$cpp_src/singleheader" -o "$build/bench_contract" \
	"$dir/bench_contract.cpp" "$cpp_src/singleheader/simdjson.cpp"

if [ "${CONTRACT_ONLY:-0}" != 1 ]; then
	clang++ $cppflags -I"$cpp_src/singleheader" -o "$build/bench_simdjson" \
		"$dir/bench_simdjson.cpp" "$cpp_src/singleheader/simdjson.cpp"
	clang++ $cppflags -I"$cpp_src/singleheader" -o "$build/bench_stage1" \
		"$dir/bench_stage1.cpp" "$cpp_src/singleheader/simdjson.cpp"
	"$build/bench_simdjson" "$corpus"
	"$build/bench_stage1" "$corpus"
fi

cpp_contract_out=$("$build/bench_contract" "$corpus")
go_pure_contract_out=$(
	cd "$dir/.."
	GOTOOLCHAIN=local GOEXPERIMENT=nosimd "$tip_go" run ./crosslang/go_contract "$corpus"
)
go_simd_contract_out=$(
	cd "$dir/.."
	GOTOOLCHAIN=local GOEXPERIMENT=simd "$tip_go" run ./crosslang/go_contract "$corpus"
)
printf 'benchmark-implementation=cpp\n%s\n' "$cpp_contract_out"
printf 'benchmark-implementation=go-pure\n%s\n' "$go_pure_contract_out"
printf 'benchmark-implementation=go-simd\n%s\n' "$go_simd_contract_out"

contract_digests() {
	printf '%s\n' "$1" | awk '/contract=parse\+semantic-digest/ {
		for (i = 1; i <= NF; i++) if ($i ~ /^digest=/) print $1, $i
	}'
}
cpp_digests=$(contract_digests "$cpp_contract_out")
go_pure_digests=$(contract_digests "$go_pure_contract_out")
go_simd_digests=$(contract_digests "$go_simd_contract_out")
if [ "$cpp_digests" != "$go_pure_digests" ] || [ "$cpp_digests" != "$go_simd_digests" ]; then
	echo "cross-language semantic digests differ" >&2
	printf 'C++:\n%s\nGo portable:\n%s\nGo SIMD:\n%s\n' \
		"$cpp_digests" "$go_pure_digests" "$go_simd_digests" >&2
	exit 1
fi
echo "cross-language semantic digests match in C++, portable Go, and SIMD Go"

if [ "${CONTRACT_ONLY:-0}" != 1 ]; then
	RUSTFLAGS="-C target-cpu=native" cargo run --release --locked \
		--manifest-path "$dir/rustbench/Cargo.toml" -- "$corpus"
fi
