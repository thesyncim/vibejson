# Benchmarking

This repository uses benchmarks for two different purposes:

1. **coverage runs** execute every declared benchmark and catch panics,
   allocation-contract failures, missing corpus data, and stale harnesses; and
2. **regression gates** compare interleaved baseline and candidate samples on a
   controlled machine.

A single benchmark number from an unspecified laptop or hosted runner is not a
performance claim.

## Benchmark modules

| Module | Coverage |
| --- | --- |
| `.` | Root APIs, typed codecs, indexes, streams, hooks, transforms, SIMD helpers, scanners, and structural kernels |
| `tests/stdlib` | High-level operations over the pinned standard-library JSON corpus |
| `benchmarks` | Native corpus, typed model, Stage 2, and cross-package benchmark harnesses |

All three modules require Go 1.27.0 or later. Use released Go 1.27 for portable
and SIMD comparisons, and the compiler pinned by
[`scripts/bootstrap-gotip.sh`](../scripts/bootstrap-gotip.sh) for the independent
regression lane. Published snapshots retain their original compiler metadata.

## Published comparison

The reviewed [benchmark snapshot](../benchmarks/README.md) contains:

- a broad comparison of strict validation, owned typed and dynamic decoding,
  and owned typed encoding over the seven-file pinned standard-library corpus;
  and
- a focused comparison of reused typed decoding for identifier, telemetry, and
  coordinate arrays through vibejson portable, vibejson SIMD, and
  `encoding/json`.

Every SVG uses absolute zero-based values. The JSON sources retain the full
environment, commands, medians, `ns/op`, `B/op`, `allocs/op`, and, for numeric
arrays, input bytes and values per operation.

Publication is deliberately stricter than aggregation: it fails if either
vibejson mode loses to `encoding/json` on time, allocated bytes, or allocation
count in any individual corpus/operation row. The numeric publication also
requires every SIMD row to beat its portable counterpart. This prevents a
favorable total or chart scale from hiding a local regression.

Reproduce the publication from a clean worktree:

```sh
TIP_GO="$(command -v go)" \
  ./benchmarks/publish-comparison.sh
```

The current snapshot uses released Go 1.27.1. The publisher measures portable
peers and the identical vibejson APIs with
`GOEXPERIMENT=simd`, using one CPU and six 300 ms samples by default. It
compiles each mode once and alternates portable/SIMD process order on every
sample round, preventing phase-order drift from systematically favoring either
mode. It measures the root numeric workloads and the nested comparison corpus
in the same run. It does not publish raw one-sample health runs. Adding a
library or operation requires an exact result/ownership contract and a
pre-timing correctness check; an API with different acceptance semantics must
not share a chart row. The script sets `GOWORK=off` for every command so the
checked-in module replacements, rather than a caller's parent workspace,
define the run.

For chart layout changes, render all four SVGs from the checked-in measurements
without running benchmarks again. This retains the recorded commit and compiler:

```sh
(cd benchmarks && go run ./cmd/benchchart -render-only \
  -json results/comparison.json -numeric-json results/numeric.json \
  -time-chart charts/go-times.svg -bytes-chart charts/go-allocations.svg \
  -simd-chart charts/simd-validation-times.svg \
  -numeric-chart charts/simd-numeric-times.svg)
```

## Run every benchmark

The complete one-sample health matrix is:

| Toolchain and backend | Root | `tests/stdlib` | `benchmarks` |
| --- | --- | --- | --- |
| Released Go 1.27, portable | Required | Required | Required |
| Released Go 1.27, SIMD | Required | Required | Required |
| Pinned Go, portable | Required | Required | Required |
| Pinned Go, SIMD | Required | Required | Required |

One P avoids scheduler and `sync.Pool` migration noise and matches the
maintained publication workflow.

```sh
GOTIP="$HOME/sdk/vibejson-gotip/bin/go"

run_benchmarks() (
  cd "$1"
  GOTOOLCHAIN=local GOEXPERIMENT="$3" GOMAXPROCS=1 \
    "$2" test -run '^$' -bench . -benchmem \
    -benchtime=250ms -count=1 -cpu=1 ./...
)

run_benchmarks . "$(command -v go)" ""
run_benchmarks tests/stdlib "$(command -v go)" ""
run_benchmarks benchmarks "$(command -v go)" ""
run_benchmarks . "$(command -v go)" simd
run_benchmarks tests/stdlib "$(command -v go)" simd
run_benchmarks benchmarks "$(command -v go)" simd
run_benchmarks . "$GOTIP" ""
run_benchmarks tests/stdlib "$GOTIP" ""
run_benchmarks benchmarks "$GOTIP" ""
run_benchmarks . "$GOTIP" simd
run_benchmarks tests/stdlib "$GOTIP" simd
run_benchmarks benchmarks "$GOTIP" simd
```

