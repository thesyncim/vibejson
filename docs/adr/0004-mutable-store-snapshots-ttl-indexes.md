# ADR 0004: immutable Store publication, TTL, and online indexes

Status: accepted and implemented. Builds on ADR 0002 and ADR 0003.

## Decision

Add a root-package `Store` that serializes mutation and atomically publishes
immutable snapshots. Reader-visible state contains:

- a per-Store-keyed persistent 32-way HAMT from complete key to chunk/slot;
- a persistent 32-way radix vector of immutable chunks;
- at most 64 stable slots per chunk and one dense `DocSet` row table; and
- the logical online-index watermark plus immutable exact-index roots.

Writer-only state contains reusable and indexed chunk-id sets, TTL metadata,
online-index build cursors, and reclamation progress. Readers never consult it.

## Hard requirements

1. `Snapshot.GetRaw` takes no lock, reads no clock, checks no expiry,
   tombstone, or version bit, and allocates zero bytes.
2. Every prior snapshot remains byte-correct after any later operation.
3. Invalid input publishes nothing.
4. Ordinary mutation work is bounded by one chunk and persistent-tree height,
   never total Store size.
5. Delete churn creates no tombstones, delta overlay, or later compaction
   threshold.
6. TTL changes create one live heap node per expiring key, not stale deadline
   generations.
7. Index construction and reclamation are caller-budgeted and observable.
8. Hash collisions may widen work but never change an answer.
9. Declared indexes support nested RFC 6901 paths and ordered compound keys.
10. Buffered exact probes and indexed snapshot queries allocate zero bytes
    after their caller-owned storage reaches its high-water mark.
11. A compiled Store key may bypass hashing and the HAMT only after verifying
    its cached stable slot; movement and cross-Store use fall back safely.

## Publication and ownership

Writers hold `Store.mu`, build a complete next state, and publish it with one
release `atomic.Pointer.Store`. A reader either reaches the complete old graph
or the complete new graph.

The HAMT and chunk vector path-copy only changed radix nodes. Complete hashes
and keys are verified at HAMT leaves. The keyed hash seed prevents a remote
caller from selecting a deterministic collision family; collision chains still
compare the full key.

The key HAMT uses fixed 32-way nodes for its cache-hot first 15 hash bits. A
terminal slot keeps two leaves before promoting the rare third collision to
another node. The two-leaf policy keeps the lookup small enough for the
compiler to inline while avoiding most sparse deep nodes. Delete flattens a
promoted subtree as soon as
it fits the terminal bucket again, so churn cannot leave a permanently expanded
tail.

Document source bytes and structural tapes are independently immutable.
Updating a row validates and copies only its replacement. Unchanged rows in the
new chunk reuse their source and classic tape backing directly; the new chunk
copies only dense row headers and the chunk-relative narrow value slab. Enabled
postings and value dictionaries are rebuilt because their ordinals and ids are
chunk-local.

Compiled shape records are immutable and may also be shared. A rebuild seeds
its shape cache only from records referenced by surviving rows. Reused classic
rows may be promoted through the same repeat-sighting and exact key-byte proof
as ordinary ingest. An updated-away shape is omitted, so optimization state is
bounded by live layouts rather than becoming a version chain. Published narrow
slabs are never reused as writable capacity.

Old storage becomes collectible when the last snapshot and current chunk stop
referencing it. The implementation stores no parent-chunk pointer, version
list, epoch token, or deferred merge obligation.

## Delete behavior

Slots are stable while a key is live. `ord[slot]` maps the slot to the current
dense `DocSet` row. Delete rebuilds the row headers without the removed slot;
all surviving source and tape storage is shared unchanged. Deleting the final
row removes the chunk leaf immediately and builds no empty replacement.

Freed slots and empty chunk ids enter O(1) writer sets and are reused. Radix
traversal descends materialized branches only, so a high historical chunk id
does not turn into scan work. There is no read-time tombstone and no compaction
operation.

## Why bounded immutable chunks

