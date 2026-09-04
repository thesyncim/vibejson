#!/bin/sh
# Prove that baseline wrappers stay CPU-safe and AVX2 kernels use only their
# guarded instruction set. v3/v4 retain direct dispatch.
set -eu

go_bin=${1:-go}
work=$(mktemp -d "${TMPDIR:-/tmp}/vibejson-stage1-isa.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM
package_path=$(GOTOOLCHAIN=local "$go_bin" list -f '{{.ImportPath}}' ./x/kernels)
package_pattern=$(printf '%s\n' "$package_path" | sed 's/\./\\./g')

for level in v1 v2 v3 v4; do
    files=$(
        GOOS=linux GOARCH=amd64 GOAMD64=$level GOEXPERIMENT=simd GOTOOLCHAIN=local \
            "$go_bin" list -f '{{range .GoFiles}}{{println .}}{{end}}' ./x/kernels
    )
    case $level in
        v1 | v2) dispatch=stage1_dispatch_amd64.go ;;
        *) dispatch=stage1_dispatch_v3_amd64.go ;;
    esac
    printf '%s\n' "$files" | grep -qx "$dispatch"
    printf '%s\n' "$files" | grep -qx stage1_amd64.go
    binary="$work/kernels-$level.test"
    assembly="$work/kernels-$level.asm"
    GOOS=linux GOARCH=amd64 GOAMD64=$level GOEXPERIMENT=simd GOTOOLCHAIN=local \
        "$go_bin" test -c ./x/kernels -o "$binary"
    "$go_bin" tool objdump -s "^${package_pattern}\\." "$binary" >"$assembly"
    test -s "$assembly"
    # AVX2 kernels must be present even when the binary targets v1/v2.
    grep -Eq '[[:space:]]VPSHUFB[[:space:]]' "$assembly"
    grep -Eq '[[:space:]]VZEROUPPER' "$assembly"
    case $level in
        v1 | v2)
            # Static wrappers may inline into callers. Scan every symbol,
            # allowing vector instructions only within the guarded entries.
            awk '
                /^TEXT / { kernel = ($0 ~ /\.stage1Block(Brackets)?AVX2\(/) }
                !kernel && /[[:space:]]V[A-Z0-9]+[[:space:]]/ {
                    print "unguarded AVX instruction: " $0; bad = 1
                }
                END { exit bad }
            ' "$assembly"
            ;;
    esac
    case $level in
        v1 | v2 | v3)
            if grep -Eq 'VPERMB|[[:space:],]Z[0-9]+|[[:space:],]K[0-7]([[:space:],]|$)' "$assembly"; then
                echo "GOAMD64=$level emitted an unguarded AVX-512 instruction" >&2
                exit 1
            fi
            ;;
    esac
done