Coverage runs answer “does every benchmark still execute?” They do not provide
enough samples for a regression decision.

## Regression gates

`make bench` and the standalone regression gate default to SIMD. Use
`GOEXPERIMENT=nosimd` for a portable Make run or
`BENCH_GOEXPERIMENT=nosimd` for a portable paired gate. The chart publisher
always builds both modes explicitly, independently of the caller's environment.

The backend validation workflow also accepts an optional `comparison_baseline`
git ref when dispatched manually. This runs the same ten-pair native regression
gates against that revision, which isolates a new optimization from earlier
changes in a PR. Leave it empty for the broad backend matrix.

`scripts/bench-gate.sh` compiles baseline and candidate test binaries, alternates
their execution order, validates the exact benchmark row set, runs `benchstat`,
and enforces time, bytes-per-operation, and allocations-per-operation limits.

Stable portable example:

```sh
BENCH_GO="$(command -v go)" BENCH_GOEXPERIMENT=nosimd \
  ./scripts/bench-gate.sh -b HEAD~1 -c 63 -n 12 -t 500ms -r 2
```

Pinned SIMD example:

```sh
BENCH_GO="$HOME/sdk/vibejson-gotip/bin/go" BENCH_GOEXPERIMENT=simd \
  ./scripts/bench-gate.sh -b HEAD~1 -c 63 -n 12 -t 500ms -r 2
```

The default gate targets high-level corpus rows. Use `-d` and an explicit
pattern only with the correct expected row count. The authoritative performance
workflow also gates resource-after-spike and native-hook end-to-end paths.

## Reporting results

Every report must include:

- candidate and baseline commits;
- full `go version` output;
- `GOEXPERIMENT`, `GOAMD64`, and relevant build tags;
- operating system, architecture, and CPU model;
- benchmark command, selector, `-cpu`, `-benchtime`, and `-count`;
- `ns/op`, `B/op`, and `allocs/op`; and
- whether the result came from a coverage run, a noisy directional runner, or
  an authoritative dedicated runner.

Keep raw benchmark output outside the repository unless it is a deliberately
reviewed fixture. The compact comparison and numeric JSON files and generated
SVGs are reviewed publication artifacts; transient raw logs, test binaries,
profiles, and `benchstat` work directories are not.

## Interpreting improvements

A kernel microbenchmark establishes only the cost of that kernel. An
end-to-end improvement also needs:

- a route-selection test proving the public operation reaches the kernel;
- at least one public-operation benchmark on representative input;
- unchanged correctness and ownership results in portable and SIMD modes; and
- retained-memory measurements when scratch or pooling changes.

Throughput, latency, allocation count, and retained memory are separate
dimensions. Report all affected dimensions; do not trade an unbounded retained
high-water mark for a smaller `ns/op` number.

## Canonicalization release comparison

The v0.1.0 review replaced reflection-based stable sorting with a typed stable
sort. Measured code: `fafdde630e41447a61e69177a44a1c4f81b80e2e`; baseline:
`f5c65829fc7edd57af223044e71ac60dda29f3e1`. Both binaries used the candidate's
`BenchmarkAppendCanonicalize` fixtures, released Go 1.27.1, portable mode,
`darwin/arm64` on Apple M4 Max, `-cpu=1`, and ten alternating 250 ms samples.
Input setup and the reusable output buffer were outside the timer. Numbers are
medians for the complete validating `AppendCanonicalize` call.

| Input | Before | After | Time change | Allocations before → after |
| --- | ---: | ---: | ---: | ---: |
| Empty object | 121.85 ns | 92.32 ns | -24.23% | 3 → 2 |
| Four-field placement document | 718.5 ns | 556.9 ns | -22.50% | 6 → 3 |
| Nested objects with escaped duplicates | 1,196 ns | 828 ns | -30.77% | 19 → 7 |
| 64 fields in reverse key order | 39.11 µs | 22.49 µs | -42.50% | 6 → 3 |

