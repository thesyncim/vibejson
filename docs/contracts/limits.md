# Resource and input limits

This page records the limits that affect acceptance, memory growth, and
retention. A per-value or per-state limit is not a process-wide memory budget;
applications still need protocol-level limits for total input, work, and
concurrency.

## Configured input limits

### Nesting depth

Parsing, validation, selection, indexing, decoding, and transforms default to
10,000 nested arrays and objects. A positive `Options.MaxDepth`,
`DecoderOptions.MaxDepth`, or `document.IndexOptions.MaxDepth` replaces that
default for the APIs that accept the corresponding options. Values at or below
zero select the default. Convenience APIs without a depth option use the
default.

`MaxDepth` counts container nesting. It does not limit bytes, string or number
length, members in one container, or members in the whole document.

Encoding follows the supported compiler's `encoding/json` behavior. Go 1.26
has no fixed encoder nesting limit; it begins identity-based cycle detection
after a recursive path crosses 1,000 pointer-, map-, or slice-bearing values.
Go 1.27 and later reject encoding beyond 10,000 nested containers and also
bound a chain of pointer hops. There is no encoder option that changes these
toolchain-specific guards.

### Streamed value size

`ReaderOptions.MaxValueBytes` limits one top-level value when positive. Zero,
the default, means unbounded. The limit does not cap the number of values,
total stream bytes, elapsed work, or the Reader's initial buffer capacity. A
value over the limit stops iteration and is reported by `Reader.Err`.

`NewReader` and a zero `ReaderOptions.BufferSize` use a 64 KiB initial buffer.
A positive `BufferSize` chooses initial capacity only; values below 512 bytes
are rounded up to 512. Negative option values are rejected. The rolling buffer
grows when a complete value does not fit, so callers handling untrusted input
should set a positive `MaxValueBytes` and an application-level total-stream
policy.

One-shot byte-slice APIs do not impose a separate package-wide limit on input,
string, or number length. Available memory, Go's `int` and address space, and
the representation-specific index limits below still apply.

## Structural-index limits

`IndexEntry` is four `uint32` words (16 bytes). `BuildIndexOptions` rejects a
source length or caller-provided storage capacity greater than `math.MaxUint32`
with `document.ErrIndexTooLarge`; offsets and entry links must fit the same
32-bit representation. Insufficient caller storage returns
`document.ErrIndexFull`.

A container's direct member count occupies 26 bits, so one array or object can
hold at most 2^26 - 1 direct members in an `Index`. Exceeding that count returns
`document.ErrIndexTooLarge`. This is not a separate total-member limit across
the whole document. `RequiredIndexEntries` reports the exact caller storage
needed at the default depth limit.

The 64-level bitmap index machine is an internal fast-path limit, not an input
contract. Deeper accepted documents fall back to the general builder, which
enforces the caller's `MaxDepth`.

## Retained memory

### Reader, Writer, and returned values

- A `Reader` retains its rolling buffer, which can grow to accommodate the
  largest value encountered. `Reader.Close` releases the Reader's references to
  its input and buffer. Any slice already returned by `Bytes` remains retained
  by its caller.
- A `Writer` buffers one complete top-level value before flushing. Its size is
  a flush threshold, not a maximum value size. `Flush` and `Close` reuse the
  grown buffer; `Close` does not shrink it, release it, or close the underlying
  writer.
- Caller-owned output capacity, decoded destination storage, an owning
  `Value`'s source copy and index, and caller-provided `Index` storage live as
  long as their owners retain them. Pool budgets do not apply to these values.

### Pooled operation state

The following are retention budgets for one reusable state, not aggregate
process limits:

- Encoder map-entry storage and rendered map-key bytes each have an independent
  512 KiB retention budget. Each eligible typed-value backing slot also has a
  512 KiB budget. A compiled plan can own multiple fixed-type slots; oversized
  maps use one-call storage instead of replacing pooled scratch.
