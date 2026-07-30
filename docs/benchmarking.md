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

The `benchmarks` module requires the development toolchain pinned by
[`scripts/bootstrap-gotip.sh`](../scripts/bootstrap-gotip.sh). The other two
modules build with stable Go 1.26 and the pinned compiler.

## Published comparison

The reviewed [benchmark snapshot](../benchmarks/README.md) compares strict
validation, owned typed and dynamic decoding, and owned typed encoding over the
seven-file pinned standard-library corpus. Its SVG charts use absolute
zero-based values. The JSON source retains the full environment, commands,
per-file medians, `ns/op`, `B/op`, and `allocs/op`.

Reproduce the publication from a clean worktree:

```sh
TIP_GO="$HOME/sdk/vibejson-gotip/bin/go" \
  ./benchmarks/publish-comparison.sh
```

The publisher runs portable peers first and then the identical vibejson APIs
with `GOEXPERIMENT=simd`, using one CPU and six 300 ms samples by default. It
does not publish raw one-sample health runs. Adding a library or operation
requires an exact result/ownership contract and a pre-timing correctness check;
an API with different acceptance semantics must not share a chart row.

## Run every benchmark

The complete one-sample health matrix is:

| Toolchain and backend | Root | `tests/stdlib` | `benchmarks` |
| --- | --- | --- | --- |
| Stable Go, portable | Required | Required | Not buildable (`go 1.27`) |
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

`scripts/bench-gate.sh` compiles baseline and candidate test binaries, alternates
their execution order, validates the exact benchmark row set, runs `benchstat`,
and enforces time, bytes-per-operation, and allocations-per-operation limits.

Stable portable example:

```sh
BENCH_GO="$(command -v go)" BENCH_GOEXPERIMENT= \
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
reviewed fixture. The compact comparison JSON and generated SVGs are reviewed
publication artifacts; transient raw logs, test binaries, profiles, and
`benchstat` work directories are not.

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
