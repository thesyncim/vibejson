# Contributing

Changes must preserve JSON correctness, ownership, portability, and the
documented allocation contract before they improve a benchmark.

## Toolchains

Use the latest Go 1.26 patch release for the stable portable lane. Changes that
touch scanners, kernels, numeric formatting, or backend selection also require
the exact development compiler built by:

```sh
./scripts/bootstrap-gotip.sh "$HOME/sdk/vibejson-gotip"
```

Stable Go always selects portable code. The pinned compiler can additionally
select the `GOEXPERIMENT=simd` source lane on supported amd64 and arm64 builds.
Compiler families outside the validated build-tag window fall back to portable
code.

## Required checks

Run the stable package tests and static checks first:

```sh
GOTOOLCHAIN=local go test ./...
GOTOOLCHAIN=local go vet ./...
go run ./internal/cmd/testcontracts -check
git diff --check
```

If generated source or its inputs changed, reproduce it and review the diff:

```sh
go generate ./...
go mod tidy
```

Generated output belongs in the same change as its generator or source input.
The root module must remain standard-library-only.

For backend-sensitive changes, run the pinned compiler in both modes:

```sh
export GOTIP="$HOME/sdk/vibejson-gotip/bin/go"
GOTOOLCHAIN=local "$GOTIP" test ./...
GOTOOLCHAIN=local GOEXPERIMENT=simd "$GOTIP" test ./...
GOTOOLCHAIN=local "$GOTIP" vet ./...
```

## Correctness evidence

Add the smallest permanent test that proves the changed contract:

- codec, field-resolution, number, and formatting changes need differential
  coverage against `encoding/json`, `strconv`, or another defined oracle where
  their semantics match;
- parser and validator changes need accepted and rejected corpus cases, exact
  error-boundary checks where promised, and agreement across entry points;
- streams need fragmented-I/O, frame-boundary, size-limit, and terminal-state
  coverage;
- ownership changes need retained-result, forced-GC, stack-growth, and aliasing
  coverage;
- optimized routes need portable/SIMD parity, route-selection coverage, and a
  malformed-input path;
- output whose bytes are contractual needs a checked-in golden or byte-for-byte
  oracle, not a visual comparison.

Fuzz a changed grammar or state machine with:

```sh
./scripts/fuzz-smoke.sh
```

`internal/cmd/testcontracts/contracts.txt` is the machine-checked ownership map
for test files, fuzz targets, and checked-in corpus seeds. Update it with any
test or fuzz-target addition, deletion, or rename.

## Backend validation

Portable behavior is the reference contract. Every accelerated implementation
must have a portable fallback with the same results, errors, and ownership
behavior.

Backend work should cover:

1. stable Go in portable mode;
2. the pinned compiler in portable mode;
3. the pinned compiler with `GOEXPERIMENT=simd`;
4. the affected architecture and dispatch level, including portable fallback
   on an unsupported or future compiler selection.

Use `scripts/check-amd64-stage1-isa.sh` and
`scripts/check-amd64-bitset-isa.sh` when those amd64 kernels change. CI adds
amd64 and arm64 execution, cross-compilation, race, checkptr, corpus, generated
source, unsafe-inventory, and ISA checks.

## Unsafe code

Unsafe code is permitted only for a bounded path with an explicit bounds,
layout, ownership, and GC-visibility proof. Do not depend on private runtime
layout, hide a Go pointer in `uintptr`, or put a Go pointer in external memory.

After changing an unsafe scope:

```sh
"$GOTIP" run ./internal/cmd/unsafeinventory -write UNSAFE.md
"$GOTIP" run ./internal/cmd/unsafeinventory -check UNSAFE.md
"$GOTIP" test -race -skip 'Alloc|ZeroCost|StaysOnStack' ./...
GOEXPERIMENT=simd "$GOTIP" test \
  -gcflags=all=-d=checkptr=2 \
  -skip 'Alloc|ZeroCost|StaysOnStack' ./...
```

Review the affected invariant family and required tests in
[UNSAFE.md](UNSAFE.md). That file is generated; do not hand-edit its scope
list.

## Performance

Compare with the merge base using the same compiler, experiment setting, CPU,
operating system, input, benchmark duration, and repetition count. Report
time/op, bytes/op, and allocations/op together. Inspect retained memory and
generated code when a change affects either.

Use the maintained benchmark gate for end-to-end codec paths:

```sh
BENCH_GO="$(command -v go)" BENCH_GOEXPERIMENT= \
  ./scripts/bench-gate.sh -b HEAD~1 -c 63
```

Record the exact benchmark selector and row count for a targeted run. A kernel
microbenchmark does not establish an end-to-end improvement: add a route test
and measure at least one public operation that reaches the specialization.

## Documentation

The canonical documents have distinct jobs:

- [README.md](README.md) maps the public library surface and ownership rules;
- [docs/architecture.md](docs/architecture.md) describes package boundaries
  and backend design;
- [MIGRATION.md](MIGRATION.md) covers the module rename and database split;
- [CONTRIBUTING.md](CONTRIBUTING.md) owns contributor and measurement policy;
- [docs/provenance.md](docs/provenance.md) and [UNSAFE.md](UNSAFE.md) are
  machine-checked inventories.

Describe implemented behavior and measured conditions. Do not add an
implementation journal, duplicate roadmap, invented figure, or performance
claim without its machine, toolchain, input, command, and repetition count.
