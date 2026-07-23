# Store

`Store` is an in-memory keyed JSON collection with updates, deletes, immutable
snapshots, TTL, declared single/compound indexes, and wildcard postings.
`FileStore` is the incremental, crash-safe, bounded-residency sibling for a
caller-owned file. They share JSON validation, stable 64-slot chunks, exact
scalar semantics, and the compiled query layer; they intentionally have
different ownership and index-lifecycle surfaces.

`StorePageReader` and `StorePageDB` preserve a smaller page-file contract:
fixed-cache reads plus durable insertion, replacement, and deletion in a
`Store.WritePageFile` checkpoint. They are useful as a specialized I/O
baseline, but their format does not support TTL, secondary indexes, overflow
values, or extent reuse. Their hash-routed page-key directory is therefore
intentionally separate from `FileStore`'s variable-key tree.

The zero value is ready to use. Options are frozen by the first `Put`,
`CreateIndex`, or `AddIndex`.

```go
store := simdjson.NewStore(simdjson.StoreOptions{
	ChunkDocuments: 8, // zero selects 64
	ShapeTapes:      true,
})

created, err := store.Put("user:42", []byte(`{"name":"Ada","team":"compiler"}`))
if err != nil {
	return err
}
_ = created

view := store.Snapshot()
raw, ok := view.GetRaw("user:42")
```

## Collections and optional schemas

`Store` is the physical collection boundary: it already owns one keyspace,
snapshot sequence, TTL directory, index catalog, and serialized writer.
`Collection` adds an immutable name without adding a collection id or pointer
to each row. `Database` is a concurrency-safe in-memory catalog:

```go
schema, err := simdjson.CompileStoreSchema(simdjson.StoreSchemaDefinition{
	Root: simdjson.SchemaObject,
	Fields: []simdjson.StoreSchemaField{
		{Path: "/id", Types: simdjson.SchemaInteger, Required: true},
		{
			Path: "/profile/name",
			Types: simdjson.SchemaString | simdjson.SchemaNull,
		},
	},
})
if err != nil {
	return err
}

var database simdjson.Database
users, err := database.CreateCollection("users", simdjson.StoreOptions{
	ShapeTapes: true,
	Schema:     schema,
})
if err != nil {
	return err
}
_, err = users.Put(
	"user:42",
	[]byte(`{"id":42,"profile":{"name":"Ada"}}`),
)
```

Holding `users` keeps all CRUD, snapshot, query, TTL, and nested/compound-index
operations on the ordinary Store path. The catalog lock and name lookup occur
only during catalog operations. Different collections may use the same key.
Collection options are validated and frozen when the collection is created.
`DropCollection` unlinks the name but cannot invalidate handles or snapshots
already acquired; recreating the name creates an independent empty collection.

Schema compilation owns and canonicalizes its paths. `Required` distinguishes
an absent path from a present `null`; include `SchemaNull` in the type mask to
accept null. `SchemaInteger` accepts number spellings without a fraction or
exponent and does not impose an `int64` range. Paths may address nested object
members and array elements. Unspecified fields remain legal: this is an
evolving-document contract, not a closed-row or full JSON Schema
implementation.

Validation is fused into the structural-index build before shape compaction,
so Store, StoreBuilder, FileStore, and page-backed writes do not parse valid
input twice. A constraint failure publishes nothing. Bulk FileStore creation
gathers each field across a complete 64-slot source page instead of resolving
one row at a time. `ValidateIndex` and successful field checks allocate zero
memory.

Nil `StoreOptions.Schema` preserves the specialized schemaless path. The
schema pointer lives once in collection configuration and is excluded from
every immutable generation, preserving the pre-schema state size class.
Allocation-contract tests require successful validation with caller-owned
scratch to allocate nothing; mutation tests cover both schemaless and compiled
schema paths.

Schema identity is durable. `WriteTo` embeds the definition and `OpenStore`
recompiles and revalidates it. FileStore and the bounded page format bind the
identity in their durable root; callers must provide the same compiled schema
when reopening them. `Store.WriteFileStore` validates a schemaless source
against the target FileStore schema before writing. Schemas cannot be changed
in place: bulk-rewrite into a collection with the new contract.

`Database` itself is currently an in-memory catalog, and every FileStore,
Store image, or page file represents one durable collection. There is no
cross-collection transaction or atomic durable multi-collection catalog claim.
The rationale and complete flow matrix are in
[ADR 0006](adr/0006-collections-and-compiled-schema.md).

## Command reference

