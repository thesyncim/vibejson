#!/bin/sh
# Build the Linux test binary outside the memory cgroup, then prove a live
# FileStore image and its allocated blocks exceed the complete cgroup limit by
# the requested ratio. The default writes a little over 6.4 GiB under 64 MiB.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
memory=${FILESTORE_SCALE_MEMORY:-64m}
ratio=${FILESTORE_SCALE_RATIO:-100}
payload=${FILESTORE_SCALE_PAYLOAD:-3141632}
image=${FILESTORE_SCALE_GO_IMAGE:-golang:1.26.4-bookworm}
work=$(mktemp -d "${TMPDIR:-/tmp}/simdjson-physical-scale.XXXXXX")
volume=simdjson-physical-scale-$$

cleanup() {
	docker volume rm -f "$volume" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT INT TERM

docker volume create "$volume" >/dev/null
docker run --rm \
	-v "$root:/src:ro" \
	-v "$work:/out" \
	-w /src \
	-e GOTOOLCHAIN=local \
	"$image" \
	go test -c -o /out/simdjson.test .

docker run --rm \
	--memory "$memory" \
	--memory-swap "$memory" \
	--pids-limit 256 \
	-v "$work:/out:ro" \
	-v "$volume:/scale" \
	-w /scale \
	-e TMPDIR=/scale \
	-e GOMEMLIMIT=36MiB \
	-e GOGC=50 \
	-e SIMDJSON_FILESTORE_PHYSICAL_100X=1 \
	-e SIMDJSON_FILESTORE_PHYSICAL_RATIO="$ratio" \
	-e SIMDJSON_FILESTORE_PHYSICAL_PAYLOAD="$payload" \
	--entrypoint /out/simdjson.test \
	"$image" \
	-test.run '^TestFileStorePhysicalHundredXMemory$' \
	-test.v \
	-test.count=1
