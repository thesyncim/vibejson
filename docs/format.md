# On-disk format

This document specifies the byte-level on-disk format written and read by
`store/durable` (backed by `internal/storeio`): the superblock, the common
page envelope, every page kind's payload layout, the checksum scheme, and the
commit/crash-recovery protocol. It is derived directly from the encode/decode
functions and their doc comments in `internal/storeio` — every offset, size,
and invariant below is read from that source, not inferred from behavior.

It does not cover API surface, `Options` semantics, defaults, or mutation
behavior (snapshots, TTL, index construction) — see `docs/store.md` for that.
It does not cover the two I/O backends' wire protocol with the kernel
(`internal/storeio/ring_linux.go`, `device_portable.go`) beyond noting their
existence in the commit-protocol section; that is an implementation detail of
how `Device.Commit` gets bytes onto disk, not part of the durable format
itself.

All multi-byte integers are little-endian. All sizes are in bytes unless
stated otherwise.

## Overview

A durable collection file is a graph of fixed-identity, immutable, checksummed
physical pages rooted at one of two alternating superblocks:

```text
Superblock (2 fixed copies, generation-selected)
  -> StateRoot page (one per generation)
       -> ChunkDirectory (packed-radix tree)  -> DocumentPage / DocumentGroup
       -> KeyDirectory   (B+tree)             -> (chunk, slot) locations
       -> IndexDirectory (B+tree)             -> IndexPosting pages
       -> TTLDirectory   (B+tree)             -> (chunk, slot) locations
       -> Float64ScanHead (Float64Catalog B-tree) -> Float64Stripe pages
       -> IndexGroupHead  (IndexGroupCatalog chain)
  -> FreeImage/FreeDelta (free-set log of reclaimed extents; separate Superblock root)
```

Every page is copy-on-write: a mutation never overwrites live bytes. It
allocates a new physical extent, writes the new page(s), and republishes a
new StateRoot that points at the new subtree while structurally sharing every
untouched page with the previous generation. A generation becomes durable by
writing its data pages, taking a data-integrity barrier, then writing one of
the two alternating superblock copies and taking a final barrier — see
"Commit and durability protocol" below.

`LogicalID` gives a page a stable identity across copy-on-write replacement
(so a `PageRef` chain can be diffed/attributed across generations), while its
physical `Offset` changes every time the logical page is rewritten.
`Generation` records which collection generation produced that physical version;
because unchanged subtrees are shared, a live page's `Generation` is often
older than the `StateRoot` that currently references it.

## Superblock

`internal/storeio/superblock.go`. `SuperblockSize = 128` bytes. Two copies
occupy the file's first two physical pages (`superblockCopies = 2`) and
alternate by generation: `SuperblockOffset(generation, pageSize) =
((generation-1) & 1) * pageSize` — generation 1 uses physical page 0,
generation 2 uses physical page 1, generation 3 reuses page 0, and so on. The
rest of each physical page beyond the 128-byte record stays reserved so a
torn write to one copy can never overlap the other copy's page.

```text
 0        8        12       16       20       24
+--------+--------+--------+--------+--------+
| magic  |version | size   | flags  |pageSize|
+--------+--------+--------+--------+--------+
 24                32                40
+-----------------+-----------------+
|   generation    |  ~generation    |
+-----------------+-----------------+
 40                48        52       56       60
+-----------------+--------+--------+--------+--------+
|   stateOffset   |stateLen|stateCRC| ~CRC   |reserved|
+-----------------+--------+--------+--------+--------+
 64                72                80       84       88       92
+-----------------+-----------------+--------+--------+--------+--------+
|    fileEnd      |   freeOffset    |freeLen |freeCRC | ~CRC   |reserved|
+-----------------+-----------------+--------+--------+--------+--------+
 96                              112                  120       124      128
+-------------------------------+---------------------+---------+---------+
|            storeID (16B)      |      reserved (8B)   |  CRC32C | ~CRC32C |
+-------------------------------+---------------------+---------+---------+
```

| Offset | Field | Type | Notes |
| --- | --- | --- | --- |
| 0:8 | magic | `"SJROOT01"` | fixed |
| 8:12 | version | u32 | `2` |
| 12:16 | size | u32 | `SuperblockSize` = `128`, self-describing |
| 16:20 | flags | u32 | must be `0` (no flag bits defined yet) |
| 20:24 | pageSize | u32 | power of two, `>= 4096` |
| 24:32 | generation | u64 | monotonic, `!= 0` |
| 32:40 | ~generation | u64 | bitwise complement, torn-write detector |
| 40:48 | stateOffset | u64 | physical offset of the StateRoot page |
| 48:52 | stateLength | u32 | StateRoot page's physical length |
| 52:56 | stateChecksum | u32 | StateRoot page's CRC32C |
| 56:60 | ~stateChecksum | u32 | complement |
| 60:64 | reserved | — | must be zero |
| 64:72 | fileEnd | u64 | exclusive physical high-water mark, page-aligned |
| 72:80 | freeOffset | u64 | physical offset of the newest `PageFreeDelta` (0 when the durable free set is empty) |
| 80:84 | freeLength | u32 | 0 means the durable free set is empty |
| 84:88 | freeChecksum | u32 | that delta page's CRC32C |
| 88:92 | ~freeChecksum | u32 | complement |
| 92:96 | reserved | — | must be zero |
| 96:112 | storeID | `[16]byte` | binds this file's identity; rejects a page copied from another file |
| 112:120 | reserved | — | must be zero |
| 120:124 | checksum | u32 | CRC32C over bytes `[0:120)` |
| 124:128 | ~checksum | u32 | complement |

Every `u64`/`u32` field that records durable state also stores its bitwise
complement immediately after it (generation, both checksums). `DecodeSuperblock`
rejects a record whose value and its stored complement disagree — this is a
second, independent torn-write detector on top of the CRC32C, since a torn
sector write is likely to leave a value/complement pair inconsistent even in
the rare case a partial write happens to still pass CRC32C over the region it
touched.

**Selection and recovery.** `SelectSuperblock` decodes both fixed copies and
returns the one with the higher `Generation` (after checking both decode
cleanly, are in their correct alternating slot, and share `StoreID`/`PageSize`);
if only one decodes, that one wins. This check alone does *not* read the
referenced StateRoot page, so it is unsafe after a crash.

