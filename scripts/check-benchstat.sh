#!/bin/sh
# Fail on statistically significant benchmark regressions. benchstat emits a
# numeric "vs base" only after its significance test passes; "~" is noise.
set -eu

if [ "$#" -lt 2 ] || [ "$#" -gt 3 ]; then
	echo "usage: check-benchstat.sh old.txt new.txt [max-sec-regression-percent]" >&2
	exit 2
fi

old=$1
new=$2
limit=${3:-${BENCH_REGRESSION_LIMIT:-2}}
byte_limit=${BENCH_B_PER_OP_REGRESSION_LIMIT:-0.01}
benchstat=${BENCHSTAT:-benchstat}

case $limit in
'' | *[!0-9.]* | *.*.*)
	echo "invalid regression limit: $limit" >&2
	exit 2
	;;
esac
case $byte_limit in
'' | *[!0-9.]* | *.*.*)
	echo "invalid B/op regression limit: $byte_limit" >&2
	exit 2
	;;
esac
if [ ! -x "$benchstat" ] && ! command -v "$benchstat" >/dev/null 2>&1; then
	echo "benchstat is not executable: $benchstat" >&2
	exit 1
fi

csv=$(mktemp "${TMPDIR:-/tmp}/slopjson-benchstat.XXXXXX")
trap 'rm -f "$csv"' EXIT HUP INT TERM
"$benchstat" -format csv "$old" "$new" >"$csv"

awk -F, -v sec_limit="$limit" -v byte_limit="$byte_limit" '
$1 == "" {
	# Every benchstat section starts with one or more header rows. Reset the
	# tracked metric even for unguarded throughput sections such as B/s; without
	# this, a positive throughput change inherits sec/op and becomes a false
	# latency regression.
	metric = ""
	if ($2 == "sec/op" || $2 == "B/op" || $2 == "allocs/op") {
		metric = $2
	}
	next
}
$1 == "geomean" { next }
metric != "" && $6 ~ /^\+/ {
	change = $6
	sub(/^\+/, "", change)
	sub(/%$/, "", change)
	# Repeated benchmark processes can vary by substantially less than one
	# allocation while keeping allocs/op identical. Preserve an exact zero
	# tolerance for allocation count, but ignore B/op movement no larger than
	# the displayed 0.01% benchstat noise floor.
	threshold = metric == "sec/op" ? sec_limit : (metric == "B/op" ? byte_limit : 0)
	if (change == "Inf" || change + 0 > threshold) {
		printf "regression: %s %s increased %s (limit %.2f%%; %s)\n", $1, metric, $6, threshold, $7 > "/dev/stderr"
		failed = 1
	}
}
END { exit failed }
' "$csv"
