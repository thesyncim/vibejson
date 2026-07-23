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
	printf '%s\n' "stdlib corpus check requires Go commit $expected_go_commit; got ${actual_go_commit:-unknown}" >&2
	exit 1
fi
source_dir=$goroot/src/encoding/json/internal/jsontest/_embed
models_source=$goroot/src/encoding/json/internal/jsontest/testdata.go
destination=$repo_root/tests/stdlib/testdata
models_destination=$repo_root/tests/stdlib/models.go

if [ ! -d "$source_dir" ] || [ ! -f "$models_source" ]; then
	printf '%s\n' "encoding/json high-level corpus not found: $source_dir" >&2
	exit 1
fi
if [ ! -d "$destination" ]; then
	printf '%s\n' 'stdlib corpus is missing; run scripts/update-stdlib-corpus.sh' >&2
	exit 1
fi

source_names=$(find "$source_dir" -type f -name '*.json.zst' -exec basename {} \; | LC_ALL=C sort)
destination_names=$(find "$destination" -type f -name '*.json.zst' -exec basename {} \; | LC_ALL=C sort)
if [ "$source_names" != "$destination_names" ]; then
	printf '%s\n' 'stdlib corpus file set differs; run scripts/update-stdlib-corpus.sh' >&2
	printf 'upstream:\n%s\nvendored:\n%s\n' "$source_names" "$destination_names" >&2
	exit 1
fi

for source in "$source_dir"/*.json.zst; do
	name=$(basename "$source")
	if ! cmp -s "$source" "$destination/$name"; then
		printf '%s\n' "stdlib corpus differs: $name" >&2
		exit 1
	fi
done

if ! cmp -s "$goroot/LICENSE" "$destination/LICENSE"; then
	printf '%s\n' 'Go license copy differs; run scripts/update-stdlib-corpus.sh' >&2
	exit 1
fi

expected_models=$(mktemp)
trap 'rm -f "$expected_models"' EXIT HUP INT TERM
{
	sed -n '1,4p' "$models_source"
	printf '%s\n' '' '// Provenance: GO-CORPUS-001.' '// Derived from encoding/json/internal/jsontest/testdata.go at the Go revision' '// recorded in README.md.' 'package stdlibcorpus' '' 'import (' '    "errors"' '    "time"' ')' ''
	sed -n '/^type (/,$p' "$models_source"
} >"$expected_models"
"$goroot/bin/gofmt" -w "$expected_models"
if ! cmp -s "$expected_models" "$models_destination"; then
	printf '%s\n' 'stdlib concrete models differ; run scripts/update-stdlib-corpus.sh' >&2
	exit 1
fi

"$go_bin" test encoding/json
(cd "$repo_root/tests/stdlib" && "$go_bin" test ./...)
(cd "$repo_root/tests/stdlib" && GOEXPERIMENT=simd "$go_bin" test ./...)