| Strategy | Reader cost | Reclamation behavior | Decision |
| --- | --- | --- | --- |
| Per-key object under a reader lock | lock or retry | immediate | rejected: taxes reads |
| Delta overlay over a base segment | extra lookup and branch | periodic merge | rejected |
| Append-only versions and tombstones | version checks | global compaction debt | rejected |
| Epoch-reused pages | direct read | caller lifetime protocol | rejected for the public Go API |
| Whole-corpus copy | direct read | O(corpus) per write | rejected |
| Bounded chunk plus persistent metadata | direct read | ordinary GC by snapshot lifetime | selected |

`ChunkDocuments` controls the copied row-table bound: 1 for the lowest write
amplification, 8 for write-heavy mixed use, and 64 by default for scan locality
and metadata density. It is a document count, not a byte limit.

## TTL

TTL is publication-based and deliberately absent from snapshots. An indexed
four-ary min-heap contains exactly one mutable node per expiring key, with a
position map for in-place deadline changes and cancellation. `Persist` removes
the node; no stale generation remains to clean later.

`ExpireDue` removes due nodes, groups them by chunk, rebuilds each affected
chunk once, and publishes the whole batch as one generation. `RunExpiry` arms
one timer for the next deadline and sleeps without a ticker when no key
expires. A key remains visible until expiry work publishes its delete, and an
older snapshot retains it permanently.

## Online posting indexes

`AddIndex` publishes a logical `Building` definition. Writes dual-maintain all
active families. `BackfillIndex(k)` resumes from an immutable radix cursor and
examines at most `k` start-snapshot chunks; an already-covered candidate still
consumes budget. Covered chunks use postings, uncovered chunks use the exact
scan fallback, and both return the same answer.

Coverage uses sparse 4,096-id bitmap pages. Empty pages disappear, and `Ready`
releases the captured vector root and collapses coverage to implicit all-live
state. Multiple logical posting names share one physical layer.

`DropIndex` removes the logical definition immediately. After the final
consumer disappears, `ReclaimIndexes(k)` rebuilds at most `k` ids taken from an
O(1) writer set; it never scans the Store merely to discover completion. Old
snapshots continue to own their physical index version through normal GC.

## Declared exact indexes

`CreateIndex` publishes one to four compiled RFC 6901 paths. The definition may
name a top-level column, a nested object field, or an array position. Multiple
paths form one ordered compound key. Missing paths, incompatible traversal
steps, and containers contribute no entry; the four JSON scalar families do.
Compiled pointers share the Store-owned
path spelling and reuse the existing shape/classic `AppendPointerRows` engines,
so index maintenance does not introduce a second JSON traversal implementation.

The chunk is also the index micro-page. Its stable slots are a native `uint64`
posting word; the chunk's dense document ordinal may change after a delete, but
the slot bit does not. This separates scan density from index identity:

- delete rebuilds dense row metadata and clears the old stable bit;
- insert reuses the free bit without a tombstone or remap table;
- update moves only changed slots between tuple postings; and
- an unchanged tuple reuses the complete old index root and catalog slices.

Each tuple fingerprint maps to an adaptive posting. Up to four chunk words live
inside the posting leaf. Wider values promote to a sparse persistent radix
vector. Removal demotes a four-word result back inline and contracts redundant
top radix levels immediately; there is no background bitmap compaction phase.
The fingerprint directory and word vector path-copy only changed routes, so old
Snapshots retain their exact version through ordinary reachability.

Strings use the Store's keyed collision-resistant hash over decoded content;
the tuple fold receives a final low-bit avalanche before HAMT routing. Numbers
use an equality-compatible candidate bucket. Neither is authoritative: lookup
re-resolves every path and applies exact Boolean, decimal, decoded-string, or
null equality before returning a row. A collision therefore costs work but
cannot forge an answer.

