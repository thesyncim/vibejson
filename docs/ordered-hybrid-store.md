# Ordered hybrid store

This is the promotion specification for the next durable primary and exact
index format. It is intentionally stricter than a design sketch: every number
is labeled as measured, projected, or a gate, and no isolated primitive becomes
the default until the complete store passes the gates at the end.

The target is not an LSM. Readers consult one immutable published generation,
with no memtable, delta, tombstone, version-chain, or merge cursor. Mutations
construct the next canonical pages directly and publish one root. Snapshots pin
old immutable roots; qualified in-place materialization is an optimization, and
copy-on-write is always the safe fallback.

## Why the primary cannot be globally hash-partitioned

Arbitrary hash partitioning and global lexical ranges cannot coexist without a
second ordered copy of every key or a sort at query time. Either choice violates
the read-amplification and space goals.

The winning interpretation of hash routing is therefore:

1. route by shortest distinguishing key prefixes to a lexical tablet and leaf;
2. use bounded hash candidates inside that leaf;
3. confirm every candidate against the complete key;
4. store key/value bytes physically in lexical-rank order.

This follows the useful division in Wormhole-style indexes: prefix hashing
keeps point routing independent of total key count, while ordered leaves retain
exact lower bounds and range scans. Swiss-table control groups are useful
inside a leaf, not as the global ordering structure.

## Canonical generation

```text
StateRoot
  |
  +-- TabletCatalog
  |     shortest distinguishing prefixes -> TabletID
  |
  +-- AnchorMap per tablet
  |     prefix-hash route + exact anchor confirmation
  |     ordered fence vector -> BucketID
  |
  +-- BucketMap
  |     stable BucketID -> generation-specific PageRef
  |
  +-- OrderedHashLeaf
  |     stable hash slots -> lexical ranks -> exact keys/values
  |
  +-- ExactTermIndex
        exact term -> adaptive (BucketID, quadrant, mask) posting tiles
```

The `BucketMap` is canonical indirection, not a read overlay. Stable bucket and
slot identities let secondary postings survive ordinary value updates while
copy-on-write changes physical page references. Removing it would require
rewriting anchors and every affected index posting on each update.

One state root selects the tablet catalog, anchors, bucket map, primary leaves,
and exact-index roots. A snapshot therefore chooses the entire database view in
O(1), and all readers see either the old generation or the new generation.

## Non-negotiable invariants

1. Readers consult exactly one published primary representation.
2. Point reads never consult a range cursor; scans never merge representations.
3. Global iteration, lower bounds, upper bounds, and prefix scans use bytewise
   lexical key order.
4. Hash tags only remove candidates. A hit always compares the complete key in
   the selected leaf.
5. `(BucketID, slot)` is stable across ordinary update and delete and is the
   secondary-posting identity.
6. Slot movement happens only during one bounded split, merge, or slot-class
   rewrite that atomically rebuilds all affected posting tiles.
7. Delete leaves no tombstone, probe-chain obligation, or later compaction work.
8. An active snapshot forces copy-on-write for any bytes it can still observe.
9. Every persistent router, leaf, posting, and allocator structure is
   pointer-free, checksummed, self-describing, and capacity-bounded.
10. False tag matches never scan another leaf, tablet, or table.

## Ordered hash leaf

The existing hash-bucket lab proves the local point core:

| Isolated M4 Max result | Median |
| --- | ---: |
| hit, hash included | 40.2 ns |
| miss, hash included | 50.5 ns |
| four false-tag exact checks | 34.6 ns |
| stable-slot traversal | 2.57 ns/row |
| same-length page rewrite | 1.37 µs |
| growing page rewrite | 2.20 µs |
| allocations | 0 |

Its traversal is `(BucketID, slot)` order, not lexical order. It is evidence for
the hash core only.

The production leaf separates stable hash slots from lexical ranks:

- 256 stable slots for the normal class;
- one control byte per slot: keyed tag plus empty/live state;
- one byte per slot mapping slot to lexical rank, with `0xff` empty;
- compact common key lengths with a rare wide escape;
- one overflow bit per live rank;
- succinct monotone record boundaries;
- key/value heap in lexical-rank order;
- small bounded insertion stash; no tombstones;
- adaptive 4–64 KiB page classes.

Projected structural metadata at 230 live rows:

