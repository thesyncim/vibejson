#!/bin/sh
# Prove that pinned-SIMD amd64 builds select only instructions allowed by
# GOAMD64. v1/v2 must compile the portable kernel; v3 must retain the AVX path.
set -eu

go_bin=${1:-go}
work=$(mktemp -d "${TMPDIR:-/tmp}/slopjson-stage1-isa.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM
package_path=$(GOTOOLCHAIN=local "$go_bin" list -f '{{.ImportPath}}' ./internal/kernels)
package_pattern=$(printf '%s\n' "$package_path" | sed 's/\./\\./g')

for level in v1 v2 v3; do
	files=$(
		GOOS=linux GOARCH=amd64 GOAMD64=$level GOEXPERIMENT=simd GOTOOLCHAIN=local \
			"$go_bin" list -f '{{range .GoFiles}}{{println .}}{{end}}' ./internal/kernels
	)
	case $level in
	v1 | v2)
		if ! printf '%s\n' "$files" | grep -qx 'stage1_default.go'; then
			echo "GOAMD64=$level did not select the portable Stage 1 source" >&2
			exit 1
		fi
		if printf '%s\n' "$files" | grep -qx 'stage1_amd64.go'; then
			echo "GOAMD64=$level selected the AVX Stage 1 source" >&2
			exit 1
		fi
		;;
	v3)
		if ! printf '%s\n' "$files" | grep -qx 'stage1_amd64.go'; then
			echo 'GOAMD64=v3 did not select the SIMD Stage 1 source' >&2
			exit 1
		fi
		if printf '%s\n' "$files" | grep -qx 'stage1_default.go'; then
			echo "GOAMD64=v3 selected the portable Stage 1 source" >&2
			exit 1
		fi
		;;
	esac

	binary="$work/kernels-$level.test"
	assembly="$work/kernels-$level.asm"
	GOOS=linux GOARCH=amd64 GOAMD64=$level GOEXPERIMENT=simd GOTOOLCHAIN=local \
		"$go_bin" test -c ./internal/kernels -o "$binary"
	"$go_bin" tool objdump -s "^${package_pattern}\\." "$binary" >"$assembly"
	if ! test -s "$assembly"; then
		echo "GOAMD64=$level produced no disassembly for $package_path" >&2
		exit 1
	fi

	case $level in
	v1 | v2)
		if grep -Eq '[[:space:]]V[A-Z0-9]+[[:space:]]' "$assembly"; then
			echo "GOAMD64=$level emitted an AVX instruction in internal/kernels" >&2
			exit 1
		fi
		;;
	v3)
		if ! grep -Eq '[[:space:]]VPSHUFB[[:space:]]' "$assembly"; then
			echo 'GOAMD64=v3 did not retain the SIMD Stage 1 kernel' >&2
			exit 1
		fi
		;;
	esac
done
