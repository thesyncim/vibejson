# Contributing

Contributions must preserve JSON correctness, ownership, portability, and
documented allocation behavior before they improve throughput or latency.

## Development environment

All modules require Go 1.27.0 or later. Validate with the latest Go 1.27 patch
release in portable and SIMD modes. Backend-sensitive work also checks the
development toolchain pinned by the repository:

```sh
./scripts/bootstrap-gotip.sh "$HOME/sdk/vibejson-gotip"
```

Released Go 1.27 and the pinned compiler both build the SIMD lane on supported
amd64 and arm64 systems with `GOEXPERIMENT=simd`:

```sh
GOTOOLCHAIN=local GOEXPERIMENT=simd \
  "$HOME/sdk/vibejson-gotip/bin/go" test ./...
```

The repository contains three Go modules:

| Directory | Purpose | Toolchain |
| --- | --- | --- |
| `.` | Library, packages, tests, and most microbenchmarks | Go 1.27.0+ |
| `tests/stdlib` | Pinned standard-library corpus and parity benchmarks | Go 1.27.0+ |
| `benchmarks` | Cross-package/native corpus benchmark harness | Go 1.27.0+ |

The root module must remain standard-library-only. Dependencies used by corpus
or benchmark tooling belong in their nested modules.

## Default build, test, and benchmark commands

Repository commands default to `GOEXPERIMENT=simd` on Go 1.27:

```sh
make build
make test
make bench
make info
```

`make bench` selects a small set of public-operation benchmarks for fast
feedback. Narrow tests with `PACKAGES` and `TEST_FLAGS`; choose benchmark
packages and rows with `BENCH_PACKAGES` and `BENCH`. `BENCHTIME` defaults to
250 ms and `COUNT` to one; use `COUNT=10` for repeated measurements. Override
`GO` to select a specific compiler. Run benchmarks alone after correctness
checks; building or testing concurrently invalidates timing comparisons.

Portable checks are explicit: `make test GOEXPERIMENT=nosimd` or
`make bench GOEXPERIMENT=nosimd`. CI and standalone test/benchmark scripts use
SIMD by default and retain named portable parity checks. For raw Go commands
and nested modules, set `GOEXPERIMENT=simd` in the command or shell environment;
`go.mod` cannot enable compiler experiments.

## Before changing code

Identify the contract the change affects:

- accepted JSON syntax and error offsets;
- `encoding/json` compatibility for typed values;
- ownership or zero-copy lifetime;
- stream framing and terminal errors;
- portable/SIMD parity;
- concurrency and scratch reuse;
- generated output or source provenance; or
- a measured performance boundary.

Read the corresponding permanent tests and the relevant row in
[UNSAFE.md](UNSAFE.md) before editing an unsafe scope.

## Fast, safe performance iterations

Start from a clean checkpoint and retain its commit as the benchmark baseline.
Inspect the hot function's disassembly, change one measured bottleneck, and run
its byte-exact differential tests in portable and SIMD modes before timing it.
Use a narrow interleaved kernel comparison, then the affected public-operation
rows with unchanged allocation limits. Run timings alone to avoid contention
from builds or other benchmarks. Discard regressions before expanding the test
matrix; run the full checks below on the candidate checkpoint and commit it.

Keep publication separate from experiments: after correctness and regression
gates pass, regenerate all charts and JSON together, then update the Markdown
from those results. Record the measured code commit so documentation-only
commits do not obscure which implementation produced the snapshot.

## Required local checks

Run the released Go 1.27 portable and SIMD checks for every change:

```sh
make test
make test GOEXPERIMENT=nosimd
make vet
GOTOOLCHAIN=local go run ./internal/cmd/testcontracts -check
git diff --check
```

If generated source or generator input changed:

```sh
GOTOOLCHAIN=local go generate ./...
GOTOOLCHAIN=local go mod tidy
git diff --exit-code -- go.mod go.sum
```

Generated output belongs in the same commit as its input. Review the generated
diff like handwritten code.

Changes involving scanners, kernels, numeric conversion, strings, unsafe code,
or backend selection also require the pinned toolchain:

```sh
GOTIP="$HOME/sdk/vibejson-gotip/bin/go"

GOTOOLCHAIN=local GOEXPERIMENT=nosimd "$GOTIP" test ./...
GOTOOLCHAIN=local GOEXPERIMENT=simd "$GOTIP" test ./...
GOTOOLCHAIN=local "$GOTIP" vet ./...
GOTOOLCHAIN=local "$GOTIP" run ./internal/cmd/unsafeinventory -check UNSAFE.md
```

## Correctness evidence

Add the smallest permanent test that proves the changed contract.

- **Typed codecs and field resolution:** compare with `encoding/json` wherever
  semantics are intended to match. Cover addressability, custom methods,
  duplicate fields, null, and reused destinations when relevant.
- **Numbers and formatting:** compare exact bits or bytes with `strconv`,
  `encoding/json`, or a checked-in oracle.
- **Parsers and validators:** add accepted and rejected corpus cases and verify
  agreement across entry points.
- **Streams:** exercise fragmented reads/writes, frame boundaries, value limits,
  impossible `io.Reader`/`io.Writer` counts, and terminal source errors.
- **Ownership:** retain results past the call, mutate or discard source storage,
  force GC and stack growth, and test aliases explicitly.