| Component | Projected B/live key |
| --- | ---: |
| 256 control bytes | 1.113 |
| 256 slot-to-rank bytes | 1.113 |
| 7-bit key lengths | 0.878 |
| overflow bitmap | 0.126 |
| succinct 231 record boundaries | 1.270 |
| 40-byte compact envelope | 0.174 |
| compact bucket reference, anchor share, upper nodes | 0.15–0.20 |
| **Total** | **4.82–4.87** |

This is a projection, not a whole-file claim. Current 64-byte page headers and
32-byte `PageRef`s push it above 5 B/key, so the target requires a compact leaf
envelope and reference. Overflow references, allocator state, roots, and value
bytes must remain visible in the whole-file benchmark.

At 100 billion documents and 200–230 rows per leaf, the store has roughly
435–500 million primary leaves. A macro tablet near one million documents
produces roughly 100,000 tablets. No Go object may exist per key or leaf;
resident navigation must use packed arrays and bounded page-cache frames.

### Point read

```text
snapshot root
 -> prefix-hash tablet/anchor route with exact confirmation
 -> BucketMap page
 -> ordered-hash leaf
 -> two bounded control groups plus optional stash
 -> slot-to-rank
 -> boundary select
 -> exact key compare
 -> inline bytes or bounded overflow extents
```

An inline point read may acquire at most three resident pages. The router and
bucket-map upper pages should normally be resident. An overflow value adds the
explicit extent reads reported by the benchmark.

### Ordered scan

```text
AnchorMap lower bound
 -> snapshot-owned ordered BucketID cursor
 -> one BucketMap resolution per leaf
 -> sequential lexical-rank decoder
 -> physically lexical key/value heap
```

There is no sort, tombstone subtraction, hash permutation, or version merge.
Physical sibling `PageRef`s cannot be the authoritative successor because COW
makes them stale in older generations; successor state comes from the
snapshot-owned rooted cursor.

## Updates, deletes, and structural work

Ordinary updates preserve `(BucketID, slot)`, rewrite one leaf and the
`BucketMap` path, and publish one root. A same-length, projection-neutral update
may use recovery-journaled canonical materialization only when no snapshot can
observe the old bytes. Otherwise it uses COW.

Delete clears the live control byte and compacts lexical bytes immediately.
It writes no tombstone and leaves no probe-chain obligation. Empty leaves are
removed in the same generation.

Runtime inserts use empty candidate slots and then the bounded stash without
relocating published rows. Bulk build may use augmenting placement because no
posting identity exists yet.

Splits and merges are bounded structural transactions:

- a split keeps the old `BucketID` for the left leaf and allocates one for the
  right;
- rows moved right receive new posting tile identities in the same generation;
- adaptive slot classes avoid half-empty 256-slot leaves after ordinary splits;
- merge hysteresis prevents oscillation;
- every affected exact-index tile is rebuilt atomically;
- p50, p95, and p99 structural latency is reported separately.

The tradeoff is real: many secondary indexes make a slot-class rewrite more
expensive. Hiding that work behind an overlay would only move the cost to every
read and is not permitted.

## Exact indexes and deduplication

One canonical term leaf groups exact ordered scalar tuples. Posting identity is:

```text
TileID = (BucketID << 2) | quadrant
posting = (TileID, uint64 live-slot mask)
```

Four quadrants cover a normal 256-slot primary leaf. Singleton, few-mask, run,
sparse, and dense encodings are selected per posting. Repeated payloads share
leaf-local content-addressed bytes only when the dictionary entry is smaller
than the repeated payloads.

Index definitions are canonicalized by ordered paths and semantics. Aliases
with identical definitions share one physical index root; they do not build or
maintain duplicate postings.

The isolated adaptive term leaf currently measures:

| Shape | New leaf | Current exact layout |
| --- | ---: | ---: |
| low-cardinality | 5.09–16.22 B/posting | 32–2,048 B/posting |
| high-cardinality | 25.32–35.32 B/posting | 54–2,070 B/posting |

That space result is excellent, but its ordered traversal remains slower than
the packed current lower bound. It stays isolated until the read regression is
closed. Space savings never excuse slower default reads.

Posting order is stable identity order, not lexical key order. A query that
explicitly requests lexical index results intersects candidates while walking
the ordered primary cursor or sorts its bounded result. The API must state
which order it promises.

## Allocator and locality