- A structural decoder state returned to the package pool retains at most
  2 MiB of `uint32` positions. Larger tapes are dropped on release.
- A compiled decoder caches at most four operation states. The cumulative
  shallow map key/value box budget is at most 64 KiB per state; ineligible or
  oversized boxes use operation-local storage.
- `Parse` seeds each pooled estimate buffer with 1,024 `IndexEntry` values
  (16 KiB). If a document needs a larger index, that grown index belongs to the
  returned `Value` rather than replacing the pooled estimate.

These states use `sync.Pool`. The runtime may discard pooled objects during
garbage collection, but the package does not place a deterministic aggregate
cap on the number of states retained across processors, compiled plans, or
concurrent calls.

### Compiled-plan caches and Marshal hints

The convenience `Marshal` and `Unmarshal` paths cache compiled plans by Go
type. Concrete types reached through interface values have additional encode
and decode plan caches; encode entries distinguish HTML-escaping mode. These
`sync.Map` caches have no entry-count limit or eviction policy and live for the
process lifetime. Programs that synthesize an unbounded number of `reflect`
types should avoid sending them through these paths. Explicitly compiled plans
live as long as their `Encoder` or `Decoder` owner.

`Marshal` retains an integer output-size estimate, not an output buffer.
Ordinary observations through 256 KiB update it immediately, with a 64-byte
minimum initial capacity. A first larger observation is stored as an
unconfirmed outlier and the next call starts with 512 bytes; a repeated equal
large observation confirms exact presizing, even above 256 KiB. A smaller
observation replaces the estimate. Therefore 256 KiB is an outlier-confirmation
threshold, not an output-size or retained-memory ceiling.

### Mutable Store

`StoreOptions.ChunkDocuments` must be from 1 through 64; zero selects 64. The
limit bounds documents rebuilt by an ordinary mutation, index-maintenance step,
or one affected chunk in an expiry batch. It is not a byte-size limit: a single
document may approach the structural-index limits above, and a chunk of large
documents can retain correspondingly large source and tape storage.

Chunk addresses are `uint32`. `Put` returns `ErrStoreTooLarge` before an append
could wrap the persistent vector's address space. Empty ids are reused, making
the theoretical limit unreachable under ordinary churn, but this is not a
process memory budget.

Each live Store key is present in one chunk slot and is reachable through the
heap key directory, the mapped immutable base directory, or a post-open heap
delta. Mapped-base entries can remain after delete, but the stable-slot live bit
and an exact spelling check make them unreachable; they are not reader-visible
tombstones or version chains. Each expiring key adds exactly one pointer-free
packed stable-slot/deadline heap node and one integer-keyed position-map entry;
changing a TTL does not add a stale generation or retain the key string. Each
physical posting chunk appears exactly once in writer reclamation metadata,
even when multiple logical posting index names share it.

A declared exact index has one to four RFC 6901 paths. Missing paths,
incompatible traversal steps, and JSON containers are not indexed. Each
distinct composite fingerprint owns an adaptive stable-slot posting. A
bulk-built or reopened immutable base packs multiple compressed streams per
physical page; later writes wholly shadow touched chunks in an inline/sparse
persistent-radix delta. Online indexes use that delta representation while
backfill is incomplete. Each materialized word addresses 64 document slots;
empty address ranges use no word. Fingerprint collisions can increase candidate
bits but exact rechecks preserve answers. `Snapshot.IndexStats` reports packed
base and delta footprint separately; no distribution-independent bytes/key
bound exists because value cardinality and frequency determine both.

Snapshots pin every immutable node, chunk, source arena, tape, declared-index
root, and physical wildcard-index version reachable from their publication
state. Holding old snapshots while updating hot keys intentionally retains old
versions. The package cannot bound that memory without invalidating the
snapshot contract; applications must bound snapshot age or count according to
their workload.