Benchstat reports ±19% uncertainty for the wide-object after measurement;
these are local workload results. All four timing changes were significant
at `p < 0.001` with ten samples. Compare the retained
[before](../benchmarks/results/canonical-before.txt) and
[after](../benchmarks/results/canonical-after.txt) logs with `benchstat`.
The permanent tests preserve decoded UTF-8 key order, number spellings,
nested structure, and stable order across 128 escaped/unescaped duplicate keys.

## Paired correctness and performance passes

For repeated review passes, set `BENCH_CORRECTNESS_PATTERN` and use `-n 10`
with `scripts/bench-gate.sh`. Before each baseline/candidate measurement, the
script runs matching short tests with a shuffle seed derived from the pass
number. A failed test or a pattern matching no passing test aborts the gate.
The two binaries alternate order across the ten passes. Each performance row
must appear exactly once on each side per pass; allocation and significant
time-regression limits remain unchanged. Native PR gates use this mode for
the public JSON operations.

## Generic cursor comparison

The public native cursor uses the same generic scalar kernels that already
backed compiled decoding. Generic methods accept defined destination types and
remove width-specific wrappers; this is not an automatic speedup.

Ten paired Go 1.27.1 SIMD samples on Apple M4 Max (`darwin/arm64`, one CPU,
250 ms per sample), with shuffled hook correctness tests before each measured
process, compare `fafdde630e41447a61e69177a44a1c4f81b80e2e` against
`ba74e524519b3725a3056720c30238de90835713`:

| Existing native-hook benchmark | Before | After | Timing result |
| --- | ---: | ---: | --- |
| Small record | 82.41 ns | 81.81 ns | No significant difference, p=0.218 |
| 1,024-record document | 123.1 µs | 122.6 µs | No significant difference, p=0.393 |

Allocations are unchanged. Compare the retained
[before](../benchmarks/results/generic-hooks-before.txt) and
[after](../benchmarks/results/generic-hooks-after.txt) samples. The
[migration guide](../MIGRATION.md#generic-native-cursor-readers) describes the
breaking source changes and direct decoding into named IDs and enums.

## amd64 UTF-8 lookup comparison

The amd64 UTF-8 validator uses three byte lookup tables, keeping intermediate
vectors in registers instead of spilling them in the block loop. An isolated
comparison uses baseline `3bf3635b95d731a0e99c57edd5a25604434c3f92`, which already
contains the AVX upper-register cleanup, and candidate
`a6efb4db75c47d3d4fe569fd1ba2510268c0f1fd`. On a GitHub-hosted AMD EPYC 7763,
released Go 1.27.1 (`linux/amd64`, `GOEXPERIMENT=simd`, `GOAMD64=v1`, one CPU),
ten alternating 250 ms samples of `BenchmarkValidLongUnicodeString` measured
2.561 µs → 1.030 µs: **59.80% less time, or 2.49× faster**, with zero bytes and
allocations per call. This benchmark exercises the public `Valid` operation.

The [complete comparison report](../benchmarks/results/utf8-lookup-comparison.txt)
includes correctness checks before every measurement and the surrounding
public-operation rows. All passed the existing 5% hosted-runner gate. This is
not a claim of universal improvement: small-record decoding rose 3.30%, large
validation 0.65%, and encoding 1.52% in the same run. The separate comparison
against the PR base and the native arm64 gates also passed. Local instruction
audits check CPU-safe dispatch; native parity, mutated UTF-8 boundaries,
malformed-input, and fallback tests check the selected implementation.

## Native amd64 PR measurements

The `amd64 performance` workflow compares the PR with its base on native
Linux/amd64 hosted runners, separately for runtime-dispatched `GOAMD64=v1`
and direct `GOAMD64=v3` builds. It compiles each revision once and alternates
process order across six rounds. Artifacts include CPU/toolchain metadata,
raw samples, allocations, and benchstat comparisons for public decoding,
validation, encoding, indexing, and structural kernels.

These hosted-runner measurements are directional evidence. The dedicated
`performance` workflow supports both `x64` (default) and `arm64` runner labels
for authoritative regression gates. Docker amd64 execution on an ARM64 host
is useful for correctness, but its emulated timing is not native x86 evidence.
