# Contributing

Changes must preserve correctness, ownership, portability, and the documented
allocation contract before they improve a benchmark.

## Toolchains

Use the latest Go 1.26 patch release for the stable portable lane. SIMD changes
also require the exact development compiler built by:

```sh
./scripts/bootstrap-gotip.sh "$HOME/sdk/slopjson-gotip"
```

Stable Go builds the portable implementation. The pinned development compiler
must pass both portable and `GOEXPERIMENT=simd` modes on amd64 and arm64;
unvalidated compiler families keep the portable source set.

## Required local checks

Start with the stable lane:

```sh
GOTOOLCHAIN=local go test ./...
GOTOOLCHAIN=local go vet ./...
```

Then run the pinned compiler in both source modes:

```sh
export GOTIP="$HOME/sdk/slopjson-gotip/bin/go"
GOTOOLCHAIN=local "$GOTIP" test ./...
GOTOOLCHAIN=local GOEXPERIMENT=simd "$GOTIP" test ./...
GOTOOLCHAIN=local "$GOTIP" vet ./...
```

Before committing:

```sh
go generate ./...
go mod tidy
go run ./internal/cmd/testcontracts -check
git diff --check
```

Generated output belongs in the same commit as its generator or source change.

## Correctness

Add the smallest permanent test that proves the changed contract:

- parser and codec behavior needs differential coverage where
  `encoding/json` has the same semantics;
- stream changes need fragmented-I/O, boundary, and terminal-state coverage;
- ownership changes need retained-result, forced-GC, and stack-growth coverage;
- persistence changes need fault injection, reopen, and previous-generation
  recovery coverage;
- optimized routes need portable/accelerated parity and a malformed-input path.

[TEST_CONTRACTS.md](TEST_CONTRACTS.md) is the machine-checked ownership map for
test files, fuzz targets, and checked-in corpus seeds.

## Unsafe and external memory

Unsafe code is permitted only for a bounded, measured path that ordinary Go
cannot express without violating a maintained contract.

Do not hide a Go pointer in `uintptr`, depend on a private runtime layout, or
place Go pointers in external memory.

After changing an unsafe scope:

```sh
"$GOTIP" run ./internal/cmd/unsafeinventory -write UNSAFE.md
"$GOTIP" run ./internal/cmd/unsafeinventory -check UNSAFE.md
"$GOTIP" test -race -skip 'Alloc|ZeroCost|StaysOnStack' ./...
GOEXPERIMENT=simd "$GOTIP" test \
  -gcflags=all=-d=checkptr=2 \
  -skip 'Alloc|ZeroCost|StaysOnStack' ./...
```

Review the affected bounds, GC visibility, ownership, aliases, and fallback
behavior in [UNSAFE.md](UNSAFE.md).

## Performance

Compare the change with its merge base using the same compiler, CPU, operating
system, input, and benchmark duration. Report time, bytes, and allocations.
Inspect retained memory and generated code when the change affects either.

Use `scripts/bench-gate.sh` for maintained root benchmarks:

```sh
BENCH_GO="$(command -v go)" BENCH_GOEXPERIMENT= \
  ./scripts/bench-gate.sh -b HEAD~1 -c 63
```

The gate rejects statistically significant time regressions above 2% and any
significant bytes/op or allocations/op increase. A targeted run must record its
exact selector and row count.

A synthetic kernel result does not justify a specialization by itself. Keep a
portable implementation and add a route test that proves when the optimized
path is selected.

## Documentation

Update one canonical document:

- [README.md](README.md) for the product surface;
- [docs/store.md](docs/store.md) for storage and durability;
- [CONTRIBUTING.md](CONTRIBUTING.md) for build, compiler, test, and benchmark
  policy;
- [docs/provenance.md](docs/provenance.md) or [UNSAFE.md](UNSAFE.md) for their
  machine-checked inventories.

Do not add historical implementation journals or a second roadmap beside the
current contract.