The current Store version may share unchanged source and structural-tape
backing with an older publication, but it retains no parent chunk or historical
version list. Replacing or deleting a row makes that row's old backing
collectible as soon as no retained snapshot reaches it. Compact narrow-value
slabs and dense row headers are publication-private.

A building online index temporarily retains the immutable chunk-vector root
captured by `AddIndex` or `CreateIndex` so its bounded cursor cannot miss an
original live chunk. Coverage is a sparse-paged bitmap: dense populated pages
cost one bit per chunk, while empty historical address ranges retain no pages.
Reaching `Ready` or dropping the definition releases the root; completion also
collapses the coverage bitmap to an implicit all-live state.

`BackfillIndex`, `ReclaimIndexes`, and `ExpireDue` accept caller work limits.
A non-positive limit means all currently eligible work, which may be large.
Production event loops should normally choose a positive batch size and expose
`Store.Stats`/`Snapshot.AppendIndexes` as progress metrics.

`Store.WriteTo` uses 32-bit counts and lengths for chunk/index/TTL metadata and
key/path spellings, 32-bit chunk ids, and 64-bit image offsets. It returns
`ErrStorePersistTooLarge` instead of truncating those fields. Only `Ready`
indexes can be written. `OpenStore` rejects unknown flags, nonzero reserved
fields, unaligned nested page images, inconsistent stable slots, duplicate
keys/indexes/TTLs, impossible counts, malformed nested `DocSet` images, and a
bad manifest checksum before publishing a Store. The pre-v1 format is versioned
but not yet promised stable across releases.

`OpenStore` borrows its complete input slice. A file mapping must remain
immutable and mapped until the Store, all snapshots, and every borrowed value
are unreachable. The package does not call `munmap`, retain an OS file handle,
or infer lifetime from finalizers. `AppendRaw` and `AppendRawKey` copy exact JSON
into caller storage when an independently owned result is required.

Mapped image bytes do not count toward Go `HeapAlloc`. On supported Unix
systems, the immutable base key directory and one 32-byte descriptor per row
also use pointer-free anonymous mappings outside `HeapAlloc`; they still count
toward RSS. `OpenStore` validates every key and row and still eagerly rebuilds
distinct shapes, optional accelerators, and declared exact-index roots as Go
objects. The image size is therefore not a resident-memory limit. Applications
must measure mapped image bytes, external metadata, and reconstructed heap
metadata separately; `OpenStore` images do not provide an eviction budget or a
100x-RAM guarantee. `StorePageReader` and `StorePageDB` do provide an explicit
fixed resident budget, and their 4,096-row smoke keeps a 1,155,072-byte file
correct over an 8,192-byte cache (141.0x). That ratio covers the page cache,
not process baseline, kernel cache, or equal latency. `StorePageDB` currently
supports durable insertion, replacement, deletion, stable-slot reuse, key-tree
splits, and chunk-radix growth. TTL and secondary-index roots, overflow values,
and free-extent reuse remain rejected or unavailable rather than silently
exceeding the contract. In this specialized checkpoint format, replacement
versions increase file bytes even though resident memory stays bounded. The
separate `FileStore` format below implements TTL, frozen exact indexes,
overflow extents, and snapshot-safe persistent extent reuse.

Dense Store bitmap workspaces use one `uint64` per logical chunk high-water id,
including empty historical ids. Prefer sparse `StoreMask` streams for selective
predicates or unusually sparse address spaces. Empty ids are reused under
ordinary churn. `StoreBitmapWords` panics before returning a wrapped length if
an otherwise valid address span cannot be represented by the platform's `int`
slice length.

### Attached FileStore

