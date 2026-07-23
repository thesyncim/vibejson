# ADR 0002: document-store substrate

Status: accepted and implemented.

## Context

The structural `Index` is efficient for one document, but a document engine
also needs batch ingest, repeated-layout compression, column reads, selective
predicates, and restart without reparsing. Building those features outside the
repository would duplicate the tape, shape, interner, and containment logic and
would lose the existing correctness and unsafe-code gates.

The substrate is evaluated against explicit JSON-store contracts: exact byte
reconstruction, bounded allocation, indexed selectivity, durable recovery,
mutation amplification, and stable throughput across synthetic and real
corpora.

## Decision

Keep the multi-document substrate in this repository:

- `DocSet` owns immutable document sources and structural tapes in append-only
  arenas.
- `ShapeTapes` stores recurring flat-object keys once per proven shape and
  retains only per-document values. Documents whose root fits below 64 KiB use
  8-byte narrow value entries; wider documents use 16-byte entries.
- `ValueDict` records repeated value spans in a shared dictionary while
  preserving byte-exact materialization and random access.
- `Postings` provides top-level existence and exact-containment candidate
  lists. Hashes only prune: every candidate is verified by the exact evaluator.
- `AppendPointer`, typed shape columns, and sparse row gathers expose the data
  without materializing Go object trees.
- versioned `DocSet` serialization stores sources, tapes, shapes, dictionaries,
  and postings in a reopenable image. Formats remain unstable before v1.

The root module keeps no third-party dependency. Corpus-only dependencies
remain isolated in nested modules.

## Required invariants

1. Source bytes and tape entries are immutable for every handle lifetime.
2. Shape fingerprints route work but never authorize a field read. Compact
   shape tapes are created only after exact key-byte conformance.
3. Narrow and wide tapes return the same `Index`, `Node`, and raw bytes as the
   classic representation.
4. Posting and dictionary hashes may add work but cannot change an answer.
5. Buffered column, posting, and query operations allocate zero bytes once the
   caller-provided result and workspace capacities are warm.
6. A serialized image is rejected transactionally on corrupt bounds, counts,
   references, or checksums.
7. SIMD is admitted only for an already-native dense representation and must
   retain a safe portable implementation with identical results.

## Representation choices

The classic tape costs 16 bytes per structural entry and retains the original
JSON spelling. Shape tapes remove repeated object-key entries; narrow values
halve the remaining value-entry width. The value dictionary attacks a different
source of redundancy—repeated complete value spans—without decompression on
read. These mechanisms compose and remain optional because heterogeneous data
can make their ingest or metadata cost unprofitable.

Sparse postings remain sorted ordinals. A native dense bitmap may use the
allocation-free word kernels in `internal/bitset`, but an ephemeral posting
list is not converted merely to reach SIMD: conversion would add a dense build
and result-decode pass to a representation already suitable for linear merge.

## API boundary

This ADR owns storage and execution primitives. ADR 0003 owns the currently
implemented SQL-shaped read interface. ADR 0004 owns keyed mutation, snapshots,
TTL, and online index lifecycle. ADR 0007 accepts the compact document-query
and binary prepared-plan direction, including a bounded join path. Distributed
planning, serving policy, replication, and cross-collection transaction
semantics remain outside this storage ADR.

## Evidence and acceptance

The implementation is held by classic-versus-compact differential tests,
bounded exhaustive representation checks, corrupt-image tests, retained-value
and forced-GC tests, portable/SIMD parity, race and `checkptr` runs, and
allocation-contract tests.

[`docs/store.md`](../store.md) defines mutable Store behavior, ownership, and
operational counters.

## Consequences

The substrate preserves key order, duplicate keys, and exact number spellings
that a decoded-tree store normally discards, and lets a consuming engine shard
by owning multiple sets or Stores. In exchange, options must be chosen before
ingest, compact shape tapes may allocate once if widened through `Doc`, and the
library deliberately does not promise SQL-engine storage or transaction
semantics.
