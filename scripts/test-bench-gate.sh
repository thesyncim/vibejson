#!/bin/sh
# Exercise real baseline preparation without compiling or timing Go code.
# An optional gate path supports checking an earlier version of the script.
set -eu

[ "$#" -le 1 ] || { echo "usage: test-bench-gate.sh [bench-gate.sh]" >&2; exit 2; }
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
gate=${1:-"$root/scripts/bench-gate.sh"}
fixture=$(mktemp -d "${TMPDIR:-/tmp}/vibejson-bench-gate-test.XXXXXX")
trap 'rm -rf "$fixture"' EXIT HUP INT TERM
mkdir -p "$fixture/repo/scripts" "$fixture/tmp" "$fixture/capture"
cp "$gate" "$fixture/repo/scripts/bench-gate.sh"

# The compiler records the files that would be compiled on each side, then
# deliberately stops after the baseline snapshot. No benchmark binaries run.
cat > "$fixture/go" <<'GO'
#!/bin/sh
set -eu
if [ "$1" = version ]; then
	echo 'go version benchmark-fixture'
	exit 0
fi
[ "$1" = test ] || exit 2
output=
while [ "$#" -gt 0 ]; do
	if [ "$1" = -o ]; then
		shift
		output=$1
	fi
	shift
done
case $output in
*/new.test) side=candidate ;;
*/old.test) side=baseline ;;
*) exit 2 ;;
esac
source_root=$PWD
while [ ! -f "$source_root/fixture-root" ]; do
	[ "$source_root" != / ] || exit 2
	source_root=$(dirname "$source_root")
done
mkdir -p "$BENCH_GATE_TEST_CAPTURE/$side/package"
cp "$source_root/implementation.go" "$BENCH_GATE_TEST_CAPTURE/$side/root.go"
cp ./*.go "$BENCH_GATE_TEST_CAPTURE/$side/package/"
if [ "$side" = baseline ]; then
	echo 'fixture: captured baseline compilation' >&2
	exit 73
fi
: > "$output"
GO
cat > "$fixture/benchstat" <<'STAT'
#!/bin/sh
# The gate must stop at the compiler sentinel before invoking benchstat.
exit 74
STAT
chmod +x "$fixture/go" "$fixture/benchstat"

printf 'package fixture\n// baseline production\n' > "$fixture/old.go"
printf 'package fixture\n// candidate production\n' > "$fixture/new.go"
printf 'package fixture\n// baseline harness\n' > "$fixture/old_test.go"
printf 'package fixture\n// candidate harness\n' > "$fixture/new_test.go"
: > "$fixture/repo/fixture-root"
for directory in . x/kernels simd internal/benchgate tests/stdlib; do
	mkdir -p "$fixture/repo/$directory"
	cp "$fixture/old_test.go" "$fixture/repo/$directory/bench_test.go"
	cp "$fixture/old_test.go" "$fixture/repo/$directory/removed_test.go"
	case $directory in
	. | x/kernels | simd)
		cp "$fixture/old.go" "$fixture/repo/$directory/implementation.go"
		cp "$fixture/old.go" "$fixture/repo/$directory/removed.go"
		;;
	esac
done
git -C "$fixture/repo" init -q
git -C "$fixture/repo" add -f .
git -C "$fixture/repo" -c user.name=Fixture -c user.email=fixture@example.invalid \
	-c commit.gpgSign=false -c core.hooksPath=/dev/null commit -qm baseline

for directory in . x/kernels simd internal/benchgate tests/stdlib; do
	cp "$fixture/new_test.go" "$fixture/repo/$directory/bench_test.go"
	cp "$fixture/new_test.go" "$fixture/repo/$directory/added_test.go"
	rm "$fixture/repo/$directory/removed_test.go"
	case $directory in
	. | x/kernels | simd)
		cp "$fixture/new.go" "$fixture/repo/$directory/implementation.go"
		cp "$fixture/new.go" "$fixture/repo/$directory/added.go"
		rm "$fixture/repo/$directory/removed.go"
		;;
	esac
done

fail() {
	echo "benchmark baseline isolation: $directory: $*" >&2
	cat "$fixture/gate.log" >&2
	exit 1
}

for directory in . x/kernels simd internal/benchgate tests/stdlib; do
	rm -rf "$fixture/capture"
	mkdir -p "$fixture/capture"
	gate_status=0
	BENCH_GO="$fixture/go" BENCHSTAT="$fixture/benchstat" \
		BENCH_GATE_TEST_CAPTURE="$fixture/capture" TMPDIR="$fixture/tmp" \
		sh "$fixture/repo/scripts/bench-gate.sh" -b HEAD -d "$directory" \
		-c 1 -n 1 -t 1x '^BenchmarkFixture$' > "$fixture/gate.log" 2>&1 || gate_status=$?
	[ "$gate_status" -eq 73 ] || fail "gate did not reach the baseline compiler (status $gate_status)"
	old="$fixture/capture/baseline"
	new="$fixture/capture/candidate"
	cmp -s "$fixture/old.go" "$old/root.go" || fail 'baseline root production changed'
	cmp -s "$fixture/new.go" "$new/root.go" || fail 'candidate root production changed'
	cmp -s "$fixture/new_test.go" "$new/package/bench_test.go" || fail 'candidate harness changed'
	[ -f "$new/package/added_test.go" ] && [ ! -e "$new/package/removed_test.go" ] || fail 'candidate test files changed'
	case $directory in
	internal/benchgate | tests/stdlib)
		cmp -s "$fixture/new_test.go" "$old/package/bench_test.go" || fail 'isolated harness was not synchronized'
		[ -f "$old/package/added_test.go" ] && [ ! -e "$old/package/removed_test.go" ] || fail 'isolated harness file set was not synchronized'
		;;
	*)
		cmp -s "$fixture/old.go" "$old/package/implementation.go" || fail 'baseline package production was replaced'
		cmp -s "$fixture/new.go" "$new/package/implementation.go" || fail 'candidate package production changed'
		cmp -s "$fixture/old_test.go" "$old/package/bench_test.go" || fail 'production package tests were replaced'
		[ -f "$old/package/removed.go" ] && [ ! -e "$old/package/added.go" ] || fail 'baseline production file set changed'
		[ -f "$new/package/added.go" ] && [ ! -e "$new/package/removed.go" ] || fail 'candidate production file set changed'
		[ -f "$old/package/removed_test.go" ] && [ ! -e "$old/package/added_test.go" ] || fail 'baseline test file set changed'
		;;
	esac
	echo "PASS benchmark baseline isolation: $directory"
done
