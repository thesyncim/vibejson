# Store completion ledger

This ledger separates implemented behavior from accepted design and missing
work. It is a merge checklist, not a performance claim. Machine-specific
timings are not published here; an item moves to “implemented” only with code,
correctness tests, allocation/accounting evidence where relevant, and
user-facing documentation.

## Implemented in this branch

- Keyed bulk build, insert/upsert, replacement, delete, immutable snapshots,
  stable-slot reuse, exact JSON reconstruction, and caller-buffered reads.
- Mutable deadlines: set, change, clear, persist, and bounded expiration.
- Nested one-to-four-column exact hash indexes with online backfill, collision
  certification/recheck, stable-slot bitmap algebra, and durable posting pages.
- Scalar-object containment lowering to exact nested candidates where sound,
  with exact structural recheck as the final authority.
- Optional compiled open/evolving schemas and in-memory named collections.
- Bounded-residency `FileStore`, automatic copy-on-write publication,
  synchronous or grouped asynchronous durability, dual-root recovery,
  deterministic tear tests, generation leases, extent retirement/reuse, and an
  optional scoped pure-Go Linux `io_uring` path.
- Pointer-free/off-heap bulk document and exact-index arenas, bounded page
  cache, compact document groups, structural templates, scalar dictionaries,
  and explicit heap/external/caller memory accounting.
- Authoritative typed numeric sidecars plus dense numeric scan stripes under a
  fixed-page 64-way copy-on-write directory. Covered replacement/delete and
  in-range fitting insert copy one stripe and one bounded path at any depth.
- Low-cardinality transactional and high-cardinality segmented categorical
  grouping covers for the currently eligible grouping shape.
- Current typed query builder and optional SQL adapter for projection,
  predicates, containment, grouping, aggregation, ordering, and limit.
- Reusable result/workspace APIs, extensible versioned result type ids, and
  opaque length-delimited extension cells without enlarging the 56-byte Cell.
- A scoped `internal/orderedkey` foundation: canonical exact-decimal and
  decoded-string order, ascending/descending components, compound equality
  prefixes, fail-closed parsing, and zero-allocation caller-buffered encoding.
- Synthetic scale tests, real corpora, portable/SIMD parity, unsafe inventory,
  generated test contracts, and corruption/recovery campaigns. External
  comparison scoreboards are not part of the repository.

## Accepted design, not yet implemented

- One typed operation algebra covering collection/schema administration,
  reads, mutations, deletes, TTL, index DDL/backfill, relational queries,
  joins, streaming, cancellation, batches, durability waits, compaction,
  statistics, and feature discovery.
- A minimal versioned binary prepared-plan and command encoding shared by a
  zero-overhead pure-Go client. The readable JSON-shaped query and SQL are
  changeable cold adapters, not execution formats.
- Versioned length-delimited output descriptors with required/optional feature
  negotiation for future scalar, nested, vector, and application-defined
  types.

## Required after this merge

### Ordered and multikey indexes

- Add explicit typed field-segment vectors: object key, array index, and array
  wildcard. Do not make dotted strings or RFC 6901 text the durable identity.
- Add ordered-index definitions with per-column direction and multikey policy.
- Build checksummed pointer-free ordered leaves/branches and stable-slot
  postings. Keep large keys and exponent-overflow values in an explicit
  residual bitmap so range results cannot produce false negatives.
- Maintain one bounded copy-on-write root-to-leaf path for insert, replacement,
  and delete. Backfill must be resumable and reader-safe.
- Bind equality prefixes, one ordered range, compatible ordering, early limit,
  nested fields, and multikey `$any`/`$all` semantics.
- Add indexed nested-loop joins and a bounded partitioned-hash fallback.
  Cross-collection snapshots must expose captured generations until a catalog
  commit epoch supplies an atomic database-wide instant.

### Full scans without indexes

- Fuse page admission, field extraction, predicate evaluation, selection
  bitmap, projection, and early limit into one pass.
- Operate on page/column batches, never per-row objects or intermediate result
  slices. Reuse caller-owned output and scratch at steady state.
- Add profitable amd64 and arm64 SIMD typed lanes with a scalar-identical
  implementation and route-parity tests. Inspect generated code; do not add
  assembly merely to claim SIMD.
- Gate changes on synthetic 10K/100K/5M rows and real corpora, recording exact
  result digests, bytes read, cache admission, allocation-contract results,
  retained bytes, and physical plan counters.

### Mutation, concurrency, and durability

- Add a public atomic multi-mutation batch so tree paths, state roots, and
  durability fences amortize together.
- Measure and reduce remaining transient key/index maintenance allocations.
- Profile reader/writer contention under point reads, range scans, index
  backfill, TTL expiry, commit grouping, and long snapshots. Replace locks or
  maps only when profiles, resource accounting, and route-level tests justify
  the change.
- Keep immutable reader publication and bounded single-writer ownership unless
  a multi-writer protocol proves failure atomicity, reclamation safety, and
  better throughput. “Lock-free” is not itself an acceptance criterion.
- Add lower-amplification checkpoint/compaction policy and operational controls
  for long-snapshot retained extents and file high-water.

### Larger than RAM and GC footprint

- Exercise the bounded cache at 100x admitted residency with direct-I/O and
  buffered routes, cold/warm mixes, cgroup limits, and fault injection. Equal
  cold and resident latency is not claimed.
- Continue replacing per-key/per-row Go pointer structures with packed offsets,
  flat tables, bitmaps, and externally accounted arenas. Number of documents
  or keys must not imply a proportional number of GC-visible pointers.
- Publish heap objects, scanned heap, external bytes, mapped/cache bytes,
  staging, retained generations, caller output, and process RSS as distinct
  domains.
- Evaluate CHAMP or another persistent hash structure only against the current
  packed/radix/COW structures on lookup, mutation amplification, pointer count,
  and recovery complexity.

### Query/client and operations

- Implement the typed operation builder and bounded validator before freezing
  any readable syntax.
- Encode field segments, constants, parameters, expression/relation opcodes,
  output descriptors, and capability bits without maps, reflection, or retained
  strings in prepared execution.
- Give point operations fixed payloads; analytical features must add no branch
  or allocation to get/put/delete/deadline hot paths.
- Implement borrowed column batches, caller-owned `Into` variants, byte/row
  credit backpressure, cancellation, and bounded idempotency tokens.
- Keep output types extensible: unknown required semantics fail closed;
  explicitly optional length-delimited values can be skipped safely.

### Remaining product boundaries

- Make the named collection/schema catalog durable before claiming atomic
  multi-collection administration or transactions.
- Incrementally maintain segmented categorical covers after mutation.
- Broaden covering grouping beyond one scalar column and `COUNT(*)`.
- Add replication, archival recovery, point-in-time restore, and multi-process
  ownership only as separately designed features.
- Stabilize page and wire formats only after corruption, compatibility, and
  upgrade/downgrade matrices exist.

## Merge and rename sequence

1. Merge this branch only after the full suite, race targets, vet, generated
   contracts, documentation checks, and GitHub CI pass on the pushed head.
2. Continue the required work from `main`, using focused reviewable changes.
3. Rename the repository and module to `slopjson` in one separate atomic
   compatibility change: repository metadata, module/import paths, package
   docs, examples, CI, badges, generated files, and migration notes must move
   together.
4. Treat the ironic name as no excuse for loose engineering. Trust comes from
   explicit invariants, bounded resources, readable code, failure tests,
   reproducible resource accounting, and honest limitations—not adjectives.