`RecoverSuperblock` / `RecoverStateRoot` implement the crash-safe form:
newest-to-oldest, for each generation-ordered candidate superblock they read
and CRC-verify the referenced StateRoot page (and the newest free-log delta
page, and —
for `RecoverStateRoot` — decode the StateRoot schema and verify every
top-level `PageRef` it names resolves to a page with matching `StoreID`,
`PageSize`, `Kind`, `LogicalID`, and `Generation`). The first candidate that
passes every check is authoritative; a newest generation whose superblock CRC
is fine but whose referenced state graph is semantically torn (e.g. a crash
between writing data pages and writing the root) falls back to the
preceding, still-fully-valid generation. This is why data pages are always
written and barriered *before* the root: the root is the single physical
switch, so any state a fully-written root points to is guaranteed complete.

## Common page envelope

`internal/storeio/page.go`. Every physical page — regardless of kind —
shares one fixed 64-byte header and one fixed 8-byte trailer.
`PageHeaderSize = 64`, `PageTrailerSize = 8`.

```text
 0        8    10   12 13  14      16       20                24
+--------+----+----+--+--+--------+--------+--------+--------+
| magic  |ver |hlen|K |Fl|reserved|pageSize| payload|generation
+--------+----+----+--+--+--------+--------+--------+--------+
 24                32                40                     56       64
+-----------------+-----------------+----------------------+--------+
|   generation    |    logicalID    |     storeID (16B)    |reserved|
+-----------------+-----------------+----------------------+--------+
 64                                                    pageSize-8  pageSize
+---------------------------------------------------------+--------+
|                  payload ... zero padding ...            | CRC32C |
+---------------------------------------------------------+--------+
```

| Offset | Field | Type | Notes |
| --- | --- | --- | --- |
| 0:8 | magic | `"SJPAGE01"` | fixed |
| 8:10 | version | u16 | `2` |
| 10:12 | headerLength | u16 | `PageHeaderSize` = `64`, self-describing |
| 12 | Kind | u8 | `PageKind`, see table below |
| 13 | Flags | u8 | kind-specific; `0` for every kind except `PageDocumentGroup` |
| 14:16 | reserved | — | must be zero |
| 16:20 | PageSize | u32 | physical extent size, power of two |
| 20:24 | PayloadLength | u32 | payload bytes, excludes header/padding/trailer |
| 24:32 | Generation | u64 | `!= 0` |
| 32:40 | LogicalID | u64 | `!= 0`, stable across copy-on-write replacement |
| 40:56 | StoreID | `[16]byte` | must match the owning collection |
| 56:64 | reserved | — | must be zero |
| 64 : 64+PayloadLength | payload | — | kind-specific, see below |
| 64+PayloadLength : PageSize-8 | padding | — | must be zero; CRC-covered but not re-scanned by readers |
| PageSize-8 : PageSize-4 | checksum | u32 | CRC32C over bytes `[0 : PageSize-8)` |
| PageSize-4 : PageSize | ~checksum | u32 | complement |

`InitPage` clears the destination extent, writes the header, and returns the
exact `[64 : 64+PayloadLength)` payload window for the caller to fill.
`SealPage` (or the internal fast path `sealInitializedPage`, which trusts
`InitPage` already zeroed the padding) computes and writes the trailer.
`OpenPage` re-derives the header, verifies the CRC32C and its complement,
verifies reserved bytes are zero, and returns a capacity-clipped payload view
— padding is checksum-covered but never re-scanned by readers, since the
clipped view makes it unreachable.

### PageKind

```go
const (
	PageStateRoot PageKind = iota + 1 // 1
	PageDocument                      // 2
	PageOverflow                      // 3
	PageChunkDirectory                // 4
	PageKeyDirectory                  // 5
	PageIndexDirectory                // 6
	PageTTLDirectory                  // 7
	PageIndexPosting                  // 8
	PageDocumentGroup                 // 9
	PageFloat64Group                  // 10
	PageFloat64Catalog                // 11
	PageFloat64Stripe                 // 12
	PageIndexGroupCatalog             // 13
	PageFreeImage                     // 14
	PageFreeDelta                     // 15
)
```

| Value | Kind | Purpose |
| --- | --- | --- |
| 1 | `PageStateRoot` | one graph root per generation |
| 2 | `PageDocument` | stable-slot micro-page of up to 64 documents in one logical chunk |
| 3 | `PageOverflow` | one ordered piece of a JSON value too large to inline |
| 4 | `PageChunkDirectory` | packed-radix routing from chunk id to `PageDocument`/`PageDocumentGroup` |
| 5 | `PageKeyDirectory` | B+tree from raw key bytes to `(chunk, slot)` |
| 6 | `PageIndexDirectory` | B+tree from `(indexID, tupleHash, chunk)` to a posting segment |
| 7 | `PageTTLDirectory` | B+tree ordered by `(deadline, chunk, slot)` |
| 8 | `PageIndexPosting` | packed exact-match posting segments for one index |
| 9 | `PageDocumentGroup` | immutable multi-chunk compact/bulk document extent (template+dictionary compression) |
| 10 | `PageFloat64Group` | detached column-major typed float64 sidecar for a `PageDocumentGroup` run |
| 11 | `PageFloat64Catalog` | B-tree directory over `PageFloat64Stripe` leaves (scan accelerator) |
| 12 | `PageFloat64Stripe` | aggregate-only, mask-free dense float64 projection for a chunk range |
| 13 | `PageIndexGroupCatalog` | bounded aggregate-only categorical grouping cover |
| 14 | `PageFreeImage` | one page of the free set's base image, ordered by offset |
| 15 | `PageFreeDelta` | one commit's complete free-set diff, and the link to the previous one |

### PageRef — 32 bytes

Every inter-page pointer in the format uses this one fixed encoding
(`internal/storeio/state_root.go`):

```text
 0                8                 16                24    25  26  28    30      32
+----------------+-----------------+-----------------+-----+---+---+-----+-------+
|     Offset     |    LogicalID    |    Generation   |Length|K |Fl | Aux         |
+----------------+-----------------+-----------------+-----+---+---+-------------+
```

| Offset | Field | Type | Notes |
| --- | --- | --- | --- |
| 0:8 | Offset | u64 | physical byte offset of the referenced page |
| 8:16 | LogicalID | u64 | referenced page's stable logical id |
| 16:24 | Generation | u64 | referenced page's `Generation` (may be older than the referrer's) |
| 24:28 | Length | u32 | referenced page's physical `PageSize` |
| 28 | Kind | u8 | expected `PageKind` |
| 29 | Flags | u8 | `0` except document-group float64-sidecar routing (see below) |
| 30:32 | Aux | u16 | `0` except document-group float64-sidecar routing |

## StateRoot

`internal/storeio/state_root.go`, kind `PageStateRoot`, fixed `LogicalID = 1`
(`StateRootLogicalID`). Fixed-size 256-byte payload (`StateRootPayloadSize`),
current version `5`; versions 2–4 are still decodable and simply have fewer
trailing fields populated (this is the format's only versioning mechanism —
every other page kind has exactly one live encoder/decoder version, sometimes
with an older still-decodable predecessor for the same kind, e.g. document
pages v1/v2 and posting pages v1/v2, described in their own sections).

