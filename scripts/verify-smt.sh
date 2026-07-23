#!/bin/sh
# Discharge the bit-packing invariant proofs with an SMT solver. Each script in
# testdata/smt asserts the negation of an invariant; the solver must report
# `unsat`. The raw solver output is the artifact; this script does no
# arithmetic. Run from the repository root.
set -eu

solver=${Z3:-z3}
dir=testdata/smt
log=$dir/z3-results.log

if ! command -v "$solver" >/dev/null 2>&1; then
	echo "required tool is unavailable: $solver (set Z3 to its path)" >&2
	exit 1
fi

: >"$log"
{
	echo "SOLVER $("$solver" --version 2>/dev/null || echo unknown)"
	echo "DIR $dir"
} >>"$log"

fail=0
for script in "$dir"/*.smt2; do
	result=$("$solver" -smt2 "$script" 2>&1 | tr -d '[:space:]')
	echo "SCRIPT $script" >>"$log"
	echo "RESULT $result" >>"$log"
	if [ "$result" = "unsat" ]; then
		echo "VERDICT PASS" >>"$log"
	else
		echo "VERDICT FAIL" >>"$log"
		echo "SMT script did not prove unsat: $script -> $result" >&2
		fail=1
	fi
done

echo "SUMMARY scripts=$(ls "$dir"/*.smt2 | wc -l | tr -d ' ') fail=$fail" >>"$log"
cat "$log"
[ "$fail" -eq 0 ]