`FileStoreOptions` makes retained and in-flight resources explicit.
`ResidentBytes` bounds an arena of `PageSize` quanta. A page consumes exactly
`Length/PageSize` contiguous slots, so small metadata no longer pays
`MaxPageSize`; the minimum accepted budget is the exact worst-case dirty
transaction byte bound. The lookup table and 64-byte slot controls contain no
Go pointers. The buddy free-span directory is also pointer-free and bounded by
cache quanta plus maximum page size; allocation splits or coalesces one
power-of-two span without an arena scan. `BufferCount` and `QueueSlots` bound
commit data/descriptors, and `Stats.CommitCapacityBytes` exposes the complete
fixed staging arena even when it is mmap-backed and invisible to Go heap
accounting. The reusable-extent directory is another fixed pointer-free arena
sized by `MaxRetiredExtents`; `Stats.ReusableCapacityBytes` and
`ReusableExternalBytes` distinguish its capacity from Go heap.
`CommitCoalesce` is either zero or a bounded duration no greater than one
second; it changes durability grouping, not publication ordering.
`ReadConcurrency` bounds portable workers,
`ReadQueueDepth` bounds one native batch, and `PrefetchQueue` bounds waiting
references. `MaxSnapshotLeases` and `MaxRetiredExtents` bound
lifetime/reclamation metadata. Exhaustion returns an error or applies queue
backpressure rather than growing without limit. `Stats` reports current use and
pressure. Native SQ/CQ mappings, pointer-free in-flight descriptors, and
runtime stacks are bounded by these counts but remain process overhead outside
`ResidentBytes`.

`PageSize` is a power of two at least 4 KiB. `MaxPageSize` is a power-of-two
multiple of it. Keys and JSON are rejected above `MaxKeyBytes` and
`MaxDocumentBytes`; values above `InlineValueBytes` use bounded overflow pages.
Chunk ids remain `uint32`, physical offsets and file high-water are bounded by
signed OS file offsets, and persistent logical ids are `uint64`. At most 64
one-to-four-column exact indexes and 256 RFC 6901 `Float64Columns` may be
frozen into a file. Cover order and spelling are part of the durable catalog
hash. Reopening with a different effective catalog or format options fails
instead of interpreting old bytes under new semantics. Each cover adds one
eight-byte stable-slot mask plus eight bytes per finite numeric cell to its
document page. `MaxPageSize` validation includes the worst-case configured
cover section. Writer scratch is fixed at `columns * (8 + 64*8)` bytes and
reported as `Float64ScratchBytes`.

Opening reads bounded root/page scratch and does not scale heap with corpus
cardinality. This is not a promise that every operation fits one page: one
maximum-sized document, a copy-out destination, transaction scratch, and the
caller's final query result exist outside `ResidentBytes`. The file query
executor separately bounds raw batches and merge state with `MemoryBytes`;
one oversized row may exceed the target, and an unbounded projection or group
result necessarily consumes output-proportional memory. Spill merge opens at
most 32 runs at once and removes its temporary files on return.

Durable exact-index planning can reduce admitted JSON rows but does not weaken
validation. A posting certificate may decide an entire bitmap only when its
validated scalar or compound tuple has no collision flag; otherwise indexed
paths are rechecked against stored documents. Version-one postings and
oversized representatives always take the recheck path.
`FileExecutionStats.RowsTotal`, `RowsScanned`, `IndexPostingPages`,
`IndexCertificateRows`, and `IndexRecheckRows` separate logical cardinality
from physical posting and JSON work.
`FileIndexWorkspace` and `FileExecutionWorkspace` are caller-retained
high-water storage and are single-consumer. Call `Release` after an exceptional
broad probe if retaining that capacity is undesirable.

An unfiltered scalar aggregate over configured numeric covers can report
`RowsScanned == 0` and a non-zero `CoveringColumns`. The engine preflights all
paths, then fuses their reductions over one page walk. `COUNT(path)`, filtered
aggregates, grouping, and partially covered plans do not use this lane.
`ReduceFloat64PathsInto` requires equal-length caller buffers and supports no
more than 256 paths; warmed calls allocate zero. Covers are co-located with
document bytes, so a cold reduction still reads the containing document pages
and remains bounded by the ordinary cache and I/O queues.

