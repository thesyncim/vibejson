#!/bin/sh
# stress-concurrency.sh drives the concurrency-corruption regression suite
# under the conditions that expose cross-goroutine heap corruption in the
# pooled encoder/decoder scratch: aggressive GC (GOGC=1) plus GOMAXPROCS
# transitions mid-run (-cpu=1,4,8), repeated in fresh processes.
#
#   scripts/stress-concurrency.sh [-n rounds] [-c count] [pattern]
#
# Rounds default to 20 fresh processes, count to 3 per process, and the pattern
# to the whole corruption suite. A regression surfaces as a fatal "found bad
# pointer in Go heap" / "found pointer to free object" crash, or a byte-for-byte
# golden mismatch. The race detector's slower scheduling MASKS this class, so
# this run is deliberately without -race; run -race separately for data races.
#
# Requires the pinned gotip. Exits non-zero on the first failing round.
set -eu

gotip=${GOTIP:-"$HOME/sdk/slopjson-gotip/bin/go"}
rounds=20
count=3
pattern='^Test.*Corruption'

while getopts n:c: flag; do
	case $flag in
	n) rounds=$OPTARG ;;
	c) count=$OPTARG ;;
	*) exit 2 ;;
	esac
done
shift $((OPTIND - 1))
[ $# -ge 1 ] && pattern=$1

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"

if [ ! -x "$gotip" ]; then
	echo "pinned Go toolchain is not executable: $gotip (set GOTIP to override)" >&2
	exit 1
fi

echo "stress: rounds=$rounds count=$count pattern=$pattern (GOGC=1 -cpu=1,4,8)" >&2

round=0
while [ "$round" -lt "$rounds" ]; do
	round=$((round + 1))
	if ! out=$(GOGC=1 GOEXPERIMENT=simd "$gotip" test -run "$pattern" -count="$count" -cpu=1,4,8 ./ 2>&1); then
		echo "ROUND $round FAILED:" >&2
		echo "$out" >&2
		exit 1
	fi
	printf '.' >&2
done
echo >&2
echo "stress: $rounds rounds clean" >&2