- **Optimized routes:** prove route selection, malformed-input behavior, and
  byte-for-byte parity with the portable implementation.
- **Concurrency:** reuse immutable plans from multiple goroutines with separate
  destinations and output buffers.

Run all discovered fuzz targets after changing a grammar, state machine, unsafe
boundary, or codec dispatch path:

```sh
./scripts/fuzz-smoke.sh
```

`internal/cmd/testcontracts/contracts.txt` is the machine-checked ownership map
for test files, fuzz targets, and checked-in fuzz seeds. Update it whenever one
of those artifacts is added, removed, or renamed.

## Race, checkptr, and architecture checks

Representative local commands are:

```sh
"$GOTIP" test -short -race -timeout=20m \
  -skip 'Alloc|ZeroCost|StaysOnStack' ./...

GOTOOLCHAIN=local GOEXPERIMENT=simd "$GOTIP" test \
  -gcflags=all=-d=checkptr=2 \
  -skip 'Alloc|ZeroCost|StaysOnStack' ./...
```

Backend changes should also run:

```sh
./scripts/check-amd64-stage1-isa.sh "$GOTIP"

GOOS=linux GOARCH=386 "$GOTIP" build ./...
GOOS=linux GOARCH=s390x "$GOTIP" build ./...
```

CI adds native amd64 and arm64 execution, portable and SIMD lanes, shuffled
tests, corpus validation, generated-source checks, static analysis, dependency
audits, and benchmark-module builds.

## Benchmarks

Correctness tests come first. When performance is in scope, record:

- commit and comparison baseline;
- exact Go version and `GOEXPERIMENT`;
- operating system, architecture, and CPU;
- benchmark selector, `-cpu`, `-benchtime`, and `-count`; and
- `ns/op`, `B/op`, and `allocs/op`.

Run every build-selected benchmark once across released Go 1.27 and the pinned
compiler, in portable and SIMD modes, as a coverage and health check:

```sh
GOTIP="$HOME/sdk/vibejson-gotip/bin/go"

run_benchmarks() (
  cd "$1"
  GOTOOLCHAIN=local GOEXPERIMENT="$3" GOMAXPROCS=1 \
    "$2" test -run '^$' -bench . -benchmem \
    -benchtime=250ms -count=1 -cpu=1 ./...
)

run_benchmarks . "$(command -v go)" nosimd
run_benchmarks tests/stdlib "$(command -v go)" nosimd
run_benchmarks benchmarks "$(command -v go)" nosimd
run_benchmarks . "$(command -v go)" simd
run_benchmarks tests/stdlib "$(command -v go)" simd
run_benchmarks benchmarks "$(command -v go)" simd
run_benchmarks . "$GOTIP" nosimd
run_benchmarks tests/stdlib "$GOTIP" nosimd
run_benchmarks benchmarks "$GOTIP" nosimd
run_benchmarks . "$GOTIP" simd
run_benchmarks tests/stdlib "$GOTIP" simd
run_benchmarks benchmarks "$GOTIP" simd
```

Use the maintained interleaved gate for a regression decision:

```sh
BENCH_GO="$(command -v go)" \
  ./scripts/bench-gate.sh -b HEAD~1 -c 63
```

The gate defaults to SIMD. Use `BENCH_GOEXPERIMENT=nosimd` for the portable
comparison and repeat backend-sensitive work with the pinned toolchain. The gate alternates baseline and candidate binaries,
requires the expected rows, and checks statistically significant time,
allocation, and retained-byte regressions. A microbenchmark alone does not
establish end-to-end improvement; add a route test and measure a public
operation that reaches the specialization.

See [docs/benchmarking.md](docs/benchmarking.md) for suite structure and result
interpretation.

## Documentation and provenance

Documentation changes are part of the implementation:

- update exported Go comments when behavior or lifetime changes;
- keep complete README examples runnable and code fragments type-correct and
  task-oriented;
- keep package paths, Go versions, build tags, and CI commands exact;
- state whether examples own or borrow their data;
- avoid unqualified performance claims; and
- update provenance and required license text with externally derived material.

The documentation map is:

- [README.md](README.md): user-facing overview and API selection;
- [benchmarks/README.md](benchmarks/README.md): published absolute results and
  comparison contract;
- [docs/architecture.md](docs/architecture.md): package and execution design;
- [docs/benchmarking.md](docs/benchmarking.md): benchmark methodology;
- [MIGRATION.md](MIGRATION.md): module rename and package moves;
- [SECURITY.md](SECURITY.md): supported revisions and private reporting;
- [docs/provenance.md](docs/provenance.md): external source and algorithm ledger;
  and
- [UNSAFE.md](UNSAFE.md): generated unsafe-scope inventory.

Do not hand-edit generated sections of `UNSAFE.md`, generated decoder files, or
generated numeric tables.

## Pull request checklist

Before requesting review:

1. keep the change focused and explain the contract it changes;
2. include the permanent correctness test;
3. run the required stable and applicable pinned-toolchain lanes;
4. reproduce generated files and module metadata;
5. report benchmark methodology and all three metrics when performance is
   claimed;
6. update ownership, architecture, migration, security, and provenance
   documentation where applicable; and
7. leave the worktree free of generated or benchmark artifacts.