Backfill traverses the captured chunk vector in caller-bounded batches and
adds stable words to the persistent root without rebuilding document chunks.
Writes touching an uncovered chunk index its complete next image and mark that
chunk covered before publication. `Building` probes scan the immutable
snapshot exactly; `Ready` probes visit only candidate masks. Dropping a declared
index detaches its root in one publication, and old roots need no explicit
reclamation command because they contain no chunk-local physical layer.

## Snapshot query planning

`query.RunSnapshotInto` binds against the Snapshot's catalog on every run. A
compiled query can therefore precede index creation and survive backfill or
drop without invalidation. Equality leaves bind to matching single-column
definitions. An `AND` first chooses the widest usable compound definition and
then intersects any further indexed conjuncts. An `OR` unions only when every
branch has a sound bound. Exact `NOT` complements against live stable-slot
words. Every mask operation is ordered by chunk and evaluates 64 rows per
machine-word Boolean operation.

Candidate count decides late materialization: at or below half the snapshot,
set bits decode to `(chunk, slot)` rows and existing sparse column gathers touch
only survivors; above half, the engine keeps the dense column scan. The original
predicate remains the semantic authority after candidate pruning. Projection,
aggregation, grouping, stable ordering, and limiting reuse the same execution
code as `DocSet` queries.

## Allocation contract

After caller-owned capacity is warm, snapshots, `GetRaw`, `Range`, buffered
exact/posting probes, sparse Snapshot gathers, compiled Store-key reads,
compiled `query.RunInto` and
`query.RunSnapshotInto`, warmed TTL changes, and native dense Boolean operations
allocate zero bytes.

Mutations allocate the next immutable state. A zero-allocation `Put` or
`Delete` would require overwriting storage visible to an old snapshot or
borrowing caller memory after return, so it is incompatible with this lifetime
contract. The optimization target is bounded allocation proportional to the
replacement plus one chunk's small metadata, not an unsafe zero.

## External source-memory decision

Byte arenas are pointer-free and are not scanned by the Go collector, but they
count toward live heap and its pacing target. A read-only serialized `DocSet`
can already borrow a caller-owned mmap through `Open`; this moves the image out
of `HeapAlloc` while leaving total mapped memory unchanged. The mapping must
outlive the set and every derived borrowed view.

`Store.WriteTo` and `OpenStore` now expose caller-owned external source bytes.
The reopened Store is mutable, but its immutable base source/native tape
sections continue to borrow the complete input image through every derived
state and snapshot. `AppendRaw` and `AppendRawKey` provide caller-buffered,
lifetime-independent copy-out.

Automatic mmap ownership remains rejected. `RawValue.Bytes` is a plain slice
and `Index` also borrows its source, so either can outlive the Store or Snapshot
value that produced it. Finalizer-driven or eager unmapping could invalidate a
live handle. The caller must keep the image mapped until all borrowed values
are dead. A future automatic mode still requires an explicit scoped lease or
owner-bearing handles whose lifetime and size are explicit in the type system.
No unsafe lifetime shortcut is hidden behind `StoreOptions`.

## Mutation evidence

Replacement, delete plus insert, stable-slot reuse, unchanged and changed
index tuples, compiled-key fallback, and retained snapshots are covered by
state-machine and allocation-contract tests. Distribution changes posting
density, so `Snapshot.IndexStats` is the production sizing authority rather
than a fixed bytes-per-document claim.

## Validation

Required gates include randomized map and query differentials across chunk
sizes and representation modes; retained snapshots; caller-input mutation;
live-storage reuse and obsolete-shape release; invalid-input rollback;
address reuse; TTL and index state machines; race, `checkptr`, forced GC,
portable/SIMD parity, and steady-state allocation tests.

Operational semantics, complexity, examples, and tuning live in
[`docs/store.md`](../store.md). Resource ceilings live in
[`docs/contracts/limits.md`](../contracts/limits.md).

## Non-goals

This decision does not add a WAL, crash recovery, replication, eviction,
distributed consensus, cross-Store transactions, multi-core query scheduling,
or a stable post-v1 Store format. The current image format is explicitly
versioned and pre-v1. Those features may consume publication generations but
cannot weaken the reader contract.
