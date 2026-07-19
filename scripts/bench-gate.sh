#!/bin/sh
# bench-gate.sh compares the working tree against a baseline ref with
# interleaved benchmark rounds, the zero-regression gate used before every
# performance-sensitive commit.
#
#   scripts/bench-gate.sh [-b ref] [-c expected-rows] [-d directory]
#                         [-n rounds] [-t benchtime]
#                         [-r max-regression-percent] [pattern]
#
# The default baseline is HEAD, rounds default to 8, benchtime to 250ms, and
# the pattern defaults to the corpus decode/validate/encode rows. Requires
# the pinned gotip and benchstat. Runs the tests/stdlib corpus benchmarks
# with GOEXPERIMENT=simd; results land under $TMPDIR/simdjson-bench-gate.
set -eu

gotip=${GOTIP:-"$HOME/sdk/simdjson-gotip/bin/go"}
benchstat=${BENCHSTAT:-"$HOME/go/bin/benchstat"}
baseline=HEAD
benchdir=tests/stdlib
expected_rows=
rounds=8
benchtime=250ms
regression_limit=${BENCH_REGRESSION_LIMIT:-2}
pattern='BenchmarkHighLevelCorpus/.*/(valid|index|decode-typed|decode-any|encode-typed)/simdjson'

while getopts b:c:d:n:t:r: flag; do
	case $flag in
	b) baseline=$OPTARG ;;
	c) expected_rows=$OPTARG ;;
	d) benchdir=$OPTARG ;;
	n) rounds=$OPTARG ;;
	t) benchtime=$OPTARG ;;
	r) regression_limit=$OPTARG ;;
	*) exit 2 ;;
	esac
done
shift $((OPTIND - 1))
[ $# -le 1 ] || {
	echo "bench-gate accepts at most one benchmark pattern" >&2
	exit 2
}
if [ $# -eq 1 ]; then
	pattern=$1
	if [ -z "$expected_rows" ]; then
		echo "custom benchmark patterns require -c expected-rows" >&2
		exit 2
	fi
elif [ -z "$expected_rows" ]; then
	expected_rows=63
fi

case $expected_rows in
0* | *[!0-9]*)
	echo "expected benchmark rows must be a positive integer: $expected_rows" >&2
	exit 2
	;;
esac

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
repo_id=$(printf '%s' "$root" | cksum | cut -d ' ' -f 1)
work=${TMPDIR:-/tmp}/simdjson-bench-gate-$repo_id
mkdir -p "$work"
baseline_commit=$(git -C "$root" rev-parse "$baseline^{commit}")

if [ ! -x "$gotip" ]; then
	echo "pinned Go toolchain is not executable: $gotip (set GOTIP to override)" >&2
	exit 1
fi
if [ ! -x "$benchstat" ]; then
	echo "benchstat is not executable: $benchstat (set BENCHSTAT to override)" >&2
	exit 1
fi

echo "baseline: $(git -C "$root" rev-parse --short "$baseline_commit")  rounds: $rounds  benchtime: $benchtime" >&2
echo "benchdir: $benchdir  max significant sec/op regression: $regression_limit%" >&2
echo "max significant B/op regression: ${BENCH_B_PER_OP_REGRESSION_LIMIT:-0.01}%; allocs/op: 0%" >&2

git -C "$root" worktree add --force --detach "$work/baseline" "$baseline_commit" >/dev/null 2>&1 ||
	git -C "$work/baseline" checkout --force "$baseline_commit" >/dev/null 2>&1

(cd "$root/$benchdir" && GOEXPERIMENT=simd "$gotip" test -c -o "$work/new.test" .)
(cd "$work/baseline/$benchdir" && GOEXPERIMENT=simd "$gotip" test -c -o "$work/old.test" .)

: >"$work/old.txt"
: >"$work/new.txt"

