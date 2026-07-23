# ADR 0003: compiled single-table query surface

Status: accepted and implemented. Builds on ADR 0002.

ADR 0007 supersedes this ADR's future front-end and transport direction. The
single-table builder and optional SQL adapter described here remain the
implemented compatibility surface until the compact document plan is built.

## Context

`DocSet` exposes the fast primitives, but applications should not have to
rebuild projection, filtering, aggregation, grouping, ordering, and posting
selection around them. A full SQL engine would put unrelated planning and
distributed policy in the core package.

## Decision

Provide a `query` subpackage for a deliberately bounded, read-only,
single-`DocSet` query:

- path projection;
- `COUNT`, `SUM`, `AVG`, `MIN`, and `MAX`;
- scalar comparisons, containment, `EXISTS`, and null tests;
- `AND`, `OR`, and `NOT` composition;
- `GROUP BY`, stable `ORDER BY`, and `LIMIT`; and
- equivalent SQL text and programmatic builder front ends; and
- one public immutable typed `Plan` below both front ends.

Compilation produces one immutable plan. `Run` is the allocating convenience.
`RunInto` accepts a reusable `Result` and `Workspace`; after their row,
posting, decoded-text, ordering, and group capacities warm, execution allocates
zero bytes.

## Front-end and transport boundary

SQL is syntax, not execution state. `PrepareSQL` parses SQL and the builder's
`Prepare` method lowers builder values into the same `Plan`. The plan retains
compiled pointers, typed constants, numeric path/aggregate/group slots, and
late-bound index probes. It does not retain SQL source, lexer tokens, or the
builder predicate tree. Execution consequently performs no SQL parsing and no
string dispatch.

Schemaless JSON field names cannot disappear without an application-provided
schema: a peer must transmit each path's bytes at least once. They remain cold,
immutable compiled-pointer metadata. Result headers are likewise display and
compatibility metadata, kept separately from the hot operator array. Output
columns have stable ordinal IDs through `Plan.AppendSchema`; result cells are
typed and `Cell.AppendJSON` writes to a caller buffer. A future binary protocol
can therefore encode schema/path bytes once, then carry ordinals, type tags,
constants, bitmap batches, and typed cells without making SQL or formatted
strings its intermediate representation.

ADR 0007 now freezes the higher-level contract: compact document syntax and a
pure-Go builder compile to one minimal binary prepared plan, while SQL becomes
an optional compatibility adapter. Its decoder remains another front end to
the typed executor, not a second executor.

## Execution

Dense predicates and columns stream over compact tapes. A selective DocSet or
heap-Store posting probe runs first and sparse row gathers materialize only
candidates. Those hash-bounded routes recheck the complete compiled predicate,
so a collision can increase work but cannot admit a false result. A
FileStore's persistent exact probe can instead return final masks when a
collision-free scalar or compound certificate proves the complete stream;
legacy, missing, oversized, and collision-marked certificates retain the same
document-recheck fallback.
For an unfiltered one-column scalar `GROUP BY` with `COUNT(*)`, a compact bulk
generation may store an aggregate-only catalog derived from the same exact
certificates. It retains value/count/first-row state per group, not per row;
high cardinality streams across checksummed bounded pages. Ordinary scalar
mutations maintain a one-page cover transactionally, while a segmented cover
currently drops atomically and the executor groups certified postings plus
residual rows. Nested RFC 6901 index paths are eligible. Containers, compound
indexes, uncertified collisions, and a representative that cannot fit one
configured extent remain on the fallback without changing semantics.

Projection, aggregation, and grouping consume typed columns directly.
Shape-taped rows are read at their native narrow or wide width and are not
widened into classic tapes. Group keys use the same byte-exact semantics as the
document layer. Stable ordering retains input order for equal keys.

`Cell` stores raw JSON, decoded text only when the value is a string, and one
tagged numeric/boolean word. Its 64-bit layout is fixed by tests so adding a
value kind cannot silently grow every result row.
Computed aggregates no longer grow or borrow an eagerly formatted number arena:
`Int64`/`Float64` consume them directly and `AppendJSON` formats only when a
text encoding is actually requested. The convenience `JSON` accessor may
allocate for a computed number; its caller-buffered counterpart is the
zero-allocation transport contract.

`RunFileSnapshotInto` extends the same ownership model to durable execution.
The caller's `Result` retains reusable column cells and one packed
variable-width byte arena, so page/workspace storage can be released while
the result remains valid. Reusing or releasing that Result ends the previous
cell lifetime. This removes per-group raw/decoded clones from the direct
catalog path without borrowing the page cache.

## Correctness and ownership

- SQL and builder plans must execute identically.
- Dense and posting-accelerated routes must execute identically.
- Classic, hashed, narrow/wide shape-taped, posting, and dictionary-backed
  `DocSet`s must execute identically.
- Numeric comparison uses exact decimal semantics; it does not route through a
  lossy `float64` conversion.
- Results and workspaces are caller-owned and belong to one executing worker.
  The compiled query is immutable and may be shared.
- Failure leaves caller-owned result prefixes in the documented transactional
  state.

The differential suite compares every accelerated route to the dense executor
and an independent reference model. Allocation tests cover warmed projection,
filtering, containment, grouping, ordering, and aggregation.

## Index interaction

`DocSet.Postings` is a physical acceleration option. ADR 0004 adds logical
Store indexes that backfill it online. A Store snapshot may contain a mixture
of covered and uncovered chunks: covered chunks use postings and uncovered
chunks use the exact scan fallback. Readiness is operational state, never a
correctness precondition.

Sorted sparse posting lists use linear merge/intersection. Native dense masks
may use the internal SIMD Boolean kernel. Transient sparse-to-dense conversion
is intentionally rejected because it adds build and decode passes; a future
route must justify those passes with a planner-visible cost model.

## Evidence boundary

Query evidence uses generator-owned counts, aggregates, result digests,
physical-read counters, and allocation checks. Durable file bytes, live heap,
admitted cache, commit staging, caller workspaces, and process RSS remain
separate accounting domains.

## Non-goals

SQL mutation or DDL, joins, subqueries, window functions, transactions,
multi-table planning, multi-core scheduling, durability, replication, and a
complete SQL dialect are outside this package. Keyed programmatic mutation is
the responsibility of `Store` under ADR 0004.