```text
 0        4        8                16                24                32
+--------+--------+-----------------+-----------------+-----------------+
|version | Options |  DocumentCount |    TTLCount     | NextLogicalID   |
+--------+--------+-----------------+-----------------+-----------------+
 32       36       40       44       48       52                60       64
+--------+--------+--------+--------+--------+-----------------+--------+
|ChunkHW |LiveChks|ChunkDoc|IdxCount|IdxDepth| IndexCatalogHash |FreeHint|
+--------+--------+--------+--------+--------+-----------------+--------+
 64                                96                              128
+---------------------------------+---------------------------------+
|     ChunkDirectory PageRef (32B) |      KeyDirectory PageRef (32B) |
+---------------------------------+---------------------------------+
 128                              160                              192
+---------------------------------+---------------------------------+
|    IndexDirectory PageRef (32B) |      TTLDirectory PageRef (32B) |
+---------------------------------+---------------------------------+
 192                              224                              256
+---------------------------------+---------------------------------+
|  Float64ScanHead PageRef (32B)  |    IndexGroupHead PageRef (32B) |
+---------------------------------+---------------------------------+
```

| Offset | Field | Type | Notes |
| --- | --- | --- | --- |
| 0:4 | version | u32 | `2`–`5`; readers accept all four |
| 4:8 | Options | u32 | bit flags, see below |
| 8:16 | DocumentCount | u64 | live document count |
| 16:24 | TTLCount | u64 | count of keys with an active deadline, `<= DocumentCount` |
| 24:32 | NextLogicalID | u64 | next `LogicalID` to allocate, `> 1` |
| 32:36 | ChunkHighWater | u32 | exclusive upper bound of live chunk ids |
| 36:40 | LiveChunks | u32 | `<= ChunkHighWater` |
| 40:44 | ChunkDocuments | u32 | documents per chunk, `1..64` |
| 44:48 | IndexCount | u32 | number of declared exact indexes |
| 48:52 | IndexMaxDepth | u32 | — |
| 52:60 | IndexCatalogHash | u64 | binds the durable catalog (exact indexes, float64 covering columns, schema); nonzero iff `IndexCount != 0` or `Options` has `StateOptionFloat64Columns`/`StateOptionSchema` |
| 60:64 | FreeChunkHint | u32 | conservative lower bound of a chunk that may have a free slot (fields absent/reserved-zero in v2) |
| 64:96 | ChunkDirectory | `PageRef` | required iff `LiveChunks != 0` |
| 96:128 | KeyDirectory | `PageRef` | required iff `DocumentCount != 0` |
| 128:160 | IndexDirectory | `PageRef` | present iff `IndexCount != 0` |
| 160:192 | TTLDirectory | `PageRef` | required iff `TTLCount != 0` |
| 192:224 | Float64ScanHead | `PageRef` | v4+ only; points at a `PageFloat64Catalog` root |
| 224:256 | IndexGroupHead | `PageRef` | v5 only; points at a `PageIndexGroupCatalog` chain head |