run_benchmark() {
	binary=$1
	result=$2
	label=$3
	round_output=$work/current-round.txt
	# Maintained publishers also use one P. These benchmarks are sequential;
	# extra Ps only add scheduler and sync.Pool migration noise to allocs/op.
	if ! "$binary" -test.run '^$' -test.bench "$pattern" \
		-test.benchtime "$benchtime" -test.benchmem -test.cpu=1 >"$round_output" 2>&1; then
		cat "$round_output" >&2
		return 1
	fi
	if ! awk -v label="$label" -v expected_rows="$expected_rows" '
	function is_result( i, unit, has_time, has_bytes, has_allocs) {
		if ($1 !~ /^Benchmark/ || $2 !~ /^[0-9]+$/ || NF < 4 || NF % 2 != 0) {
			return 0
		}
		for (i = 3; i <= NF; i += 2) {
			if ($i !~ /^[-+]?[0-9]+([.][0-9]+)?([eE][-+]?[0-9]+)?$/) {
				return 0
			}
			unit = $(i + 1)
			has_time = has_time || unit == "ns/op"
			has_bytes = has_bytes || unit == "B/op"
			has_allocs = has_allocs || unit == "allocs/op"
		}
		return has_time && has_bytes && has_allocs
	}
	is_result() {
		rows++
		if (++seen[$1] > 1) {
			printf "%s emitted benchmark %s more than once\n", label, $1
			failed = 1
		}
	}
	END {
		if (rows == 0) {
			printf "%s produced no benchmark rows\n", label
			failed = 1
		} else if (expected_rows != "" && rows != expected_rows) {
			printf "%s produced %d benchmark rows, want %d\n", label, rows, expected_rows
			failed = 1
		}
		exit failed
	}
	' "$round_output" >&2; then
		cat "$round_output" >&2
		return 1
	fi
	cat "$round_output" >>"$result"
}

round=0
while [ "$round" -lt "$rounds" ]; do
	# Alternate execution order so thermal state, frequency ramp, and other
	# first-process effects are distributed evenly between the two binaries.
	if [ $((round % 2)) -eq 0 ]; then
		run_benchmark "$work/old.test" "$work/old.txt" "baseline round $round"
		run_benchmark "$work/new.test" "$work/new.txt" "candidate round $round"
	else
		run_benchmark "$work/new.test" "$work/new.txt" "candidate round $round"
		run_benchmark "$work/old.test" "$work/old.txt" "baseline round $round"
	fi
	round=$((round + 1))
done

# Require the exact same full benchmark names on both sides and one sample from
# every interleaved round before benchstat can declare the gate green.
awk -v rounds="$rounds" '
function is_result( i, unit, has_time, has_bytes, has_allocs) {
	if ($1 !~ /^Benchmark/ || $2 !~ /^[0-9]+$/ || NF < 4 || NF % 2 != 0) {
		return 0
	}
	for (i = 3; i <= NF; i += 2) {
		if ($i !~ /^[-+]?[0-9]+([.][0-9]+)?([eE][-+]?[0-9]+)?$/) {
			return 0
		}
		unit = $(i + 1)
		has_time = has_time || unit == "ns/op"
		has_bytes = has_bytes || unit == "B/op"
		has_allocs = has_allocs || unit == "allocs/op"
	}
	return has_time && has_bytes && has_allocs
}
is_result() {
	side = FILENAME == ARGV[1] ? 1 : 2
	name = $1
	rows[side]++
	count[side, name]++
	names[name] = 1
}
END {
	if (rows[1] == 0) {
		print "benchmark gate produced no baseline rows"
		failed = 1
	}
	if (rows[2] == 0) {
		print "benchmark gate produced no candidate rows"
		failed = 1
	}
	for (name in names) {
		if (count[1, name] != rounds) {
			printf "baseline benchmark %s has %d samples, want %d\n", name, count[1, name], rounds
			failed = 1
		}
		if (count[2, name] != rounds) {
			printf "candidate benchmark %s has %d samples, want %d\n", name, count[2, name], rounds
			failed = 1
		}
	}
	exit failed
}
' "$work/old.txt" "$work/new.txt" >&2

"$benchstat" "$work/old.txt" "$work/new.txt"
BENCHSTAT="$benchstat" "$root/scripts/check-benchstat.sh" \
	"$work/old.txt" "$work/new.txt" "$regression_limit"