`ReadMode` controls cache misses independently from the commit backend.
`FileStoreReadBuffered` uses the caller descriptor. On Linux,
`FileStoreReadDirectTry` and `FileStoreReadDirectRequire` reopen the same inode
on an independently owned `O_DIRECT` descriptor; neither changes nor closes the
caller descriptor. Try may fall back and reports the result in
`Stats.DirectReads`; Require returns `ErrStoreDirectIOUnsupported`. Direct reads
retain the same alignment, checksum, identity, and bounded-cache validation.
`WriteMode` independently selects buffered, try-direct, or require-direct
commits. A direct writer uses a second owned `O_DIRECT` descriptor with either
the portable or pure-Go `io_uring` device and reports activation through
`Stats.DirectWrites`. Every data page and the complete root slot write are
page-aligned; durability ordering and caller descriptor ownership are
unchanged. This prevents sustained writes from populating the kernel page
cache, but can increase latency for small commit groups.
`RangeRawReadAheadBuffer` uses this lane to overlap direct document misses. Its
window is the minimum of one half of `ResidentBytes`, `PrefetchQueue`, 64
extents, and either `ReadQueueDepth` for the native issuer or four requests per
portable worker. Buffered files retain the serial scan and kernel readahead.
The native issuer submits `IORING_OP_READ` directly into reserved mmap cache
spans; it uses no staging copy or registered data buffer. Demand and dirty
admission wait through transient all-loading pressure, but leased/dirty
exhaustion still fails closed. `Stats.ReadBackend`, `AsyncReadBatches`,
`LargestReadBatch`, `CacheMisses`, `CoalescedReads`, `ReadErrors`,
`PrefetchHits`, and the queue counters make the choice and pressure observable.

The explicit `SIMDJSON_FILESTORE_100X=1` gate covers 21,347,320 source key+JSON
bytes with a 200,704-byte cache (106.4x), including cold reopen, eviction,
an ordered full read-ahead scan, update, delete, and mutable TTL. The measured
Linux run used direct reads and writes, a 256 MiB container, and
`GOMEMLIMIT=128MiB`; it sampled 17.0 MiB current RSS, 18.1 MiB peak RSS, and
3.50 MiB Go heap. The page ratio still does not include caller output or imply
that cold reads match resident hits.

`scripts/run-filestore-physical-scale.sh` is the distinct physical-memory gate.
It compiles before entering a 64 MiB cgroup, requires Linux direct reads and
writes, checks allocated filesystem blocks rather than logical sparse size,
and defaults to a 100x ratio. The measured ARM64 Docker run stored
6,713,852,053 live source bytes and 6,920,364,032 allocated file bytes while
the complete cgroup peak was 52,576,256 bytes: 127.7x and 131.6x respectively.
It also reopened, probed a nested exact index, updated, deleted,
and changed TTL under eviction. This proves that corpus residency is bounded;
one maximum document, caller copy-out, fixed staging buffers, runtime state,
and final query output still count toward the cgroup, and cold misses still pay
device latency.

The caller owns the `*os.File` and spill directory. `FileStore.Close` does not
close the file and fails while `FileSnapshot` leases remain. `RangeRaw`,
`RangeRawBuffer`, and masked-range callback bytes expire when the callback
returns; `AppendRaw` and file-query result cells own independent copies.
Sparse masks must be strictly increasing by chunk, and a non-zero mask for a
chunk absent from the selected snapshot is rejected. The attached format is
pre-v1 and version checked, but cross-release compatibility is not yet promised.

## Limits that applications must add

The package does not provide a process-wide memory budget, total-stream byte or
value count, deadline, concurrency cap, output-size cap, or type-cache
cardinality cap. Apply those limits at the protocol or service boundary. In
particular, `BufferSize`, a Writer size, `MaxDepth`, per-state scratch budgets,
and the 256 KiB Marshal threshold must not be treated as substitutes for a
total resource budget.