Every populated `PageRef` in the StateRoot must have `Length == PageSize`
(the root's own directories are fixed-size metadata pages), a `Generation`
`<= root.Generation`, and a `LogicalID` in `(StateRootLogicalID,
NextLogicalID)`; the four top-level directory refs must also be pairwise
distinct in both `LogicalID` and `Offset`.

### Options bits

```go
const (
	StateOptionShapeTapes uint32 = 1 << iota
	StateOptionPostings
	StateOptionValueDict
	StateOptionHashKeys
	StateOptionFloat64Columns
	StateOptionSchema
)
```

`StateOptionFloat64Columns` means every live document page carries the
complete configured float64 covering-column catalog. `StateOptionSchema`
means the catalog hash also binds an application-supplied schema definition;
the schema itself is caller configuration, not part of the durable format.
Any bit outside this known set fails closed on decode.

## ChunkDirectory

`internal/storeio/chunk_directory.go`, kind `PageChunkDirectory`. A
packed-radix (Patricia-trie-like) node over 32-bit chunk ids, 6 bits per
level (`chunkDirectoryRadixBits = 6`), so each node has up to 64 children and
`Shift` values are `0, 6, 12, 18, 24, 30` (`chunkDirectoryMaxShift = 30`; at
shift 30 only the low 4 bits of a chunk id remain, so a top node's `Bitmap`
may only use lanes 0–3). `Shift == 0` marks a leaf whose children are
`PageDocument` or `PageDocumentGroup` references; several leaf lanes may
name the same `PageDocumentGroup` extent (its `Aux`/`Flags` may carry the
float64-sidecar routing bits described under Float64Group below), so those
duplicates are exempted from the page's own child-uniqueness check.

The tree's height is variable and is named by the root's own `Shift`: readers
must take it from the page rather than assume `chunkDirectoryMaxShift`. A tree
is only as tall as its live chunk ids require — one level while every chunk
fits a single 64-lane leaf, and one more per factor of 64 — so a root may carry
a non-zero `Prefix`, and a chunk id outside the span it covers is absent rather
than corrupt. Writers raise the height by wrapping the existing root in new
levels when an insert falls outside its span; the height tracks the monotone
chunk high-water mark and so never shrinks. Version `1` pages, which always
spanned chunk 0 to the uint32 ceiling in six levels, are rejected: they are
structurally valid but would make every copy-on-write update rewrite six pages.

```text
 0        4        8                16       17   18       20                  32
+--------+--------+-----------------+--------+----+--------+-------------------+
|version | Prefix |     Bitmap      | Shift  |cnt |  Flags | reserved (12B)    |
+--------+--------+-----------------+--------+----+--------+-------------------+
 32                    +32*count
+---------------------------------------------------------+
|      PageRef[count], ordered by increasing set bit lane  |
+---------------------------------------------------------+
```

| Offset | Field | Type | Notes |
| --- | --- | --- | --- |
| 0:4 | version | u32 | `2` |
| 4:8 | Prefix | u32 | chunk-id prefix this node covers (`chunkID &^ ((1<<(Shift+6))-1)`) |
| 8:16 | Bitmap | u64 | one bit per populated lane (0–63) |
| 16 | Shift | u8 | multiple of 6, `0..30`; `0` = leaf |
| 17 | count | u8 | `popcount(Bitmap)`, `<= 64` |
| 18:20 | Flags | u16 | must be `0` |
| 20:32 | reserved | — | must be zero |
| 32 : 32+32×count | refs | `PageRef[]` | packed in increasing set-bit-lane order (sparse — no empty slots stored) |

**Lookup**: check `Prefix` matches the chunk id's prefix at this `Shift`,
extract `lane = (chunkID >> Shift) & 63`, probe `Bitmap` for that bit, then
`rank = popcount(Bitmap & (bit-1))` selects the packed reference —
one bitmap probe, one popcount, no scan. Non-leaf refs must have `Kind ==
PageChunkDirectory` and exact `Length == PageSize`; leaf refs must have `Kind
∈ {PageDocument, PageDocumentGroup}` and `Length >= PageSize` (document/group
leaves may use a larger power-of-two extent than the tree's metadata quantum).

## KeyDirectory

`internal/storeio/key_directory.go`, kind `PageKeyDirectory`. An ordinary
B+tree over raw key bytes (empty keys are valid), max depth 10
(`keyDirectoryMaxLevel`), max branch fanout 64.

```text
 0        4    5    6        8                              32
+--------+----+----+--------+------------------------------+
|version |Lvl |Flag|  count | dataLength | reserved (20B)   |
+--------+----+----+--------+------------------------------+
```

Header is 32 bytes (`KeyDirectoryPayloadHeaderSize`); `Level == 0` is a
leaf, `> 0` a branch. It is followed by `count` fixed-size records, then the
packed key/lower-bound bytes referenced by those records via cumulative end
offsets (record `i`'s key spans `[recordEnd(i-1), recordEnd(i))` in the
packed key-byte region, with `recordEnd(-1) == 0`) — entries/children must be
strictly ordered by key bytes.

**Leaf record — 24 bytes** (`KeyDirectoryLeafRecordSize`):

| Offset | Field | Notes |
| --- | --- | --- |
| 0:4 | cumulative key-end offset | u32 |
| 4:8 | Chunk | u32, `< chunkHighWater` |
| 8 | Slot | u8, `< chunkDocuments` |
| 9:16 | reserved | must be zero |
| 16:24 | Deadline | i64, Unix nanoseconds (0 = no TTL) |

**Branch record — 40 bytes** (`KeyDirectoryBranchRecordSize`):

| Offset | Field | Notes |
| --- | --- | --- |
| 0:4 | cumulative lower-bound-end offset | u32 |
| 4:8 | reserved | must be zero |
| 8:40 | Ref | `PageRef`, `Kind == PageKeyDirectory`, exact `Length == PageSize` |

## IndexDirectory

`internal/storeio/index_directory.go`, kind `PageIndexDirectory`. A B+tree
keyed by `(IndexID, TupleHash, Chunk)` — `IndexDirectoryKey`. Hashes are
candidate accelerators only; exact scalar/tuple recheck happens above this
layer. 32-byte header identical in shape to KeyDirectory's; branch fanout
capped at 64.

**Leaf record — 56 bytes** (`IndexDirectoryLeafRecordSize`):

| Offset | Field | Notes |
| --- | --- | --- |
| 0:4 | IndexID | u32 |
| 4:8 | Chunk | u32 |
| 8:16 | TupleHash | u64 |
| 16:48 | Posting.Page | `PageRef`, `Kind == PageIndexPosting` |
| 48:50 | Posting.Segment | u16, packed rank inside that page |
| 50:52 | Posting.Flags | u16, only `IndexPostingImmutableBase` (`1<<0`) is defined |
| 52:56 | reserved | must be zero |

**Branch record — 48 bytes** (`IndexDirectoryBranchRecordSize`): same
24-byte `IndexDirectoryKey` prefix (`IndexID`, `Chunk`, `TupleHash` — note
the encoded field order is `IndexID(4) Chunk(4) TupleHash(8)`, not
declaration order) followed by a 32-byte `PageRef` to a child
`PageIndexDirectory` node.

`IndexPostingImmutableBase` marks a posting page shared by several compact
directory entries; online mutation of one entry redirects to an isolated
delta page but keeps the shared immutable base extent, bounding base-page
churn to bulk-build boundaries instead of requiring page-level refcounts.

## TTLDirectory

`internal/storeio/ttl_directory.go`, kind `PageTTLDirectory`. A B+tree
ordered by `TTLKey{Deadline int64, Chunk uint32, Slot uint8}` — deadline
first, chunk/slot only disambiguate ties. Same 32-byte header shape.

**Leaf record — 16 bytes** (`TTLDirectoryLeafRecordSize`): `Deadline`
(i64, 0:8), `Chunk` (u32, 8:12), `Slot` (u8, 12), reserved (13:16). No value
payload — a TTL leaf only records a total order; the corresponding document
and its deadline live in the document page and key directory.

**Branch record — 48 bytes** (`TTLDirectoryBranchRecordSize`): same 13-byte
key prefix (padded to 16) followed by a 32-byte child `PageRef`.

## Free log — FreeImage and FreeDelta

`internal/storeio/free_log.go`, kinds `PageFreeImage` and `PageFreeDelta`. The
durable set of page-aligned physical ranges retired by copy-on-write
publication and not yet safe to reuse. It is not reachable from the StateRoot's
own graph; it is published through the Superblock's own
`FreeOffset`/`FreeLength`/`FreeChecksum`, which name the **newest delta page**.

The representation is a base image plus a chain of per-commit diffs, and it
replaced a B+tree of `PageFreeDirectory` nodes. The tree could persist exactly
one edit per commit, because mutating it costs pages, allocating those pages
changes the free set, and a changed free set changes the tree's shape;
everything a commit reclaimed past the first extent therefore stayed in memory
and was abandoned at the next restart. A flat record breaks that cycle because
it can describe its own allocation: content is fixed once an extent is chosen,
so `d` records need `p = ceil(d/capacity)` pages, allocating those adds at most
`p` more records, and recomputing converges. The writer allocates the delta
pages **last** and encodes them **after** allocating, which is why a commit can
record its complete diff.

**Chain shape.** Each delta names both its predecessor (`Prev`) and the image
the whole chain is relative to (`ImageHead`), so the chain is self-describing
from its newest page alone and needs no StateRoot or Superblock field of its
own. A replay walks `Prev` to the end, loads the image through `Next`, and
applies the collected records oldest-to-newest; for one offset the newest
record wins. When the chain grows longer than the image that would replace it
(`FreeLogMaxChainPages` caps it at 32 pages, `FreeLogMaxImagePages` caps the
image at 16), the writer folds: it dumps a fresh image straight from the
in-memory set, starts a new chain whose single delta names that image, and
retires the pages the old image and chain occupied.

**Image page** — 64-byte payload header (`FreeImagePayloadHeaderSize`): version
(0:4, `1`), flags (4:5), reserved (5:6), record count (6:8, u16), reserved
(8:16), `Next` `PageRef` (16:48), reserved (48:64). Records are 24 bytes
(`FreeImageRecordSize`, one `FreeExtent`): `Offset` (u64, 0:8), `Length` (u64,
8:16), `RetiredGeneration` (u64, 16:24) — the last generation that may still
reach this extent; reuse additionally waits for every active `GenerationLease`
and protected recovery root to advance past it (see "Reclamation" below).
Extents are non-overlapping and ordered by `Offset`, both within a page and
across the chain.

**Delta page** — 96-byte payload header (`FreeDeltaPayloadHeaderSize`): version
(0:4, `1`), flags (4:5), reserved (5:6), record count (6:8, u16), reserved
(8:16), `Prev` `PageRef` (16:48), `ImageHead` `PageRef` (48:80), reserved
(80:96). Records are 32 bytes (`FreeDeltaRecordSize`): operation (0:1),
reserved (1:8), then the same three `FreeExtent` fields at 8:32. `FreeOpSet`
(1) inserts or replaces the extent at that offset — a reclaim, and a coalesced
extent growing, are both this. `FreeOpDelete` (2) removes whatever is at that
offset and carries only the offset, because an extent's offset never moves:
allocation always takes from an extent's tail. Records within one page are
applied in order and are not required to be sorted, since a later record in the
same commit legitimately supersedes an earlier one for the same offset.

## DocumentPage

`internal/storeio/document_page.go`, kind `PageDocument`. One logical chunk
of up to 64 stable-slot documents; `Live` is a 64-bit occupancy bitmap — no
tombstones, no empty-row descriptors, a deleted slot simply clears its bit
and shifts every other row's packed rank. Two live versions: v1 (no typed
columns) and v2 (adds inline float64 covering columns for
`StateOptionFloat64Columns`).

```text
 0        4        8                16       20   21   22       24    25  26 27  28
+--------+--------+-----------------+--------+----+----+--------+----+---+--+----+
|version |ChunkID |      Live       |dataLen |cnt |Flag|recSize |ovfC|f64Cnt|f64B|f64Len
+--------+--------+-----------------+--------+----+----+--------+----+---+--+----+
```

| Offset | Field | Notes |
| --- | --- | --- |
| 0:4 | version | u32, `1` or `2` |
| 4:8 | ChunkID | u32, `< chunkHighWater` |
| 8:16 | Live | u64 occupancy bitmap, `popcount == count`, `<= 64` |
| 16:20 | dataLength | u32, packed key+value byte region length |
| 20 | count | u8, row count |
| 21 | Flags | u8, must be `0` |
| 22:24 | recordSize | u16, `= DocumentPageRecordSize = 8` (self-describing) |
| 24 | overflowCount | u8 |
| 25:27 | float64ColumnCount | u16 (v2 only; v1 reserved zero here) |
| 27 | reserved | |
| 28:32 | float64Length | u32 (v2 only) |

Followed by `count` 8-byte row descriptors (`DocumentPageRecordSize = 8`),
one per row in increasing stable-slot order, each a pair of cumulative
offsets into the packed data region: `[0:4)` cumulative key-end offset,
`[4:8)` cumulative record-end offset with its high bit
(`documentPageOverflowBit = 1<<31`) set when the value is an overflow
reference rather than inline JSON. Row `i`'s key spans
`[recordEnd(i-1), keyEnd(i))` and its value spans `[keyEnd(i), recordEnd(i))`
of the data region (masking off the overflow bit), with `recordEnd(-1) == 0`.

An inline value is exactly its JSON bytes. An overflow value is a fixed
40-byte descriptor (`DocumentOverflowDescriptorSize = PageRefSize + 8`): a
32-byte `PageRef` (`Kind == PageOverflow`) followed by an 8-byte `JSONLength`
u64 giving the value's *complete* decoded length across the whole overflow
chain.

If `float64ColumnCount != 0`, the float64 section follows the packed
key/value data at offset `dataStart + dataLength`: `float64ColumnCount`
columns, each a `(mask u64, dense values)` pair — one `1`-bit per row that
has a finite value in that column (`mask &^ Live == 0`), followed by that
many densely packed 8-byte IEEE754 values in ascending-slot order (NaN and
±Inf are rejected). This is the *raw inline* float64 encoding shared with
`DocumentGroup`'s own column section; it differs from the separately
checksummed `Float64Group`/`Float64Stripe` typed extents described below.

## OverflowPage

`internal/storeio/overflow_page.go`, kind `PageOverflow`. One ordered piece
of a JSON value too large to inline, chained via `Next`.

```text
 0        4    6    7  8        12       16                24
+--------+----+----+--+--------+--------+-----------------+
|version |Flag|Slot|rs|  Chunk |dataLen |     Total       |
+--------+----+----+--+--------+--------+-----------------+
 24                32                                     64
+-----------------+---------------------------------------+
|     Offset      |            Next PageRef (32B)          |
+-----------------+---------------------------------------+
```

Fixed 64-byte header (`OverflowPagePayloadHeaderSize`): version (0:4, `1`),
Flags (4:6, must be `0`), Slot (6, owning document's stable slot),
reserved(7), Chunk (8:12), this piece's data length (12:16, redundant with
`PayloadLength - 64`, cross-checked), Total (16:24, complete value length),
Offset (24:32, this piece's byte offset within the complete value), Next
(32:64, `PageRef` to the following piece; zero `PageRef` marks the final
piece, which must satisfy `Offset + len(data) == Total`). Data bytes follow
at offset 64. Every non-final piece's `Next` must have a strictly greater
`LogicalID` than the current piece — the chain's logical ids increase
monotonically.

## DocumentGroup

`internal/storeio/document_group.go`, kind `PageDocumentGroup`. An
immutable, independently checksummed extent packing **2 or more consecutive
logical chunks** (bulk/compact generations) with structural and value
deduplication: a bounded page-local dictionary (up to 128 entries) for
repeated complete leaf spellings, and a page-local template table that
stores each record's non-varying JSON bytes once and encodes each row as a
list of tokens standing in for its variable leaf values (dictionary
reference, short literal, or long literal). This is *not* general
compression — every literal byte is stored byte-for-byte; only structural
repetition and repeated exact values are deduplicated.

Fixed 64-byte header (`DocumentGroupPayloadHeaderSize`):

| Offset | Field | Notes |
| --- | --- | --- |
| 0:4 | version | u32, `1` |
| 4:8 | FirstChunk | u32 |
| 8:10 | ChunkCount | u16, `>= 2` |
| 10:12 | RowCount | u16 |
| 12:14 | TemplateCount | u16 |
| 14:16 | DictionaryCount | u16, `<= 128` |
| 16:20 | chunk-section bytes | u32, `= ChunkCount * 16` |
| 20:24 | row-section bytes | u32, `= RowCount * 16` |
| 24:28 | key-bytes length | u32 |
| 28:32 | body-bytes length | u32 |
| 32:36 | template-section bytes | u32 |
| 36:40 | dictionary-section bytes | u32 |
| 40:44 | column-section bytes | u32 |
| 44:46 | ColumnCount | u16 (0 when the group instead uses a Float64Group sidecar) |
| 46:48 | Flags | u16, only `DocumentGroupFlagFloat64Sidecar` (`1<<0`) plus its order/logical-delta bit fields, see below |
| 48:56 | decodedBytes | u64, sum of every row's exact decoded JSON length (integrity cross-check) |
| 56:58 | recordSize | u16, `= DocumentGroupRecordSize = 16` |
| 58:60 | chunkSize | u16, `= DocumentGroupChunkSize = 16` |
| 60:64 | reserved | must be zero |

Sections follow the header back-to-back in this fixed order: **chunk
directory**, **row directory**, **key bytes**, **body tokens**, **template
table**, **dictionary table**, **column data** — each section's length comes
from the header fields above, so offsets are computed once at open time.

**Chunk descriptor — 16 bytes** (`DocumentGroupChunkSize`), one per logical
chunk, `ChunkID` strictly `FirstChunk + ordinal`: `ChunkID` (u32, 0:4),
`Live` (u64, 4:12), `firstRow` (u16, 12:14, this chunk's first row's index
into the row directory), `count` (u8, 14, `== popcount(Live)`), reserved(15).

**Row descriptor — 16 bytes** (`DocumentGroupRecordSize`), one per row in
chunk then stable-slot order: cumulative key-end offset (u32, 0:4),
cumulative body-end offset (u32, 4:8), exact decoded `JSONLength` (u32,
8:12), `TemplateID` (u16, 12:14), `Slot` (u8, 14), reserved(15). A row's key
spans `[keyEnd(row-1), keyEnd(row))` of the key-bytes section and its body
token stream spans `[bodyEnd(row-1), bodyEnd(row))` of the body section.

**Body tokens.** Each row's variable leaf values are a sequence of one-byte
tags (one per template "hole"): a byte `< DictionaryCount` is a dictionary
reference; a byte in `[0x80, 0xfe]` (`documentGroupShortLiteral` =
`0x80`..`documentGroupLongLiteral-1`) is a short literal, length
`token-0x80+1` (1–127 bytes), literal bytes follow immediately; byte `0xff`
(`documentGroupLongLiteral`) is a long literal, followed by a uvarint length
and then that many literal bytes. Because `DictionaryCount <= 128 == 0x80`,
the dictionary-id and short-literal ranges never overlap.

**Template table.** A `TemplateCount`-entry directory of cumulative u32 end
offsets (4 bytes each) into the following template-data region. Each
template entry: `values` count (u16, 0:2, number of leaf "holes"), reserved
(2:4), `staticBytes` length (u32, 4:8), then `values+1` cumulative u32 "ends"
into the static-bytes tail that follows — reconstructing a row interleaves
static bytes `[ends[i], ends[i+1])`... with the row's per-hole tokens
(`AppendJSON` does exactly this walk).

**Dictionary table.** A `DictionaryCount`-entry directory of cumulative u32
end offsets (4 bytes each) into the following raw dictionary-value bytes.

**Column section.** Only used when `ColumnCount != 0` (i.e. no float64
sidecar). Per chunk, per column, in that nesting order: `mask` u64 then
dense 8-byte IEEE754 values for each set bit — the same encoding as
DocumentPage's inline float64 section.

**Float64 sidecar routing.** When a bulk/compact generation instead sets
`DocumentGroupFlagFloat64Sidecar` in `Flags`/`PageRef.Flags`, `ColumnCount`
is `0` and the typed columns live in a separately checksummed
`PageFloat64Group` extent, addressed by a bounded delta encoded directly in
the referencing `PageRef`'s `Flags` (u8) and `Aux` (u16) — no second tree
walk is needed to find it. See "Float64Group sidecar addressing" below for
the exact bit layout.

## Float64Group

`internal/storeio/float64_group.go`, kind `PageFloat64Group`. A detached,
independently checksummed, column-major typed extent covering the same
consecutive chunk range as one or more adjacent `DocumentGroup` (or, for
non-grouped compact generations, `DocumentPage`) extents. Column-major
layout means a one-column reduction touches only that column's bytes.

Fixed 48-byte header (`Float64GroupPayloadHeaderSize`): version (0:4, `1`),
`FirstChunk` (4:8), `ChunkCount` (8:10, u16), `ColumnCount` (10:12, u16),
`RowCount` (12:14, u16), chunk-record size (14:16, `= 8`), chunk-section
bytes (16:20, `= ChunkCount*8`), directory-section bytes (20:24, `=
ColumnCount*4`), data-section bytes (24:28), reserved (28:48).

Sections: **chunk directory** — one 8-byte `Live` bitmap per chunk; **column
directory** — one 4-byte entry per column, low 30 bits are the column's
cumulative end offset into the data section, high 2 bits are its
`Float64GroupEncoding`; **data** — per column: one 8-byte `mask` per chunk
(all masks for the column first), then that column's dense values across
every chunk in chunk order, encoded at its selected width.

`Float64GroupEncoding` (per-column, chosen by the writer to be the narrowest
that round-trips every finite value in the column exactly):

| Value | Encoding | Width |
| --- | --- | --- |
| 0 | `Float64GroupFloat64LE` | 8 (raw IEEE754, NaN/Inf rejected) |
| 1 | `Float64GroupUint8` | 1 |
| 2 | `Float64GroupUint16` | 2 |
| 3 | `Float64GroupUint32` | 4 |

### Float64Group sidecar addressing

A `PageDocumentGroup`'s `PageRef` (as stored in a `ChunkDirectory` leaf or a
`DocumentGroupFloat64Sidecar`/`AttachDocumentGroupFloat64Sidecar` call site)
encodes the sidecar's location as a small forward delta rather than a second
pointer, so no additional tree is walked:

```text
PageRef.Flags (u8):
  bit 0       DocumentGroupFlagFloat64Sidecar (1 = sidecar present)
  bits 1-4    order: log2(sidecar pages), sidecar length = allocationQuantum << order
  bits 5-7    high 3 bits of (logicalDelta - 1)

PageRef.Aux (u16):
  bits 0-10   offsetPages: sidecar.Offset = group.Offset + offsetPages * allocationQuantum
  bits 11-15  low 5 bits of (logicalDelta - 1)

sidecar.LogicalID = group.LogicalID + logicalDelta   (logicalDelta = 1..256)
sidecar.Generation = group.Generation
sidecar.Kind = PageFloat64Group
```

`offsetPages` is 11 bits (max `2047`), so `DocumentGroupFloat64MaxForwardBytes
= 2047 * allocationQuantum` bounds how far forward of the document group its
sidecar may be allocated; `logicalDelta` is 8 bits split 3-high/5-low and
ranges `1..256`. This lets many adjacent document groups share one typed
extent purely through arithmetic on their own `PageRef`, with no directory
lookup.

## Float64Catalog (stripe directory)

`internal/storeio/float64_catalog.go`, kind `PageFloat64Catalog`. An ordered
B-tree (`Float64ScanHead` root) mapping chunk ranges to `PageFloat64Stripe`
leaves — a **scan accelerator only**; documented mutation paths clear
`Float64ScanHead` and fall back to the authoritative `Float64Group`
sidecars/`DocumentPage` inline columns rather than keeping this tree
incrementally consistent. Fixed fanout 64, max depth 6
(`Float64DirectoryMaxLevel`).

32-byte header (`Float64DirectoryPayloadHeaderSize`): version (0:4, `3`),
Level (4, u8, `0` = leaf), reserved (5, must be 0), count (6, u8), reserved
(7, must be 0), reserved (8:32). Followed by `count` 40-byte records
(`Float64DirectoryRecordSize`): `FirstChunk` lower bound (u32, 0:4), reserved
(4:8), child/leaf `PageRef` (8:40) — leaves (`Level == 0`) point at
`PageFloat64Stripe` pages of any valid size; branches point at
`PageFloat64Catalog` children with `Length == PageSize` exactly. Entries are
strictly ordered by `FirstChunk`.

## Float64Stripe

`internal/storeio/float64_stripe.go`, kind `PageFloat64Stripe`. An
aggregate-only, mask-free dense projection for a contiguous chunk range —
deliberately omits stable-slot validity masks (only the authoritative
Float64Group/inline columns carry those, for mutation overlay correctness);
a stripe assumes every row it names is present.

Fixed 64-byte header (`Float64StripePayloadHeaderSize`): version (0:4, `1`),
`FirstChunk` (4:8), `ChunkCount` (8:12, u32), `RowCount` (12:16, u32),
`ColumnCount` (16:18, u16), column-record size (18:20, `= 12`), directory
bytes (20:24, `= ColumnCount*12`), data bytes (24:28), reserved (28:64).

**Column directory entry — 12 bytes** (`Float64StripeColumnSize`):
cumulative end offset into the data section (u32, 0:4), value `count` (u32,
4:8, `<= RowCount`), `Float64GroupEncoding` (u8, 8), reserved (9:12, must be
zero). Data section is each column's dense values back-to-back, at its
column's chosen width, in stable row order.

## PostingPage

`internal/storeio/posting_page.go`, kind `PageIndexPosting`. Packs several
independent, uniquely-identified exact-match value streams
("segments") for one declared index into one physical page. Two live
versions: v1 (no per-segment certificate) and v2 (adds a certificate).

32-byte header (`PostingPagePayloadHeaderSize`): version (0:4, `1` or `2`),
`IndexID` (4:8), segment count (8:10, u16), Flags (10:12, must be `0`),
directory-section bytes (12:16, `= count*48`), data-section bytes (16:20),
reserved (20:32).

**Segment header — 48 bytes** (`PostingSegmentHeaderSize`), one per segment,
strictly increasing `StreamID`:

| Offset | Field | Notes |
| --- | --- | --- |
| 0:4 | StreamID | u32, `!= 0`, exact-value dictionary id, strictly increasing across segments |
| 4:8 | FirstChunk | u32 |
| 8:12 | LastChunk | u32, `>= FirstChunk` |
| 12:16 | Rows | u32, total set bits across every entry |
| 16:24 | TupleHash | u64, candidate accelerator only |
| 24:32 | Next.LogicalID | u64, continuation segment's page (0 = none) |
| 32:36 | data offset | u32, absolute payload offset of this segment's certificate+entries |
| 36:40 | data length | u32, `len(certificate) + len(encoded entries)` |
| 40:42 | entry count | u16 |
| 42:44 | Next.Segment | u16, packed rank inside the continuation page |
| 44:46 | Flags | u16, only `PostingSegmentCollision` (`1<<0`) defined — marks a certificate whose hash covers more than one exact value/tuple |
| 46:48 | certificateLength | u16 (v2 only; v1 reserved zero — no certificate) |

**Certificate** (`certificateLength` bytes): an exact scalar or compound-tuple
representative for the stream's hash, or empty (readers must then recheck
documents directly).

