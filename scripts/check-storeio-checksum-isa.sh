#!/bin/sh
# Prove that pinned-SIMD checksum builds retain carry-less multiplication,
# introduce no heap calls, and keep the capability wrapper free of SIMD work.
set -eu

go_bin=${1:-go}
work=$(mktemp -d "${TMPDIR:-/tmp}/slopjson-storeio-checksum-isa.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM
package_path=$(GOTOOLCHAIN=local "$go_bin" list -f '{{.ImportPath}}' ./internal/storeio)
package_pattern=$(printf '%s\n' "$package_path" | sed 's/\./\\./g')

check_target() {
	os=$1
	arch=$2
	body=$3
	instruction=$4
	binary="$work/storeio-$os-$arch.test"
	body_assembly="$work/storeio-$os-$arch-body.asm"
	dispatch_assembly="$work/storeio-$os-$arch-dispatch.asm"

	GOOS=$os GOARCH=$arch GOEXPERIMENT=simd GOTOOLCHAIN=local \
		"$go_bin" test -c ./internal/storeio -o "$binary"
	"$go_bin" tool objdump -s "^${package_pattern}\.${body}$" \
		"$binary" >"$body_assembly"
	if ! grep -Eq "[[:space:]]${instruction}[[:space:]]" "$body_assembly"; then
		echo "$os/$arch checksum body did not retain $instruction" >&2
		exit 1
	fi
	if grep -Eq 'runtime\.(newobject|mallocgc)' "$body_assembly"; then
		echo "$os/$arch checksum body contains a heap-allocation call" >&2
		exit 1
	fi

	"$go_bin" tool objdump -s "^${package_pattern}\.pageChecksum$" \
		"$binary" >"$dispatch_assembly"
	if grep -Eq '[[:space:]](VPCLMULQDQ|VPMULL2?)[[:space:]]' "$dispatch_assembly"; then
		echo "$os/$arch checksum capability wrapper contains SIMD work" >&2
		exit 1
	fi
}

check_target linux amd64 pageChecksumAVX512 VPCLMULQDQ
check_target darwin arm64 pageChecksumPMULL9 VPMULL

pclmul_assembly="$work/storeio-amd64-pclmul.asm"
"$go_bin" tool objdump -s "^${package_pattern}\.pageChecksumPCLMUL8$" \
	"$work/storeio-linux-amd64.test" >"$pclmul_assembly"
if ! grep -Eq '[[:space:]]VPCLMULQDQ[[:space:]]' "$pclmul_assembly"; then
	echo 'amd64 128-bit checksum body did not retain VPCLMULQDQ' >&2
	exit 1
fi
if grep -Eq 'runtime\.(newobject|mallocgc)' "$pclmul_assembly"; then
	echo 'amd64 128-bit checksum body contains a heap-allocation call' >&2
	exit 1
fi
if grep -Eq '[[:space:],]Z[0-9]+([[:space:],]|$)' "$pclmul_assembly"; then
	echo 'amd64 128-bit checksum body unexpectedly requires ZMM registers' >&2
	exit 1
fi
