# Benchmarking

Benchmarks serve two purposes:

1. coverage runs execute every declared benchmark and catch panics, allocation
   contract failures, missing corpus data, and stale harnesses; and
2. regression gates compare interleaved baseline and candidate samples on a
   controlled machine.

A result from an unspecified laptop or hosted runner is not a project-wide
performance claim.

## Benchmark modules

| Module | Coverage |
| --- | --- |
| `.` | Root APIs, typed codecs, indexes, streams, hooks, transforms, SIMD helpers, scanners, and structural kernels |
| `tests/stdlib` | High-level operations over the pinned standard-library JSON corpus |
| `benchmarks` | Native corpus, typed model, Stage 2, and cross-package harnesses |

All modules require Go 1.27.0 or later. Use released Go 1.27 for portable and
SIMD comparisons, and the compiler pinned by
[`scripts/bootstrap-gotip.sh`](../scripts/bootstrap-gotip.sh) for the independent
regression lane.

## Local publication

`benchmarks/publish-comparison.sh` measures complete public operations with
matching ownership and result contracts. It compares strict validation, owned
typed and dynamic decoding, owned typed encoding, and reused typed numeric
decoding across portable and SIMD builds. The script compiles each mode once,
uses one CPU, alternates process order by sample round, and sets `GOWORK=off`.

Run it from a clean worktree when a controlled local comparison is useful:

```sh
TIP_GO="$(command -v go)" ./benchmarks/publish-comparison.sh
```

The publisher checks every corpus and operation row. It rejects a result when a
vibejson row is slower or allocates more than its matching `encoding/json` row;
the numeric publication also checks SIMD against portable. Generated JSON, SVG,
and raw benchmark logs stay local and are ignored by Git.

The benchmark harness performs correctness checks before timing. Type caches,
corpus loading, output-size hints, and destination allocation remain outside
timed regions. APIs with different acceptance or ownership semantics use
separate rows. Report `ns/op`, `B/op`, and `allocs/op` together.

## Coverage runs

Run every benchmark once with one CPU to catch stale or missing rows:

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

Coverage answers whether every benchmark executes. It does not provide enough
samples for a regression decision.

## Regression gates

`make bench` and the standalone gate default to SIMD. Use
`GOEXPERIMENT=nosimd` for a portable Make run or
`BENCH_GOEXPERIMENT=nosimd` for a portable paired gate. The publisher builds
both modes explicitly.

`scripts/bench-gate.sh` compiles baseline and candidate binaries, alternates
execution order, validates the expected row set, runs `benchstat`, and enforces
time, bytes-per-operation, and allocation limits. Example:

```sh
BENCH_GO="$(command -v go)" BENCH_GOEXPERIMENT=nosimd \
  ./scripts/bench-gate.sh -b HEAD~1 -c 63 -n 12 -t 500ms -r 2
```

The default gate targets high-level corpus rows. Use `-d` with an explicit
pattern only when the expected row count is known. The performance workflows
also gate resource-after-spike, native hooks, public decoding, validation,
encoding, indexing, and structural kernels. The amd64 workflow covers both
runtime-dispatched `GOAMD64=v1` and direct `GOAMD64=v3` builds; emulated amd64
timings are useful for correctness but are not native x86 evidence.

For changes to scanners, kernels, numeric conversion, unsafe code, or backend
selection, pair benchmark results with route-selection tests, public-operation
benchmarks, portable/SIMD parity, and retained-memory measurements. A kernel
microbenchmark alone does not establish an end-to-end improvement.

## Reporting

Record the candidate and baseline commits, complete `go version`,
`GOEXPERIMENT`, `GOAMD64`, build tags, operating system, architecture, CPU,
benchmark selector, `-cpu`, `-benchtime`, `-count`, and `ns/op`, `B/op`, and
`allocs/op`. State whether the run is coverage, directional, or an authoritative
dedicated comparison.

Keep raw output, test binaries, profiles, and `benchstat` directories outside
the repository. Retain a result file only when it is a deliberately reviewed
fixture with a stable input and ownership contract.