**Posting entries** (the remaining `dataLength - certificateLength` bytes):
a variable-length stream of `(chunk delta, 64-bit slot bitmask)` pairs, chunk
ids strictly increasing, entries encoded with a uvarint token:

- singleton mask (exactly one set bit): `token = delta<<7 | slot<<1` (bit 0
  clear) — one uvarint, no trailing word.
- multi-bit mask: `token = delta<<1 | 1` (bit 0 set) — a uvarint followed by
  a raw little-endian `uint64` mask.

The first entry's `delta` is always encoded as `0` regardless of its actual
chunk id (the chunk is instead taken from the segment header's `FirstChunk`);
every subsequent entry's `delta` is `chunk - previousChunk`, required `> 0`.

## IndexGroupCatalog

`internal/storeio/index_group_catalog.go`, kind `PageIndexGroupCatalog`. A
bounded, aggregate-only categorical grouping cover — one entry per
`(IndexID, distinct scalar value)` pair with its row count and one
representative row token; the ordinary `IndexDirectory`/`PostingPage` tree
remains authoritative for exact lookups. Two page forms: legacy
self-contained (v1) and segmented/chained (v2, for high-cardinality indexes
that exceed one page).

**Legacy header — 32 bytes** (`IndexGroupCatalogPayloadHeaderSize`, version
`1`): version (0:4), entry count (4:8, u32), `CoveredIndexes` (8:16, u64
bitmap, one bit per durable index id), `DocumentCount` (16:24, u64), reserved
(24:32).

**Segmented header — 64 bytes** (`SegmentedIndexGroupCatalogPayloadHeaderSize`,
version `2`): same first 24 bytes, then `Next` `PageRef` (24:56, chains to
the following `PageIndexGroupCatalog` page; `Kind == PageIndexGroupCatalog`,
same `Generation` as this page, strictly greater `LogicalID`), reserved
(56:64).

**Entry — 32-byte header + value, 8-byte aligned**
(`IndexGroupCatalogEntryHeaderSize`): `IndexID` (u32, 0:4), value length (u32,
4:8), `Count` (u64, 8:16, rows in this group), `First` (u64, 16:24, stable
row token: `chunk = First>>6`, `slot = First&63`), reserved (24:32). Value
bytes (the exact JSON scalar representative) follow immediately and the
whole entry is padded with zero bytes to the next 8-byte boundary
(`alignIndexGroupCatalog`). Entries are ordered by non-decreasing `IndexID`.
For legacy (v1) pages, every bit set in `CoveredIndexes` must have entries
whose `Count` sums to exactly `DocumentCount`; segmented (v2) pages instead
only require each entry's running per-index total to stay `<= DocumentCount`
— the complete-coverage sum is checked across the whole chain, not per page.

## Checksum and integrity

Every physical page (superblock copy and common page alike) is protected by
CRC32C (Castagnoli polynomial, `crc32.MakeTable(crc32.Castagnoli)`) plus its
bitwise complement stored immediately after it — the complement is a cheap
second independent detector for a sector-level torn or short write that
happens to leave the checksummed region itself unmodified. `PageChecksum`
(`internal/storeio/superblock.go`) is the single algorithm used everywhere;
`internal/storeio/page_checksum_std.go`'s fallback is `crc32.Checksum(data,
pageChecksumTable)` — Go's hardware-dispatched CRC32C.

Coverage:

- **Superblock**: CRC32C over bytes `[0:120)` of the 128-byte record (i.e.
  everything except the trailing checksum/complement/reserved bytes
  themselves).
- **Common page**: CRC32C over bytes `[0 : PageSize-8)` — the full 64-byte
  header, the payload, and the zero padding between the payload and the
  trailer. Padding is checksum-covered even though readers never re-scan it
  directly, so a corrupted padding byte still fails the CRC.

`internal/storeio/page_checksum_simd_amd64.go` and
`page_checksum_simd_arm64.go` (`goexperiment.simd`-gated, Go's experimental
SIMD intrinsics) implement carry-less-multiplication CRC32C folding —
`pageChecksumAVX512` folds four independent 512-bit streams with VPCLMULQDQ
plus a final 128-bit PCLMUL reduction, and a PCLMUL-only 8-stream fallback
exists for pre-AVX-512 hardware. **As currently wired, neither SIMD path is
selected by `pageChecksum`**: both files' own `pageChecksum` function still
calls the standard-library CRC32C (their doc comments state the SIMD bodies
are "directly correctness- and ISA-tested candidates" kept available for a
future dispatch, not an active fast path). This is worth flagging explicitly
since the presence of dedicated SIMD source files could otherwise suggest
they are already on the hot path.

## Commit and durability protocol

`internal/storeio/committer.go`, `write_transaction.go`,
`generation_leases.go`, `device.go`. Three cooperating layers:

**`Device`** (`device.go`) is the synchronous, single-owner physical I/O
boundary: `Commit(pages []Write, root Write) error` writes every data page,
takes a data-integrity barrier, writes the root (superblock) page, takes a
final barrier, and returns only after all four steps complete *in that
order* — this ordering is the entire torn-write defense: a crash before the
root write leaves the previous generation's superblock (and everything it
transitively references) fully intact and selectable, and a crash after the
root write but before its own barrier is caught by `RecoverSuperblock`
falling back to the still-valid preceding generation. `validateCommit`
additionally requires the data-page `Write`s to be sorted by offset,
non-overlapping, using distinct staging buffers, and not overlapping the
root's own physical range. Two interchangeable backends implement this
interface — `BackendIOUring` (`ring_linux.go`, pure-Go Linux io_uring) and
`BackendPortable` (`device_portable.go`, positional writes plus
`fdatasync`/`File.Sync`); `BackendAuto` (the default) tries io_uring first
and falls back to the portable backend when the OS/sandbox rejects it. Both
implement the identical `Commit` ordering contract above; neither changes
the on-disk format, only how bytes reach the platform.

**`WriteTransaction`** (`write_transaction.go`) is the copy-on-write
allocator for one generation: `Allocate` reserves a new page-aligned physical
extent (append-only past `FileEnd`, or reused from the caller-supplied
`Reusable` free-extent list when one large enough exists), assigns it a new
or caller-specified `LogicalID`, and returns a `TransactionPage` whose
`Stage()` verifies the fully-encoded page and records its write. `Publish`
takes the transaction's staged state-root page plus the newest free-log delta
page (which is the previous generation's when the free set did not move, and
zero when the durable free set is empty), sorts every staged page by physical
offset, encodes the double-root `Superblock` record via `SetSuperblock`
(which also selects the correct alternating fixed slot for this
`Generation`), and hands the whole batch to the `Committer`.

**`Committer`** (`committer.go`) turns synchronous `Device.Commit` calls into
an asynchronous pipeline: one serialized producer calls `Begin`/`Publish`,
one private background goroutine is the sole `Device` owner and consumer. It
supports **group commit** — coalescing several adjacent published
generations (`GroupLimit`, `CoalesceDelay`) into one durable `Device.Commit`
call, publishing only the newest generation's root in the group and
correspondingly suppressing the older intermediate root writes
(`SuppressedRootWrites`/`SuppressedRootBytes` in `CommitterStats`) while
still writing every generation's data pages. `PublishedGeneration` tracks the
newest generation accepted by `Publish`; `DurableGeneration` tracks the
newest generation whose root has passed the final barrier — `Wait`/`Flush`
block until a target generation reaches durability.

### Reclamation

A retired physical extent (`FreeExtent{Offset, Length, RetiredGeneration}`)
cannot be reused until every reader that might still resolve it has moved
on. `GenerationLeases` (`generation_leases.go`) is a fixed-capacity table of
active reader snapshots; `Minimum(current)` returns the oldest generation any
lease still protects (or `current+1` if none are active). `ExtentReclaimer`
accumulates retired extents (`Retire`/`RetireBatch`, overlap-checked) and
`AppendReusable(dst, currentGeneration, oldestRecoveryGeneration)` releases
into the caller's reuse pool only the extents whose `RetiredGeneration` is
strictly below both the reader floor and the oldest generation a crash
recovery might still need to select — i.e. an extent is reusable only once
no live reader *and* no still-selectable superblock generation can reach it.

## See also

- `docs/store.md` — public API, `Options` semantics and defaults, mutation
  and query behavior, snapshots, TTL, index construction.
- `docs/provenance.md` — externally derived source/algorithm inventory; the
  on-disk format itself is original to this repository and has no
  provenance-ledger entry (the Roaring-inspired posting/candidate execution
  strategy it stores is ledger entry `ALGO-ROARING-001`, but that entry
  covers in-memory execution, not this file format).
- `UNSAFE.md` — safety invariants for the `unsafe`-using code that
  interprets these bytes.
