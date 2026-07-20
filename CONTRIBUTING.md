# Contributing

simdjson supports the latest Go 1.26 patch release for portable builds and an
exact pinned Go 1.27 development revision for SIMD builds. Read the
[`toolchain policy`](docs/toolchain.md) before changing compiler-specific or
generated files.

## Build and test

Run the portable tests and vet with stable Go first. Setting the experiment on
Go 1.26 must retain the same portable source set:

```sh
GOTOOLCHAIN=local go test ./...
GOTOOLCHAIN=local GOEXPERIMENT=simd go test ./...
GOTOOLCHAIN=local go vet ./...
```

The pinned compiler is the additional SIMD and release gate. Build it once,
then validate both its portable and accelerated configurations:

```sh
./scripts/bootstrap-gotip.sh "$HOME/sdk/simdjson-gotip"
export GOTIP="$HOME/sdk/simdjson-gotip/bin/go"
GOTOOLCHAIN=local "$GOTIP" test ./...
GOTOOLCHAIN=local GOEXPERIMENT=simd "$GOTIP" test ./...
GOTOOLCHAIN=local "$GOTIP" vet ./...
```

Before sending a change, also run the focused tests for the affected contract.
Parser or codec behavior changes need differential coverage against
`encoding/json` where its contract applies. Stream changes need fragmented-I/O
and terminal-state coverage. Ownership changes need forced GC, stack growth,
and retained-result coverage. [`TEST_CONTRACTS.md`](TEST_CONTRACTS.md) maps
each maintained invariant to its deterministic, fuzz, safety, and CI coverage.

The [`architecture and safety record`](docs/architecture.md) describes package
boundaries, ownership, typed plans, structural routes, hooks, pooling, and
unsafe-code policy.

## Unsafe code

`unsafe` is permitted only where ordinary Go cannot express the measured hot
path without a regression. Every production function or package scope that
uses it is listed in [`UNSAFE.md`](UNSAFE.md). After adding, removing, or moving
an unsafe operation, update and check the inventory:

```sh
GOTOOLCHAIN=local "$GOTIP" run ./internal/cmd/unsafeinventory -write UNSAFE.md
GOTOOLCHAIN=local "$GOTIP" run ./internal/cmd/unsafeinventory -check UNSAFE.md
GOTOOLCHAIN=local "$GOTIP" test -race -skip 'Alloc|ZeroCost|StaysOnStack' ./...
GOTOOLCHAIN=local GOEXPERIMENT=simd "$GOTIP" test -gcflags=all=-d=checkptr=2 -skip 'Alloc|ZeroCost|StaysOnStack' ./...
```

Update the relevant invariant row in `UNSAFE.md` when the bounds, layout,
ownership, tests, benchmarks, or reviewer change. Do not convert a Go pointer
to `uintptr` for storage, hide it from escape analysis, or depend on private
runtime layouts.

## Performance

Performance is a compatibility contract. Measure changes against the merge
base with the same compiler, CPU, environment, inputs, and benchmark duration.
Use `scripts/bench-gate.sh` for maintained gates and report `sec/op`, `B/op`,
and `allocs/op`; throughput never excuses allocation or retained-memory
regressions. The [benchmark contract](benchmarks/README.md) is the canonical
source for runner roles, compiler selection, exact-row selectors, comparison
boundaries, and reproduction commands.

Keep optimizations behind permanent correctness and route tests. Include the
benchmark or profile that justifies a new specialization and state the
condition under which it should be removed.

## Generated files and commits

Run `go generate ./...` and `go mod tidy`, and verify that neither leaves an
unexpected diff. Keep commits focused and buildable. Commit generated output
with the source or generator change that produced it.
