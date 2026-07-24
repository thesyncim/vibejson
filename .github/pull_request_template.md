## Contract

- [ ] The change states one observable contract and avoids unrelated features.
- [ ] Exported behavior and ownership changes are documented.
- [ ] Generated files and module files are clean.

## Correctness

- [ ] The smallest permanent regression or differential test covers the change.
- [ ] Error, malformed-input, lifetime, and concurrency behavior are covered
      where relevant.
- [ ] Persistence changes include reopen, fault, and recovery evidence.
- [ ] Test and fuzz ownership remains current in `TEST_CONTRACTS.md`.

## Unsafe, SIMD, and memory

- [ ] Not applicable, or `UNSAFE.md` was regenerated and reviewed.
- [ ] Bounds, GC visibility, aliases, fallback parity, and external-memory
      accounting are explicit.
- [ ] SIMD changes preserve a portable path and pass ISA guards.
- [ ] No Go pointer is hidden in an integer or external allocation.

## Performance

- [ ] Not a maintained hot path, or before/after runs use the same compiler,
      machine, inputs, and row count.
- [ ] Time, bytes/op, allocations/op, retained memory, and relevant generated
      code were inspected.
- [ ] A specialization has an end-to-end workload and a route test.

## Documentation and release

- [ ] The affected canonical README, Store, or Contributing document was
      updated.
- [ ] Provenance and notices reflect imported source, algorithms, or corpora.
- [ ] Stability and license claims remain accurate.
