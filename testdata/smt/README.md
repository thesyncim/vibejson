# SMT scripts for the bit-packing (encoding) invariants

These scripts machine-check, over the entire 32-bit word domain, the encoding
invariants the index layer's accessors and widenings depend on. Each asserts
the negation of one bit-arithmetic invariant and is discharged by z3 reporting
`unsat` — no input in the modelled domain breaks the invariant, so the `unsat`
is a proof of that specific invariant (the bit arithmetic), not a statement
about the wider system.

| Script | Invariant | Source |
| --- | --- | --- |
| `info_word.smt2` | `packInfo` / `Count` / `Kind` / `flags` round-trip for every `count <= infoMaxCount` and 3-bit `kind`, `flags` | `index.go` |
| `info_word_disjoint.smt2` | the count, kind, and flags masks partition the 32-bit word | `index.go` |
| `narrow_span.smt2` | narrow shape-tape `span = start | end<<16` round-trips and its halves are disjoint for `start, end <= shapeNarrowMaxEnd` | `docset_shape.go` |

`TestEncodingInvariantsSMT` (`verify_invariants_test.go`) checks that each script
still models the code's live constants and, when `z3` is on `PATH`, runs it and
requires `unsat`. `TestInfoWordRoundTrip` and `TestNarrowSpanRoundTrip` check
the same invariants in Go over the bounded/randomized-saturation domain, so the
Go suite stands on its own when z3 is unavailable. See
[`docs/design/correctness-checks.md`](../../docs/design/correctness-checks.md)
for the full method.

## Discharging with z3

The scripts are the artifact; the runner does no arithmetic. With `z3` on
`PATH`:

```sh
scripts/verify-smt.sh
```

It runs each script, requires `unsat`, and writes the verbatim log to
`testdata/smt/z3-results.log`. Committing that log records the proof the same
way `benchmarks/results/contains-oracle.log` records the PostgreSQL containment
oracle. The scripts are plain SMT-LIB 2 and run under any conforming solver.

## Raising or changing the bounds

The scripts model fixed machine-word domains, so they do not need widening as
the Go bounds rise. If a layout constant in `index.go` or `docset_shape.go`
changes, `TestEncodingInvariantsSMT` fails because a script no longer contains
the live constant; update the affected `#x...` literals and rerun the runner.
