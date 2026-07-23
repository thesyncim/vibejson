#!/bin/sh
# Prove that pinned-SIMD amd64 bitmap builds keep the sub-eight-word scalar
# crossover, runtime-gate AVX2 for GOAMD64 v1/v2, and omit that CPU capability
# branch for v3.
set -eu

go_bin=${1:-go}
work=$(mktemp -d "${TMPDIR:-/tmp}/simdjson-bitset-isa.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM
package_path=$(GOTOOLCHAIN=local "$go_bin" list -f '{{.ImportPath}}' ./internal/bitset)
package_pattern=$(printf '%s\n' "$package_path" | sed 's/\./\\./g')

for level in v1 v2 v3; do
	files=$(
		GOOS=linux GOARCH=amd64 GOAMD64=$level GOEXPERIMENT=simd GOTOOLCHAIN=local \
			"$go_bin" list -f '{{range .GoFiles}}{{println .}}{{end}}' ./internal/bitset
	)
	if ! printf '%s\n' "$files" | grep -qx 'ops_simd.go'; then
		echo "GOAMD64=$level did not select the AVX2 bitmap bodies" >&2
		exit 1
	fi
	case $level in
	v1 | v2)
		if ! printf '%s\n' "$files" | grep -qx 'ops_dispatch_amd64.go'; then
			echo "GOAMD64=$level did not select runtime bitmap dispatch" >&2
			exit 1
		fi
		;;
	v3)
		if ! printf '%s\n' "$files" | grep -qx 'ops_dispatch_v3_amd64.go'; then
			echo 'GOAMD64=v3 did not select direct bitmap dispatch' >&2
			exit 1
		fi
		;;
	esac

	binary="$work/bitset-$level.test"
	GOOS=linux GOARCH=amd64 GOAMD64=$level GOEXPERIMENT=simd GOTOOLCHAIN=local \
		"$go_bin" test -c ./internal/bitset -o "$binary"
	vector_assembly="$work/bitset-$level-vector.asm"
	"$go_bin" tool objdump -s "^${package_pattern}\\.(andWordsAVX2|and3WordsAVX2|orWordsAVX2|andNotWordsAVX2)$" \
		"$binary" >"$vector_assembly"
	for instruction in VPAND VPOR VPANDN VZEROUPPER; do
		if ! grep -Eq "[[:space:]]${instruction}[[:space:]]" "$vector_assembly"; then
			echo "GOAMD64=$level bitmap bodies did not retain $instruction" >&2
			exit 1
		fi
	done

	for operation in andWords and3Words orWords andNotWords; do
		dispatch_assembly="$work/bitset-$level-$operation-dispatch.asm"
		"$go_bin" tool objdump -s "^${package_pattern}\\.${operation}$" "$binary" >"$dispatch_assembly"
		if grep -Eq '[[:space:]]V[A-Z0-9]+[[:space:]]' "$dispatch_assembly"; then
			echo "GOAMD64=$level $operation emitted AVX in its dispatch wrapper" >&2
			exit 1
		fi
		if ! grep -Eq 'CMPQ .*\$0x8' "$dispatch_assembly"; then
			echo "GOAMD64=$level $operation omitted the eight-word crossover" >&2
			exit 1
		fi
		if grep -q "${operation}Small" "$dispatch_assembly"; then
			echo "GOAMD64=$level $operation did not inline its small scalar loop" >&2
			exit 1
		fi
		if ! grep -q "${operation}AVX2" "$dispatch_assembly"; then
			echo "GOAMD64=$level $operation dispatch omitted its AVX2 body" >&2
			exit 1
		fi
		case $level in
		v1 | v2)
			if ! grep -q "${operation}Scalar" "$dispatch_assembly"; then
				echo "GOAMD64=$level $operation omitted its large scalar fallback" >&2
				exit 1
			fi
			if ! grep -q bitsetAVX2Available "$dispatch_assembly"; then
				echo "GOAMD64=$level $operation omitted runtime AVX2 gating" >&2
				exit 1
			fi
			;;
		v3)
			if grep -q bitsetAVX2Available "$dispatch_assembly"; then
				echo "GOAMD64=v3 $operation retained a redundant capability branch" >&2
				exit 1
			fi
			;;
		esac
	done
done
