# Contributing

Contributions must preserve JSON correctness, ownership, portability, and
documented allocation behavior before they improve throughput or latency.

## Development environment

The root module builds with the latest Go 1.26 patch release. Backend-sensitive
work also requires the exact development toolchain pinned by the repository:

```sh
./scripts/bootstrap-gotip.sh "$HOME/sdk/vibejson-gotip"
```

Stable Go always selects portable source. The pinned toolchain can additionally
build the validated SIMD lane on supported amd64 and arm64 systems:

```sh
GOTOOLCHAIN=local GOEXPERIMENT=simd \
  "$HOME/sdk/vibejson-gotip/bin/go" test ./...
```

The repository contains three Go modules:

| Directory | Purpose | Toolchain |
| --- | --- | --- |
| `.` | Library, packages, tests, and most microbenchmarks | Go 1.26+ |
| `tests/stdlib` | Pinned standard-library corpus and parity benchmarks | Go 1.26+ |
| `benchmarks` | Cross-package/native corpus benchmark harness | Pinned Go 1.27 development toolchain |

The root module must remain standard-library-only. Dependencies used by corpus
or benchmark tooling belong in their nested modules.

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

## Required local checks

Run the stable portable checks for every change:

```sh
GOTOOLCHAIN=local go test ./...
GOTOOLCHAIN=local go vet ./...
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

GOTOOLCHAIN=local "$GOTIP" test ./...
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

Run every build-selected benchmark once across the stable portable, pinned
portable, and pinned SIMD lanes as a coverage and health check:

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

Use the maintained interleaved gate for a regression decision:

```sh
BENCH_GO="$(command -v go)" BENCH_GOEXPERIMENT= \
  ./scripts/bench-gate.sh -b HEAD~1 -c 63
```

For SIMD-sensitive work, repeat with the pinned toolchain and
`BENCH_GOEXPERIMENT=simd`. The gate alternates baseline and candidate binaries,
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
