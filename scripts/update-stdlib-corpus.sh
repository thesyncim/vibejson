#!/bin/sh
set -eu

go_bin=${1:-go}
case $go_bin in
	*/*) go_bin=$(CDPATH= cd -- "$(dirname -- "$go_bin")" && pwd)/$(basename "$go_bin") ;;
esac
repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
goroot=$("$go_bin" env GOROOT)
expected_go_commit=03845e30f7b73d1703bd8c21017297f6eecb76d6
actual_go_commit=$(git -C "$goroot" rev-parse HEAD 2>/dev/null || true)
if [ "$actual_go_commit" != "$expected_go_commit" ]; then
	printf '%s\n' "stdlib corpus update requires Go commit $expected_go_commit; got ${actual_go_commit:-unknown}" >&2
	exit 1
fi
source_dir=$goroot/src/encoding/json/internal/jsontest/_embed
models_source=$goroot/src/encoding/json/internal/jsontest/testdata.go
destination=$repo_root/tests/stdlib/testdata
models_destination=$repo_root/tests/stdlib/models.go
legacy_models_destination=$repo_root/benchmarks/legacy/stdlib_models_test.go

if [ ! -d "$source_dir" ] || [ ! -f "$models_source" ]; then
	printf '%s\n' "encoding/json high-level corpus not found: $source_dir" >&2
	exit 1
fi

mkdir -p "$destination"
find "$destination" -type f -name '*.json.zst' -delete
cp "$source_dir"/*.json.zst "$destination"/
cp "$goroot/LICENSE" "$destination/LICENSE"

{
	sed -n '1,4p' "$models_source"
	printf '%s\n' '' '// Provenance: GO-CORPUS-001.' '// Derived from encoding/json/internal/jsontest/testdata.go at the Go revision' '// recorded in testdata/UPSTREAM.md.' 'package stdlibcorpus' '' 'import (' '    "errors"' '    "time"' ')' ''
	sed -n '/^type (/,$p' "$models_source"
} >"$models_destination"
"$goroot/bin/gofmt" -w "$models_destination"

{
	sed -n '1,4p' "$models_source"
	printf '%s\n' '' '// Provenance: GO-CORPUS-001.' '// Derived from encoding/json/internal/jsontest/testdata.go at the Go revision' '// recorded in tests/stdlib/testdata/UPSTREAM.md.' 'package legacy' '' 'import (' '    "errors"' '    "time"' ')' ''
	sed -n '/^type (/,$p' "$models_source"
} >"$legacy_models_destination"
"$goroot/bin/gofmt" -w "$legacy_models_destination"

cat >"$destination/UPSTREAM.md" <<EOF
# Upstream

- Repository: https://go.googlesource.com/go
- Commit: \`$actual_go_commit\`
- Source: \`src/encoding/json/internal/jsontest/_embed/*.json.zst\`
- Models: \`src/encoding/json/internal/jsontest/testdata.go\`

The seven payloads are copied byte-for-byte and the concrete models are
mechanically extracted from the same revision. Run
\`scripts/check-stdlib-corpus.sh\` to detect additions, removals, or content
drift in the pinned Go tree.
EOF