| Operation | Result | Complexity |
| --- | --- | --- |
| `CompileStoreSchema` / `ValidateIndex` | compile nested type/required constraints; validate an existing index | one-time path compile / O(constrained paths), zero allocation on successful validation |
| `Database.CreateCollection` / `Collection` / `DropCollection` | create, resolve, or unlink one named Store | catalog lock + name-map operation; held handles bypass the catalog |
| `NewStoreBuilder` + `Append` + `Build` | bulk-validates unique keys into final pages; publishes one Store | O(total input + transient key-radix construction) |
| `Put(key, json)` | `created=true` on insert; validates and copies input | O(replacement bytes + one chunk's metadata + radix height) |
| `Delete(key)` | true when the key existed | O(one chunk's metadata + radix height) |
| `Snapshot()` | immutable current view | O(1) |
| `GetRaw(key)` / `Snapshot.GetRaw(key)` | borrowed exact JSON bytes | O(heap-directory depth or mapped group probes + full-key check) |
| `Get(key)` / `Snapshot.Get(key)` | borrowed navigable `Index` | same lookup; first compact-tape widening may allocate |
| `CompileKey(key)` | caches seeded hash and verified stable slot | one ordinary key lookup |
| `GetRawKey` / `GetKey` | compiled-key read with safe full-lookup fallback | O(chunk radix height) on a stable-slot hit |
| `AppendRaw` / `AppendRawKey` | append exact JSON into caller storage | same lookup + O(value bytes); zero allocation with capacity |
| `Range(fn)` | visits live keys in chunk/slot order | O(materialized radix nodes + live keys) |
| `WriteTo(w)` | stream one full immutable checkpoint/export | O(image bytes + manifest metadata) |
| `OpenStore(image)` | validate and open a mutable Store borrowing an image | O(keys + eager page metadata + exact-index rebuild) |
| `SetTTL`, `SetDeadline` | true when the key existed | O(log4 expiring keys) |
| `Persist` | true when an expiration was removed | O(log4 expiring keys) |
| `ExpireDue(now, limit)` | number of due keys published as deleted | heap work + one rebuild per affected chunk |
| `CreateIndex(definition)` | publishes a 1-4 column exact scalar index | O(1) DDL publication |
| `AddIndex(name, Postings)` | publishes the wildcard posting family | O(1), except shared coverage copy |
| `DropIndex(name)` | detaches one logical definition | O(index-catalog size) publication |
| `BackfillIndex(name, k)` | examines at most `k` start-snapshot chunks | exact: O(k × live slots × columns); wildcard: O(k bounded chunk builds) |
| `ReclaimIndexes(k)` | rebuilds at most `k` physically indexed chunks | O(k bounded chunk builds) |
| `AppendIndexRows/Masks/Keys` | exact lookup through one declared index | O(posting chunks + exact collision checks) |
| `AppendIndexBitmap` / `AppendLiveBitmap` | append one dense stable-slot word per logical page | O(page high-water + exact lookup work) |
| `AppendStoreBitmapAnd/And3/Or/AndNot` | combine dense caller-owned workspaces | O(shortest or longest input words), zero allocation with capacity |
| `query.RunSnapshotInto` | late-bound indexed query over a snapshot | candidate masks + selected-column work |
| `Store.WritePageFile` / `OpenStorePageReader` | write/open the specialized fixed-cache checkpoint | full export / bounded page-cache open |
| `StorePageDB.Put` / `Delete` | durably insert, replace, or delete a checkpoint key | copied page paths + synchronous barriers |
| `CreateFileStore` / `OpenFileStore` | create or lazily recover a durable page graph | bounded root/page scratch; no corpus walk on open |
| `Store.WriteFileStore` | create one compact mutable FileStore generation | one pass over live rows + bottom-up directories + two durability fences |
| `FileStore.Put` / `Delete` | publish a copy-on-write durable generation | changed document plus copied metadata paths |
| `FileStore.SetDeadline` / `Persist` / `ExpireDue` | mutate the persistent deadline tree | copied key/TTL paths; due work is caller-bounded by `limit` |
| `FileStore.Snapshot` / `FileSnapshot.Close` | acquire/release a generation lease | O(1); the lease fences physical extent reuse |
| `FileSnapshot.AppendRaw` | copy exact JSON into caller storage | key-tree lookup + touched document/overflow pages |
| `FileSnapshot.AppendIndexMasks/Into` | probe a frozen exact index with collision-safe certification or document recheck | posting chunks + uncertified/colliding candidates; `Into` reuses caller workspace |
| `FileSnapshot.AppendIndexCandidateMasksInto` | append a hash-bounded superset for an engine that will recheck | index/posting pages only; never a final answer |
| `FileSnapshot.AppendIndexScalarGroupsInto` | group one frozen scalar index from a compact categorical cover or certified postings | O(distinct groups) on a clean covered generation; otherwise posting streams plus residual JSON rows; warmed caller buffers allocate zero |
| `FileSnapshot.ReduceFloat64Path` / `ReduceFloat64PathsInto` | reduce frozen numeric covers without parsing JSON | one compact stripe walk, or an overlay-aware typed-extent walk after mutation; fused paths and warmed caller buffers allocate zero |
| `FileSnapshot.RangeRawBuffer` / `RangeRawReadAheadBuffer` / `RangeMasksRawBuffer` | ordered serial, bounded direct-read-ahead, or sparse stable-slot scan | touched document/overflow pages; zero allocation after caller scratch warms |
| `FileStore.Flush` | wait until the visible generation is crash-safe | queued storage work and durability barriers |
| `query.RunFileSnapshot` | late-bound persistent-index pushdown, parallel bounded batches, external ordered/group spill | O(candidate input + merge), or full input when unbounded; final result storage is caller-owned |
| `query.RunFileSnapshotInto` | execute into a reusable caller-owned `Result` | same plan as `RunFileSnapshot`; column cells and packed value bytes reuse their observed high-water capacity |

A non-positive `ExpireDue`, `BackfillIndex`, or `ReclaimIndexes` limit means all
currently eligible work. Event loops should normally use a positive limit.

## Mutation semantics

`Put` copies caller input and validates the copy before publication. The caller
may reuse the input after return. Invalid JSON, invalid frozen options, or
schema violation returns an error and changes no snapshot. A failed new-key
write takes key ownership only after document validation, so it does not retain
or transiently clone the rejected key. Updating an existing key preserves its
TTL. `Delete` removes the TTL with the document and returns false for a missing
key.

Writes are serialized. Each successful write builds a complete next graph and
publishes it through one atomic pointer store. An update parses only the
replacement document. Unchanged rows reuse their immutable source and
structural tape storage; the changed chunk copies its dense row headers and
chunk-relative narrow value slab. Published slabs are never reused as writable
capacity.

Deletes remove the row instead of recording a tombstone. Surviving document
storage is shared unchanged, freed slots and empty chunk ids are reused, and
deleting a chunk's final row removes the leaf without building an empty chunk.
There is no compaction command or compaction threshold.

`ChunkDocuments` bounds the row metadata touched by one ordinary mutation:

- `1`: lowest write amplification, highest per-document metadata;
- `8`: useful for write-heavy mixed workloads;
- `64`: default, best scan locality and metadata density.

The limit counts documents, not bytes. Measure with the application's document
sizes and option set.

## Bulk construction

Use `StoreBuilder` when the initial corpus is already keyed and no reader needs
to observe each individual insertion:

```go
builder, err := simdjson.NewStoreBuilder(simdjson.StoreOptions{
	ShapeTapes: true,
	Postings:  true,
})
if err != nil {
	return err
}
if err = builder.CreateIndex(simdjson.StoreIndexDefinition{
	Name:  "tenant_country",
	Paths: []string{"/tenant", "/profile/geo/country"},
}); err != nil {
	return err
}
for next() {
	if err := builder.Append(key, document); err != nil {
		return err
	}
}
store, err := builder.Build()
```

`Append` validates and copies the JSON and clones the key; caller buffers may
be reused as soon as it returns. Keys must be unique. An invalid document or
duplicate changes no committed row. The builder belongs to one goroutine and
closes after `Build`; the returned Store is safe for concurrent use and has the
same update, delete, TTL, snapshot, and index behavior as any other Store.
`CreateIndex` may be called before or after appends. Its one-to-four nested or
compound paths are extracted at `Build`; the returned index is `Ready` in the
first reader-visible generation, with no scan fallback window.

The builder fills final micro-pages and mutates only unpublished chunk radix
nodes. Its duplicate guard is not a per-key HAMT: one pointer-free `uint64`
slot packs a 24-bit hash fingerprint with a 40-bit row ordinal+1. A fingerprint
candidate resolves the original chunk key and compares all bytes, so
collisions cannot create a false duplicate. The table grows geometrically,
reserved insert/lookup is zero-allocation, and `Build` drops it before
publishing the compact mapped key directory. `Build` then freezes the graph and
performs one publication instead of path-copying it once per row. Bulk-build
tests assert duplicate rejection, failure atomicity, ready-on-first-publication
indexes, and equality with the online mutation path.

Index construction reuses the same per-page tuple extraction, fingerprinting,
exact-recheck contract, stable-slot masks, and immutable bulk radix builders as
online backfill. It does not maintain a parallel bulk-only index format.

### Mutation cost model

A replacement copies only the affected immutable micro-page and the radix
spine needed to publish it. Delete plus insert additionally updates the key
directory and may reuse a stable slot. Exact indexes and TTL state are
copy-on-write only when their logical value changes. Tests assert that failed
validation, duplicate insertion, and write errors publish no partial state.

## Snapshot and borrowing rules

`Snapshot` captures one state pointer and remains valid after later updates,
deletes, expiry, backfill, or reclamation.

| Read | Writer lock | Clock/TTL check | Steady allocation | Returned lifetime |
| --- | --- | --- | --- | --- |
| `Snapshot` | none | none | zero | independent immutable view |
| `GetRaw` | none | none | zero | borrows the snapshot graph |
| `GetRawKey` | none | none | zero | compiled stable-slot hit; borrows the snapshot graph |
| `Range` | none | none | zero | callback key/value borrow the snapshot graph |
| buffered exact/posting probes | none | none | zero with sufficient `dst` | rows, masks, and keys borrow the snapshot graph |
| warmed `query.RunSnapshotInto` | none | none | zero | result borrows its result/workspace storage |
| `Get` | no writer lock; shape cache may lock | none | first widening may allocate | index borrows the snapshot graph |

Do not modify returned bytes. Keep the snapshot, or a derived handle that pins
it, alive while using a borrowed `RawValue`, `Index`, `Node`, or key. Holding old
snapshots while repeatedly updating hot keys intentionally retains the old
immutable versions; bound snapshot age or count at the application boundary.

## Zero-allocation boundary

With sufficient caller-owned capacity, `Snapshot`, `GetRaw`, `GetRawKey`,
`AppendRaw`, `AppendRawKey`, `Range`, `AppendPointer`, `AppendPointerRows`,
buffered exact/posting probes, compiled `query.RunInto`/
`query.RunSnapshotInto`, and warmed TTL deadline changes allocate zero bytes.

`Put` and `Delete` allocate the new immutable publication. A zero-allocation
mutation would have to borrow caller memory after return or overwrite storage
that an older snapshot can still read. The API does neither. Supplying an input
or result buffer removes transient application allocation where documented; it
does not transfer ownership of published Store memory back to the caller.

For a repeated key, compile once. The handle caches the Store's seeded hash and
the key's current stable `(chunk, slot)` address:

```go
key := store.CompileKey("session:7")
raw, ok := store.Snapshot().GetRawKey(key)
```

The fast path verifies the live bit. Within the exact generation captured at
compile time, the publication identity proves the slot's complete spelling;
after any publication it rechecks the current spelling or resolves through the
key directories. Delete/reinsert, an initially absent key, and cross-Store use
therefore fall back safely. A handle stores no chunk pointer, so keeping it does
not pin an obsolete document page. Both ordinary and compiled reads allocate
zero bytes.

## Store images and mapped source bytes

Heap-built Store source arenas are `[]byte` objects. They contain no pointers,
so the GC does not scan each JSON byte, but they still count as live heap and
therefore affect the pacer and heap target. A mapped image lowers Go
`HeapAlloc`; it does not make those bytes disappear from RSS or the Store's
total memory footprint.

`Store.WriteTo` emits a Store-native container of the existing bounded `DocSet`
page images plus a checksummed tail manifest. It records effective options,
stable slots and keys, generation, reusable empty page ids, ready nested or
compound index definitions, wildcard posting consumers, TTL deadlines, and the
optional canonical schema. There is no second JSON, tape, or query
representation. `OpenStore` recompiles a persisted schema and validates rows
in micro-page batches before publishing the graph.

This is a full checkpoint: each call writes every live micro-page. It is not a
per-mutation persistence requirement or an incremental durability protocol.
Mutations made after `OpenStore` are not written back into the borrowed image.
Applications can checkpoint periodically for backup or faster restart; a
durable primary store still needs the append-only page/root commit path below.

`OpenStore(image)` validates the complete directory before publishing a
mutable Store. Source bytes and native tape sections view `image` directly.
On supported Unix systems, one process-seeded Swiss-style key directory and
one 32-byte-per-row record directory live in anonymous pointer-free mappings,
outside Go `HeapAlloc`; their eight-control-byte group probes use native SWAR
and every hit still verifies the complete key. A chunk holds only Store-wide
owners and base ordinals, rather than a string header and two slice pointers
per row. Post-build/open changes use the existing immutable HAMT only as a delta.
TTL locations are packed integers, so the deadline heap and position map retain
no key strings. Exact-index bases are rebuilt into packed pointer-free posting
pages; construction still uses transient Go scratch, while the retained base
lives outside `HeapAlloc` on supported Unix systems. Distinct shape records
remain Go objects. Later `Put`, `Delete`, TTL, and index operations publish
ordinary immutable generations. A Store image cannot contain a `Building`
index: finish or drop the definition before calling `WriteTo`.

For a file-backed image, map it read-only and pass the mapped slice to
`OpenStore`. The caller owns the mapping and must keep it immutable and mapped
until the Store, every retained `Snapshot`, and every derived `RawValue`,
`Index`, or `Node` are unreachable. Do not unmap based only on dropping the
current Store variable: later states and old snapshots may still share base
pages. `AppendRaw(dst, key)` and `AppendRawKey` provide lifetime-independent
copy-out and allocate zero bytes when `dst` has enough capacity. Automatic
unmapping and finalizer-based ownership remain deliberately absent.

The image is a startup/off-heap boundary, not a larger-than-memory engine;
`FileStore` below is the explicit bounded-residency surface. Open accounts for
mapped document, key, row, and index bytes separately from Go heap bytes.
Mapped source bytes add no steady ownership wrapper. Allocation-contract tests
cover keyed reads, compiled reads, and caller-buffered copy-out; cold access
still pays the operating system's page-fault and storage costs.

### Dense Boolean workspaces

`StoreMask` is the compact interchange form for selective predicates: a sorted
`(chunk, uint64)` stream omits empty pages. A repeated or less-selective plan can
instead call `StoreBitmapWords`, fill reusable buffers with
`AppendIndexBitmap`/`AppendLiveBitmap`, and combine them with
`AppendStoreBitmapAnd`, `AppendStoreBitmapAnd3`, `AppendStoreBitmapOr`, or
`AppendStoreBitmapAndNot`. The word index is the logical page id and each bit is
a stable slot, so no row decoding occurs until `AppendBitmapRows` or
`AppendBitmapKeys` consumes the final candidates.

Every append form is allocation-free with sufficient caller capacity and
supports exact in-place Boolean execution. Three-way AND is fused to avoid an
intermediate write/read pass. Pinned Go 1.27 `GOEXPERIMENT=simd` builds use two
independent 256-bit vectors per loop on amd64. GOAMD64 v1/v2 performs one
process-constant AVX2 capability branch per bitmap call and otherwise executes
the scalar reference; v3+ calls AVX2 directly. All amd64 levels retain the
unrolled scalar loop below eight words, where the two-vector body cannot run.
Generated-code CI verifies that
the v1/v2 dispatch contains no vector instruction before the guard and that
the vector bodies retain `VPAND`, `VPOR`, `VPANDN`, and `VZEROUPPER`. Arm64
keeps the scalar implementation until a separate vector dispatch is justified.
Sparse page-id merges remain scalar because converting a selective stream to
dense words merely to reach SIMD adds a dense materialization pass.

`WriteTo` streams the image without materializing document copies. Persistence headers, endian
scratch, nested page offset rebasing, and at-most-64-row page manifests use
writer-owned fixed storage; only the top-level reference list, Store manifest,
and writer object allocate. The manifest must be buffered to checksum before a
generic `io.Writer` receives it.

## Attached `FileStore`

`FileStore` implements the explicit-I/O path that the mapped checkpoint does
not: a bounded CLOCK page cache with a buddy span allocator, bounded
asynchronous read/prefetch queues, copy-on-write mutations, alternating durable
roots, persistent free extents, and generation leases. `CreateFileStore`
requires an empty caller-owned file; `OpenFileStore` reads only the two
superblocks and referenced root pages. It does not enumerate keys, chunks,
postings, TTL records, or free extents during open. The free tree is loaded
lazily when a writer first needs it.

`Store.WriteFileStore` is the bulk path. It borrows one immutable Store state,
repacks live rows into the target `ChunkDocuments` geometry, writes exact JSON
and overflow chains once, and builds key, chunk, nested/compound exact-index,
and TTL trees bottom-up. One data/tree fence precedes one alternate-root fence;
there is no per-row durable generation, load commit arena, retired history, or
mandatory compaction pass. Index postings from this generation dense-pack
multiple streams per page and carry an immutable-base flag. A later mutation
redirects only the changed stream to an isolated page and never retires the
shared base. Regression tests churn those streams and corrupt the newest
online state root to verify both non-overlap and fallback to the compact
generation.

Every page carries Store identity, kind, logical id, generation, exact bounds,
and CRC32C. Metadata uses 4 KiB pages. Document and overflow extents may use
larger configured power-of-two pages. The resident arena is divided into the
metadata-page quantum, and each page consumes exactly `Length/PageSize`
contiguous slots. Four 4 KiB directories plus one 16 KiB document therefore
fit in a 32 KiB budget; the former maximum-frame layout admitted only two such
logical pages. The lookup table and 64-byte frame controls contain no Go
pointers. A resident hit locks only its own frame, so independent pages do not
serialize on the admission/eviction lock. Free power-of-two spans use intrusive
pointer-free buddy lists. Admission splits or coalesces without scanning the
arena; under pressure it performs at most one CLOCK pass instead of a whole
free-span scan after every victim. Key, chunk, exact-index, TTL, and free trees
are path-copied; unchanged pages remain shared. The commit device writes data
pages, executes a data-integrity barrier, writes the alternate superblock, and
executes the final barrier. Recovery accepts the newest root only after its
state and top-level page graph validate, otherwise it uses the previous root.
Crash-image tests stop the sorted data phase immediately before, inside, and
after every changed physical page, then tear every byte prefix of the root
record. They verify primary JSON, TTL, and exact-index state and require
recovery to return one whole generation, never a mixture.

The acknowledgement boundary matters:

- with `Synchronous`, a successful mutation has crossed both storage barriers;
- without it, publication is reader-visible but only generations at or below
  `DurableGeneration` are crash-safe; `Flush` and `Close` wait for that bound;
- after interruption, recovery selects the newest generation whose
  superblock, state root, and top-level directory identities validate, or the
  preceding valid generation;
- corruption discovered below the bounded open-time roots fails the operation
  closed when that page is admitted; open does not spend O(database) I/O
  proving every cold leaf.

This is a single-file failure-atomic contract, not replication or archival
recovery. It still depends on the operating system and storage device honoring
the requested data-integrity barriers. Destruction of both root copies,
firmware that lies about flush completion, media loss, and rollback to an
arbitrary historical point require external replicas or backups. Deterministic
tearing is a release gate; longer real-device power-cut and recovery-fuzz
campaigns remain additional evidence rather than a completed guarantee.

Allocator tests fill a large quantum geometry, then repeatedly release and
reacquire maximum spans while asserting zero steady allocation. This isolates
span control from CLOCK victim selection and device latency.

Page construction is serialized. With `Synchronous`, `Put`, `Delete`, TTL
changes, and expiry return only after durability, but the caller waits outside
the construction lock: concurrent synchronous writers can build their
generations in order and then share one device fence. `Close` first prevents
new publications, waits for those durability waiters, and only then releases
the committer and cache. Without `Synchronous`, mutations return after the
bounded committer accepts the reader-visible generation; `DurableGeneration`
can lag and `Flush` or `Close` fences it. `CommitCoalesce` optionally gives the
background worker a bounded window to combine adjacent generations under the
latest root. Publication does not wait for that window; a synchronous caller
does, so latency-sensitive single-operation durability should leave it zero.
Only the newest state-root page in such a group can be selected by the newest
superblock. The worker omits older state-root writes and reports them through
`Stats.SuppressedRootWrites` and `SuppressedRootBytes`; it deliberately keeps
every data, directory, posting, and value page because a live snapshot of an
intermediate reader-visible generation may still reference those physical
versions.
Queue and buffer exhaustion applies backpressure. A background device error is
sticky. On Linux the native backend uses the scoped pure-Go `io_uring`
substrate for both durable commits and speculative reads; portable positional
I/O remains the fallback.

Cache misses can independently select `FileStoreReadBuffered`,
`FileStoreReadDirectTry`, or `FileStoreReadDirectRequire`. Direct modes reopen
the same Linux inode through `/proc/self/fd` with `O_DIRECT`; they do not mutate
the flags or lifetime of the caller-owned descriptor. Durable writes
independently select `FileStoreWriteBuffered`, `FileStoreWriteDirectTry`, or
`FileStoreWriteDirectRequire`. The direct writer is a second owned descriptor
used by either the positional or pure-Go `io_uring` commit device. It preserves
data-page/barrier/root/barrier ordering while keeping sustained writes from
populating the kernel page cache. The anonymous arenas and every page
offset/length are at least 4 KiB aligned. The 128-byte checksummed superblock
record is cleared and written as one complete configured page, satisfying
direct-I/O alignment without changing its decoded format. Try falls back only
for an unsupported platform/filesystem; `Stats.DirectReads` and
`Stats.DirectWrites` report what actually happened, and Require fails
construction instead. Direct I/O is a residency control; whether it wins
latency depends on device, filesystem, queue depth, grouping, and locality.

Native speculative reads use one OS-thread-owned ring. `ReadQueueDepth` fixes
its maximum SQ/CQ batch independently from `ReadConcurrency`, which bounds the
portable fallback worker set. `IORING_OP_READ` targets already reserved bytes
inside the page cache's stable mmap arena, so it adds neither a staging copy nor
long-term registered-buffer pins. The ring retains no Go-heap pointer. Cache
identity, CRC32C, and optional typed validation run before a completion changes
a frame from loading to ready. Duplicate demand misses wait on that same frame.
If speculative loads temporarily occupy every victim, demand and dirty
publication wait for one in-flight completion and retry; genuinely leased or
dirty exhaustion still returns `ErrPageCachePinned`. `Stats.ReadBackend`,
`AsyncReadBatches`, and `LargestReadBatch` distinguish the actual engine and
submission geometry. Auto falls back when ring setup is unavailable; a required
native setup fails construction. A later ring-accounting failure resets every
affected loading frame before the worker switches to positional reads.

`FileSnapshot` is an explicit generation lease and must be closed. Point reads
copy into caller storage because an evictable frame cannot safely back an
unowned `RawValue`. `AppendRaw` is zero-allocation on a resident inline hit
when the destination has capacity. `RangeRaw` visits chunk/slot order with one
document lease at a time. `RangeRawBuffer` also accepts and returns the one
reusable overflow buffer, making warmed overflow scans allocation-free.
`RangeRawReadAheadBuffer` is the cold `O_DIRECT` scan lane: it discovers a
bounded chunk-ordered window, submits those extents in physical order, and
still invokes callbacks in exact chunk/slot order. The window consumes at most
one half of the resident budget, 64 extents, and the configured prefetch queue.
Its parallel cap is `ReadQueueDepth` for the native issuer or four requests per
portable worker. Buffered files stay on the serial lane so user-space
scheduling does not fight kernel readahead. The prefetch queue closes under the
same admission lock as nonblocking submission, avoiding per-wakeup heap objects
while making shutdown race-free.
`RangeMasksRawBuffer` applies strictly ordered sparse stable-slot masks and
preserves exactly the order a filtered full scan would have produced. Dead and
zero bits cannot invent rows; a non-zero unknown chunk fails closed instead of
silently applying a stale or cross-snapshot mask. `PrefetchKeys` deduplicates
and physically orders document extents before submitting bounded read-ahead.

Snapshot age has no time-based background cost. One open `FileSnapshot` uses
one preallocated lease slot and a small handle; it does not copy the database
or pin every page in the cache. Pages touched by its reads remain ordinarily
evictable and can be reread because the generation lease prevents their
physical extents from being reused.

Writers do not wait for an old reader, but copy-on-write extents retired at or
after that reader's generation cannot become reusable. Space pressure is
therefore proportional to mutation churn and copied extent sizes, not elapsed
minutes. `Stats.ActiveSnapshots`, `OldestSnapshotGeneration`,
`PendingRetiredExtents`, `PendingRetiredBytes`, and `FileEnd` make the pressure
observable. If the fixed `MaxRetiredExtents` budget is exhausted, the next
mutation returns `ErrRetiredExtentCapacity` before publication. Closing the
snapshot lowers the reader floor; a subsequent writer can move safe retired
extents into the reusable free tree. File high-water does not shrink
automatically, but later writes consume that reusable space. `FileStore.Close`
also returns an active-lease error until every snapshot is closed. Applications
with sustained writes should bound snapshot age as well as
`MaxSnapshotLeases`.

Exact index definitions are frozen in `FileStoreOptions.Indexes`; their catalog
hash is part of the durable root. Each mutation updates the affected
`(index, tuple hash, chunk)` posting page in the same transaction. Posting
format v2 may carry one validated scalar or compound-tuple representative.
When every bit in a stream has the same exact tuple, one semantic comparison
between the query and that certificate proves the complete bitmap. If a second
tuple shares the 64-bit hash, the writer sets a sticky collision flag and the
probe reopens the candidate documents. Missing certificates in version-one
pages take the same fallback. Correctness therefore never depends on collision
probability, and alternate string escapes or equivalent JSON number spellings
compare by value. `AppendIndexMasksInto` retains the directory, copied
document, and parse-tape high-water marks in a caller-owned
`FileIndexWorkspace`; with sufficient output capacity and resident pages, a
warmed probe allocates zero bytes. `AppendIndexCandidateMasksInto` intentionally
skips the document pass and returns a superset for engines that immediately
recheck; using it as a final answer is incorrect. TTL stores the deadline beside
the key and in an ordered persistent tree; replacement preserves the deadline
and ordinary reads never consult the clock.

Clean bulk generations may also carry a categorical group catalog. It is an
aggregate-only derivative of existing single-column exact indexes, not another
per-row index: each covered index stores one exact scalar representative,
count, and earliest stable-slot token per distinct group. Missing paths merge
with explicit JSON `null`, matching query semantics. Equivalent number
spellings and escaped strings merge by value, not source spelling. RFC 6901
paths may be nested; eligibility is attached to the exact index id rather than
restricted to top-level fields.

Every catalog page is a power-of-two extent between `PageSize` and
`MaxPageSize`. A low-cardinality cover uses the original self-contained page.
High cardinality uses a checksummed, physically/logically ordered chain of
bounded pages, and one index may cross page boundaries. Cardinality therefore
increases page count rather than demanding one giant extent. The chain shares
one 64-bit covered-index bitmap and document count; readers validate global
entry order and the requested index's exact total before returning it. A
container-valued row, uncertified/colliding posting, compound index, or single
representative too large for `MaxPageSize` makes only that index ineligible.
Its query then streams the authoritative exact posting tree and reads JSON
solely for missing, container, legacy, oversized-certificate, or collision
rows.
`AppendIndexScalarGroupsInto` exposes both lanes with caller-owned result,
residual-mask, and workspace buffers. The compact cover retains O(groups)
records and no per-row pointers; its warmed Store-layer scan allocates zero.

The group catalog is published in the same checksummed state root as the
posting tree. TTL-only publications preserve it. Ordinary scalar inserts,
updates, and deletes transactionally rewrite a one-page cover in O(groups);
semantic no-ops reuse it byte-for-byte. The affected index alone declines when
a new value is a container, the rewritten summary exceeds `MaxPageSize`, or a
delete removes a still-populated group's recorded earliest row, whose successor
cannot be reconstructed from aggregate-only state. A mutation over a segmented
cover currently retires its complete coalesced chain and falls back to postings
and residual rows. Stale counts are never consulted in either case.

Numeric covering definitions are separately frozen in
`FileStoreOptions.Float64Columns` as exact RFC 6901 paths. The catalog supports
at most 256 paths and participates in the same durable catalog hash, so reopen
fails closed when the order or spelling changes. Ordinary document pages keep
one stable-slot validity mask per path followed by only its finite values.
Missing, null, non-numeric, NaN, and infinity have no set bit.

A compact generation separates mutation authority from aggregate locality:

1. a detached typed group covers consecutive stable-slot chunks, retains their
   masks, and can be shared by document groups across a bounded micro-region;
2. integer-only runs use exact unsigned 8-, 16-, or 32-bit lanes, while
   negative, fractional, or wider values use IEEE float64; and
3. an aggregate-only scan stripe copies the dense covered values—without masks
   or JSON—into physically consecutive extents named by a 64-way ordered
   directory in the state root.

The stripe covers every clean bulk-built chunk, including ordinary and
overflow-backed document pages; it does not depend on document-group
continuity. A predicate-free aggregate therefore walks directory leaves and
dense value extents directly instead of walking the chunk tree or admitting
document pages. Each fixed-size directory node carries at most 64 ordered
lower bounds; a leaf points to stripes and a branch points to older or current
immutable children. The authoritative sidecars remain update-safe. A
replacement whose configured numeric projection is semantically unchanged
reuses the scan directory byte-for-byte. A changed value or delete reconstructs
the stripe containing the touched chunk from authoritative sidecars and the
peeled document page, then copy-on-write replaces the stripe and its bounded
root-to-leaf path. Untouched stripes and subtrees remain shared and the dense
read path gains no overlay branch. An insert takes the same path when its chunk
is already covered and the rebuilt stripe still fits. An out-of-range insert,
an emptied stripe, or a rebuilt stripe that exceeds `MaxPageSize` clears and
retires the complete accelerator before falling back to the chunk tree. A
touched chunk is peeled to an ordinary document page while untouched chunks
continue to share their detached sidecar. The sidecar is retired only after
its final chunk mapping disappears. Rebuilding a compact generation restores
the dense accelerator after fallback or widespread churn.

Every typed page is checksummed independently. Admission rejects masks outside
live rows, invalid adaptive encodings, non-finite general values, unordered
directory bounds, invalid child levels or generations, incomplete chunk
coverage, and a frozen column-count mismatch. The scan root is published in
the same state-root generation as its document graph, so recovery can select
neither a stale stripe nor half of a directory path. A forced multi-level
churn test covers path replacement, historical snapshots, delete, reopen
recovery, physical reuse, and zero-allocation warmed reduction.
The page cache performs common framing/CRC32C and complete document-schema
validation once, before publishing a loaded frame. Hot point reads, scans,
index rechecks, and covering reductions then use an admitted borrowed view
instead of checksumming and decoding the immutable page again. Each consumer
still checks that the document's encoded `ChunkID` matches the selecting
chunk-tree edge, closing the cross-tree substitution case even when a forged
page carries a valid checksum and an otherwise in-range chunk id.

`ReduceFloat64PathsInto` preflights every requested path before reading an
extent and fuses all configured paths into one ordered walk.
Returning `false` therefore means no partial scan occurred and a query engine
can choose one coherent fallback. A warmed scan with caller-owned aggregate and
path slices allocates zero bytes. Portable and Go-native SIMD reducers use the
same fixed four-lane accumulation order, so dispatch and adaptive integer
widths cannot change result bits. The writer's fixed pointer-free mutation
scratch is reported by `Stats.Float64ScratchBytes`; compact-build count and
encoding arrays are transient and pointer-free.

`query.RunFileSnapshot` late-binds the frozen catalog before starting workers.
It chooses the widest matching compound equality index, avoids redundant
overlapping single-column probes, and can intersect `AND` or union a completely
bounded `OR`. File `NOT` currently stays on the full-scan lane because its
complement universe would require an independently fallible page walk. An index
read or validation error is returned; corruption never silently changes the
plan into a scan. Row-producing plans use hash-bounded candidate masks to drive
`RangeMasksRawBuffer`, then evaluate the ordinary predicate over every
survivor. This single document pass supplies exact recheck while preserving
source order, LIMIT ties, and grouped first-row ordering. A fully indexed
`COUNT(*)` instead asks the exact probe for final masks: certificates decide
non-colliding streams and only ambiguous streams reopen documents.
An unfiltered one-column scalar `GROUP BY` with `COUNT(*)` uses the matching
single-column exact index. A clean categorical cover answers in O(groups)
without posting or document pages; after mutation, certified posting groups
are accumulated directly and only residual rows use the compiled JSON pointer.
Both lanes retain first-row group ordering and exact null, number, and decoded
string equality.

Selected raw pages enter bounded arenas, build batch-local indexes in parallel,
and merge in source order. Projection ordering and group state spill as sorted
temporary runs after the configured memory frontier; multi-pass merge opens at
most 32 runs. Temporary files are removed on success and error.
`FileExecutionStats` exposes `RowsTotal`, `RowsScanned`, `IndexBounded`,
`IndexLookups`, `IndexPostingPages`, `IndexCertificateRows`,
`IndexRecheckRows`, `CandidateRows`, `CandidateChunks`, `IndexGroupedRows`,
`IndexGroups`, and `CoveringColumns`, so plan selection is observable rather
than inferred from latency. Consecutive directory entries that select one
packed posting page share one lease and decode. Exact `COUNT(*)` over a fully indexed
equality or object-containment predicate popcounts final masks directly.
Containment compilation can flatten a nested object made entirely of scalar
leaves into exact path equalities, including a matching compound index.
Duplicate needle members retain last-wins semantics. Arrays and empty objects
are not flattened because their structure is part of the answer. An unfiltered
single-row aggregate made only of `COUNT(*)` and configured
`SUM`/`AVG`/`MIN`/`MAX` paths bypasses workers, JSON admission, parsing, and
transient value/validity columns. Multiple numeric paths share one typed-extent
walk.
`COUNT(path)`, filtered aggregates, multi-column or non-count grouping, and
partially covered plans retain the general executor. A reusable
`FileExecutionWorkspace` retains index-planner and overflow scratch.
`RunFileSnapshotInto` additionally reuses a caller-owned `Result`: its column
cell arrays and packed variable-width byte arena retain their observed
high-water capacity, and `Result.Release` drops that storage after an
exceptionally broad query. Direct catalog grouping stops cloning one raw and
one decoded heap object per group; after warm-up the 100K-row/32-group path
materializes the complete owned result at zero allocations. Reusing a Result
invalidates its previous cells. Worker batches remain execution-owned. The
memory target excludes the returned `Result` and cannot make one oversized
document smaller.
Parallel floating aggregation has deterministic batch order but, like other
parallel engines, may differ in the last rounding bits from a strictly
row-at-a-time sum.

Query tests assert physical work, not wall-clock claims: exact-index and
certificate-decided plans report posting pages, JSON pages, rechecks, and
selected rows; covered numeric and grouping plans report typed/catalog pages
and zero JSON reads. Caller-provided workspaces and result storage are checked
for zero steady allocation once capacity is sufficient.

The planner does not use a fixed selectivity threshold. It currently learns
exact posting cardinality only after paying the index lookup, so abandoning the
probe for a scan can pay both paths. A future crossover decision requires
durable cardinality estimates or an online cost model whose inputs and failure
behavior are part of the file format contract.

### Capacity planning for 1 TiB

Heap `Store` owns its complete corpus and is not presented as a
1 TiB-on-64-GiB database. External arenas reduce GC scanning but remain real
resident memory. Use `Store.Stats` and `Snapshot.IndexStats` to account the
actual key, document, index, TTL, retained-generation, heap, and external
domains for a workload.

`FileStore` decouples corpus size from Go heap: `ResidentBytes` fixes admitted
page capacity, while queues, buffers, snapshot leases, and retirement records
have separate finite options. This makes a 1 TiB file structurally possible; it
does not establish acceptable performance. The explicit Linux storage-pressure
gate now places 21,347,320 source key+JSON bytes behind a 200,704-byte cache
(106.4x); the physical high-water is 120,057,856 bytes. It reopens twice and
checks a complete ordered scan, distant reads, update, delete, and a changed TTL
under eviction with direct reads and writes active. The gate records Go heap,
current and peak RSS, page faults, cache admissions, and evictions. Run it with:

```text
SIMDJSON_FILESTORE_100X=1 \
  go test . -run '^TestFileStoreHundredXResidentSmoke$' -v -count=1
```

This proves bounded-cache correctness and measures total process residency for
source data above the configured resident page budget. The separate physical
gate compiles before entering a 64 MiB cgroup and checks allocated blocks, not
only logical file length:

```text
scripts/run-filestore-physical-scale.sh
```

The Linux/ARM64 Docker resource gate stored 2,137 large documents with one
nested exact index: 6,713,852,053 live key+JSON bytes and 6,920,364,032
allocated file bytes under a 52,576,256-byte complete cgroup peak, or 127.7x
and 131.6x peak. File high-water was 6,923,669,504 bytes. Required direct
reads and writes remained active through reopen, distant and nested-index
probes, update, delete, and changed TTL. The synthetic 3 MiB-value fixture
deliberately keeps row count manageable, so it is a residency and correctness
proof rather than a latency result.

Neither gate is an equal-latency claim. Directory and posting pages compete
with documents for the same cache, cold random access pays storage latency,
and copy-on-write generations, allocator rounding, overflow pages, index
cardinality, and free-space headroom add disk amplification. A 1 TiB
deployment still needs workload-specific working-set sweeps, amplification,
latency-percentile, and fragmentation measurements.

Direct writes are optional: their purpose is to prevent write traffic from
displacing the read working set, not to promise lower latency for small
commits. Read-ahead depth is explicitly tunable. Direct and native validation
tests skip when the host cannot provide the requested feature and always retain
the portable path as the semantic reference.

## TTL

TTL metadata is writer-only: one pointer-free packed `(chunk, slot)` plus a
deadline in the indexed four-ary heap, and one integer-keyed position entry per
expiring row. It retains no key string. Changing a deadline updates that node
in place; `Persist` removes it. Delete removes the entry before its stable slot
can be reused, so repeated changes do not create stale generations or require a
slot-generation pointer.

```go
store.SetTTL("session:7", 30*time.Minute) // ttl <= 0 deletes immediately
store.SetDeadline("session:7", deadline) // a past deadline deletes immediately
store.Persist("session:7")               // remove expiration only

ctx, cancel := context.WithCancel(context.Background())
defer cancel()
go store.RunExpiry(ctx, time.Millisecond)
```

Expiration is publication-based:

- a due key remains visible until `ExpireDue` or `RunExpiry` publishes delete;
- an older snapshot keeps the key permanently;
- `TTLAt` may return a negative duration while publication is pending; and
- ordinary reads never call `time.Now` or inspect TTL state.

`ExpireDue` groups due keys by chunk, rebuilds each touched chunk once, and
publishes the batch as one generation. `RunExpiry` uses one timer for the next
deadline and sleeps without a ticker when the heap is empty. `NextExpiration`
is the integration point for an external event loop.

## Declared exact indexes

`CreateIndex` declares one to four RFC 6901 paths. One path is a column index;
multiple paths form one order-sensitive compound key. Paths may descend nested
objects or arrays, and use normal pointer escaping (`~0` for `~`, `~1` for
`/`). Missing paths, incompatible traversal steps, and containers are omitted.
`null`, booleans, exact JSON numbers, and decoded strings are indexed.

```go
info, err := store.CreateIndex(simdjson.StoreIndexDefinition{
	Name:  "tenant_country_status",
	Paths: []string{"/tenant", "/profile/geo/country", "/status"},
})
if err != nil {
	return err
}
for info.State != simdjson.StoreIndexReady {
	info, err = store.BackfillIndex(info.Name, 64)
	if err != nil {
		return err
	}
}
```

Each chunk is a bounded 64-slot micro-page. A posting stores one `uint64` per
materialized chunk, so Boolean operations process 64 documents per scalar
instruction before any row decoding. Up to four `(chunk, mask)` pairs stay
inline. Wider postings use a sparse persistent radix vector; deleting words
demotes back to inline storage and contracts redundant radix levels
immediately. Updates path-copy only the affected posting paths. When an update
does not change the indexed tuple, it publishes no new index nodes or catalog
slices.

The fingerprint directory is collision-resistant for caller-controlled string
content and avalanches low bits before radix routing. Fingerprints are never a
trust boundary: every public exact lookup re-resolves the indexed paths and
compares scalar values exactly. Numeric equality does not round through
`float64`; spellings such as `1`, `1.0`, and `1e0` agree, while distinct wide
numbers sharing a coarse candidate bucket are separated by the recheck.

Use prebuilt scalar indexes when the lookup repeats, or pass raw scalar JSON:

```go
keys := make([]string, 0, 32)
keys, err = view.AppendIndexRawKeys(
	keys[:0], "tenant_country_status",
	[]byte(`"acme"`), []byte(`"PT"`), []byte(`"active"`),
)
```

`AppendIndexRows` returns stable `(chunk, slot)` addresses for sparse gathers.
`AppendIndexMasks` keeps the native Boolean form. `AppendPointerRows` and
`AppendRowKeys` materialize only those candidates. All are zero-allocation
after caller buffers have sufficient capacity.

The query engine binds indexes at execution time, so a compiled query can
outlive create, backfill, and drop:

```go
q := query.Select(query.Path("id"), query.Path("profile.geo.country")).Where(
	query.And(
		query.Cmp("tenant", query.Eq, "acme"),
		query.Cmp("profile.geo.country", query.Eq, "PT"),
	),
)

var result query.Result
var workspace query.Workspace
err = q.RunSnapshotInto(&result, store.Snapshot(), &workspace)
```

The planner prefers the widest matching compound index, intersects indexed
`AND` branches, unions `OR` only when every branch is bounded, and complements
exact `NOT` masks against live slots. It uses sparse gathers at 50% selectivity
or below and always evaluates the original predicate over survivors. Building
indexes remain correct through dense scan fallback; readiness changes latency,
not answers.

`Snapshot.IndexStats` reports current physical `Fingerprints`, `ChunkWords`,
candidate bits, directory/bitmap nodes, and `EstimatedBytes` without allocating.
The estimate counts reachable index-owned objects but not allocator size-class
rounding. Cardinality and value distribution materially change physical size;
inspect the production definition rather than treating a fixture as a
guarantee. Update tests distinguish unchanged and changed tuples so an
unchanged value cannot publish new index nodes.

## Wildcard postings

`StoreIndexPostings` accelerates top-level existence and exact scalar
containment.

```go
info, err := store.AddIndex("search", simdjson.StoreIndexPostings)
if err != nil {
	return err
}
for info.State != simdjson.StoreIndexReady {
	info, err = store.BackfillIndex("search", 64)
	if err != nil {
		return err
	}
}
```

The definition is visible immediately as `Building`. Writes dual-maintain it
before publication. Covered chunks use postings; uncovered chunks use the exact
scan fallback, so readiness affects latency, not correctness. Already-covered
chunks still consume a backfill budget unit, keeping the call's work bounded.

Multiple logical posting names share one physical layer. `DropIndex` detaches a
logical name immediately. After the final consumer disappears, reclaim in
bounded batches:

```go
for done := false; !done; {
	_, done = store.ReclaimIndexes(64)
}
```

Build a containment needle once and reuse result capacity:

```go
n, err := simdjson.RequiredIndexEntries([]byte(`"compiler"`))
if err != nil {
	return err
}
needle, err := simdjson.BuildIndex(
	[]byte(`"compiler"`),
	make([]simdjson.IndexEntry, 0, n),
)
if err != nil {
	return err
}

keys := make([]string, 0, store.Len())
keys = store.AppendWhereContainsIndexKeys(keys[:0], "team", needle)
```

Posting hashes are candidate filters only. Exact verification removes hash
collisions and escaped-spelling aliases before returning a key.

## Operations

`Store.Stats` briefly takes the writer mutex and returns an O(1),
allocation-free operational snapshot:

- `Generation`: atomic publications, including one per expiry batch;
- `Keys`, `Chunks`, `ChunkDocuments`;
- `ChunkHighWater`: vector address span, not scan or compaction work;
- `ReusableChunks`: partial or empty ids available to writes;
- `ExpiringKeys`: exact heap-node count;
- `Indexes`, `IndexedChunks`, and `IndexReclaiming`.
- `MappedImageBytes`, `ExternalKeyBytes`, and `ExternalDocumentBytes`; mapped
  bytes remain RSS even when they do not contribute to Go `HeapAlloc`.

`Snapshot.AppendIndexes` returns reader-visible index name, kind, ordered paths,
state, covered chunks, and total chunks. `Snapshot.IndexStats` adds the physical
exact-index footprint. Alert on a stalled `Building` watermark, old retained
snapshots, or an event loop that leaves expired deadlines unpublished.

## Limits and measurement boundary

`ChunkDocuments` is in `[1,64]`. Chunk ids are `uint32`; mutation returns
`ErrStoreTooLarge` before wraparound. `FileStore` additionally fixes maximum
key/document bytes, page sizes, resident bytes, I/O and commit queue slots,
commit staging bytes and the optional coalescing window, snapshot leases,
retired extents, and at most 64 frozen exact indexes. Detailed source, tape,
depth, retention, and maintenance limits are in
[`contracts/limits.md`](contracts/limits.md).

Heap `Store` is in-memory; `FileStore` adds one-file crash recovery, explicit
eviction, and durability. Neither is a distributed database. There is no
replication, consensus, cluster protocol, server/client transport, access
control, cross-Store transaction, join, secondary range index, online
`FileStore` index DDL, or cross-process snapshot. A `FileStore` is single-node
and single-process-writer; concurrent goroutines may submit mutations, and
snapshots and compiled queries may be read concurrently. Its copy-on-write
protocol is the durability mechanism; it is not a user-visible WAL and does
not provide log shipping or point-in-time restore.

Correctness and resource gates use generator-owned counts, aggregates, result
digests, recovery checks, physical page counters, and explicit memory domains.
Durable file bytes, live Go heap, external arenas, admitted cache, commit
staging, retained generations, caller workspaces, and process RSS are never
collapsed into one number. Workload-specific capacity planning must record all
of them.