The free-space index uses a 64-way hierarchical maximum over the exact
offset-ordered free extent array. It preserves lowest-offset first-fit and adds
only 0.130 B/extent at 46,200 extents. The isolated late 64 KiB lookup falls
from about 14 µs to about 30 ns.

Reopen must make every durable clean free extent eventually reusable, not just
the largest resident prefix. Cold extents remain packed and are promoted in
bounded chunks. Allocation, deletion, and rollback update the hierarchy without
allocations.

Allocator policy must also preserve lexical locality after random churn.
Logically ordered leaves scattered across the file keep correct scans but lose
sequential device behavior. Tablet-local extent classes, bounded clustering,
and explicit offline repack are part of the production benchmark, not deferred
cleanup.

## Promotion gates

The next format replaces the current primary only when the complete durable
store, not an isolated leaf, passes all gates on equivalent corpora:

| Metric | Gate |
| --- | ---: |
| warm random point | ≤300 ns, 0 alloc |
| lexical iteration | ≤6 ns/doc, 0 alloc |
| ordered all bytes | ≤60 ns/doc |
| local hit including hash | ≤45 ns |
| local miss including hash | ≤55 ns |
| async YCSB-B | ≥2.80 M ops/s |
| async YCSB-A | ≥2.15 M ops/s |
| async YCSB-F | ≥1.56 M ops/s |
| async churn | ≥2.23 M ops/s |
| async ordered-scan mix | ≥0.79 M ops/s |
| update p50 / p99 | ≤0.45 / 1.5 µs |
| delete + restore p50 / p99 | ≤0.75 / 1.8 µs |
| structural metadata | ≤5.0 B/live key after measured churn |
| whole-file disk | ≥15% below the best matched production-compressed competitor |
| snapshot creation | O(1), no per-key work |
| snapshot-held read path | same page count and ≤1% latency change |
| post-delete debris | zero tombstones and version records |

The current async baseline is far below the mutation gates. The current Darwin
power-loss-safe pair also trails SQLite by 11–14% across YCSB-B/A/F, churn, and
the ordered-scan mix. Those are explicit open gaps, not projected wins.

## Integration sequence

1. Prove the ordered-hash leaf with exact differential, corruption, occupancy,
   split, stash, lower-bound, prefix, and scan tests.
2. Prove the persistent prefix `AnchorMap` under pathological shared prefixes,
   absent bounds, collisions, splits, and merges.
3. Add compact leaf envelopes and references; simulate 100-billion-document
   bounds before claiming 5 B/key.
4. Introduce one new root containing tablet anchors, `BucketMap`, primary
   leaves, and exact-term roots.
5. Integrate read-only point and lexical range paths first. Stop if either read
   gate fails.
6. Bulk-build final leaves and postings bottom-up. Unsorted input may use
   bounded unpublished scratch, never a runtime LSM.
7. Add ordinary update/delete with stable slots and COW publication.
8. Add split/reclass/merge with atomic posting repair and multi-index p99 tests.
9. Add qualified materialization and automatic combining only after crash-cut
   and snapshot proofs.
10. Run current point, scan, mixed, snapshot, overflow, index, memory, and disk
    matrices with matched production compression.
11. Make the winner the sole default and delete obsolete primary paths. The
    repository is unreleased, so there is no permanent two-engine tax.

## Known weaknesses

- The canonical `BucketMap` costs a lookup, though it replaces the current
  fingerprint-tree plus chunk-directory path.
- Active snapshots force COW and can remove the in-place small-update win.
- Adaptive slot-class changes do bounded but potentially large multi-index work.
- Large incompressible documents lower occupancy or add overflow reads.
- Holding ≤5 B/key under churn requires measured occupancy and merge hygiene;
  a bulk-build number is insufficient.
- The current compact document codec is too slow to be a blanket default.
- Random churn can destroy physical scan locality without an allocator/repack
  policy.
- Isolated leaf numbers omit router, cache, checksum, durability, and index
  maintenance costs; they are evidence, not database-level victories.

## Design references

- [Wormhole: A Fast Ordered Index for In-memory Data Management](https://wuxb45.github.io/papers/wormhole.pdf)
- [Abseil Swiss Tables design notes](https://abseil.io/about/design/swisstables)
- [Faster Go maps with Swiss Tables](https://go.dev/blog/swisstable)
