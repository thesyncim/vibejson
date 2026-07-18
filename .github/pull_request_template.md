## Scope

- [ ] The change has one stated contract and does not add unrelated features.
- [ ] Public behavior changes include an ADR and differential tests.
- [ ] Generated files and module files are clean.

## Correctness and ownership

- [ ] Strict JSON and the claimed `encoding/json` behavior are preserved.
- [ ] Borrowing, mutation, invalidation, concurrency, and error ownership are
      documented where they apply.
- [ ] No Go pointer is hidden in `uintptr`, an interface header, or a private
      runtime layout.
- [ ] Race, checkptr, lifetime, corpus, and route-parity gates relevant to the
      change pass.

## Unsafe or SIMD changes

- [ ] Not applicable, or `UNSAFE.md` was regenerated and the unsafe diff was
      reviewed explicitly.
- [ ] Bounds, layout, GC visibility, ownership, fallback parity, and malformed
      input behavior are each covered.
- [ ] The ordinary Go reference path was reviewed before performance evidence.
- [ ] Two reviewers approved where an independent second reviewer is available.

## Tests and fuzzing

- [ ] Each new or changed test maps to a distinct contract.
- [ ] Removed or consolidated fuzz targets retain every useful source seed,
      disk corpus entry, and prior crash input.
- [ ] Fault injection or a focused regression proves the changed test can fail.

## Performance and resources

- [ ] Not a hot-path change, or interleaved before/after benchmarks use the same
      compiler and machine.
- [ ] No statistically significant `sec/op` result regresses by more than 2%.
- [ ] No statistically significant `B/op` result regresses by more than 0.01%,
      and `allocs/op` does not regress; retained memory, compile time, and binary
      size do not regress without an approved exception.
- [ ] A specialization has multi-architecture end-to-end evidence and a generic
      fallback; synthetic kernel evidence alone is not used.

## Release material

- [ ] Provenance is updated for imported algorithms, source, or corpora.
- [ ] License or notice changes reflect an explicit maintainer decision.
- [ ] User-facing ownership, limits, toolchain, and stability documentation is
      updated when its contract changes.
