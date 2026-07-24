# Store

This document describes the storage APIs implemented in the current tree. Public
Go documentation remains authoritative for individual methods and option
fields.

## Storage surfaces

| Surface | Purpose | Persistence |
| --- | --- | --- |
| `Store` | Mutable in-memory keyed collection with immutable snapshots | Explicit full checkpoint |
| `StoreBuilder` | Bulk construction of a `Store` | None |
| `FileStore` | General bounded-residency durable collection | Automatic incremental commits |
| `Store.WriteTo` / `OpenStore` | Portable immutable `Store` image | Explicit full checkpoint |
| `StorePageReader` / `StorePageDB` | Narrow page-file reader and mutation baseline | Automatic append-only commits |

A `Store` or `FileStore` is one physical JSON collection. `Database` is an
in-memory catalog of independent `Collection` handles; it is not a durable
multi-collection database.

## In-memory Store

The zero `Store` is usable. `NewStore` is preferred when options are known:

```go
store := slopjson.NewStore(slopjson.StoreOptions{
	ChunkDocuments: 16,
	ShapeTapes:      true,
})
```

`StoreOptions` is frozen by the first operation that initializes the store:

| Option | Current behavior |
| --- | --- |
| `ChunkDocuments` | Documents per immutable chunk; zero selects 64, valid values are 1–64 |
| `IndexOptions` | Structural-index configuration for each chunk |
| `ShapeTapes` | Deduplicates recurring object layouts within each chunk |
| `Postings` | Builds wildcard existence/scalar-containment postings from the first write |
| `ValueDict` | Enables the chunk-local scalar dictionary |
| `Schema` | Optional compiled schema; nil keeps the schemaless path |

### Mutation

`Put` validates one complete JSON value, copies a new key and the document, and
atomically inserts or replaces it. The caller may reuse both inputs after
return. A validation or schema failure publishes nothing.

Replacing a key:

- parses only the replacement document;
- preserves the key's existing TTL;
- shares unchanged immutable state with older snapshots;
- rebuilds at most the configured chunk and bounded metadata paths.

`Delete` removes the row from its chunk. It creates no tombstone, version chain,
or later compaction obligation. An empty chunk is removed immediately.

Writes are serialized. Each successful write publishes one immutable state
through an atomic pointer.

### Snapshots and reads

`Snapshot` is O(1) and does not wait for an in-progress writer. A snapshot stays
valid after any later update or delete.

`Snapshot.GetRaw` is lock-free, clock-free, and allocation-free. The returned
`RawValue` borrows snapshot storage. Use `AppendRaw` when the bytes must outlive
that storage or be placed in caller-owned capacity.

`CompileKey` returns a verified stable-slot hint for repeated reads. A later
movement or delete does not make it unsafe: lookup falls back to the complete
key path when the hint no longer matches.

`Range`, pointer extraction, field extraction, index masks, and bitmap helpers
operate over the same immutable snapshot. Concurrent readers are safe.

### TTL

TTL metadata is writer-side state. Reads do not check a clock or expiration
branch.

| Operation | Effect |
| --- | --- |
| `SetTTL` | Set a duration from the current clock; a non-positive duration deletes immediately |
| `SetDeadline` | Set an absolute deadline |
| `Persist` | Remove expiration |
| `Deadline` / `TTLAt` | Inspect expiration |
| `ExpireDue` | Delete up to the requested number of due keys |
| `RunExpiry` | Run a context-controlled expiry loop |

Replacing a value preserves its deadline. Due deletes are grouped by chunk
before publication.

### Schemas

`CompileStoreSchema` creates an immutable schema reusable by `Store`,
`StoreBuilder`, and `FileStore`.

Schemas can constrain:

- the root JSON type;
- required nested RFC 6901 paths;
- allowed types at each path, including unions.

Unspecified fields remain allowed. `SchemaInteger` distinguishes JSON integer
spellings from other numbers. Successful validation walks the structural index
already built for the write and allocates no additional per-row representation.

### Exact indexes

`CreateIndex` declares one exact scalar index:

```go
info, err := store.CreateIndex(slopjson.StoreIndexDefinition{
	Name:  "tenant_country",
	Paths: []string{"/tenant", "/profile/country"},
})
```

An index accepts one to four RFC 6901 paths. One path is a scalar index; multiple
paths form an order-sensitive compound key. Missing paths and container values
are omitted. Null, booleans, exact JSON numbers, and decoded strings are
indexed.

Creation on existing data publishes `StoreIndexBuilding`. Writes immediately
maintain covered state, while `BackfillIndex` advances old chunks in a
caller-bounded batch. Queries remain exact during construction by scanning
uncovered chunks. `StoreIndexReady` means every live chunk is covered.

Hashes and fingerprints only prune candidates. Exact JSON values are verified
before a row is returned.

`DropIndex` removes the logical index immediately. `ReclaimIndexes` bounds
physical wildcard-posting reclamation after the last user is gone.

### Bulk construction

`StoreBuilder` accepts unique keys, validates and copies documents directly into
final chunks, and builds declared indexes before publishing one `Store`.
`Append` is single-goroutine. `Build` transfers completed state and closes the
builder.

Use the builder for an initial corpus. Use `Store.Put` for subsequent
mutations.

## Store checkpoints

`Store.WriteTo` writes a complete immutable checkpoint. It is not incremental:
every live chunk is streamed on each call, and later writes do not modify the
image.

`OpenStore` validates the complete image before publication and returns a
normally mutable `Store`. Source and structural-tape bytes may borrow the input
image. Keep that image immutable and alive until the store, all snapshots, and
all derived borrowed values are unreachable. Mutations after open are heap-only
until another `WriteTo`.

The format is versioned and pre-v1. Cross-version compatibility is not promised
until the format is declared stable.

## FileStore

`FileStore` is the general durable path. It uses checksummed copy-on-write
pages, alternating superblocks, bounded queues, and a fixed-size page cache.
The caller owns the `*os.File` lifetime; keep it open until `FileStore.Close`
returns.

`CreateFileStore` requires an empty file and durably initializes its first root.
`OpenFileStore` performs bounded recovery from the superblocks, selected root,
and top-level directories. It does not scan the complete key or document set at
open.

### Configuration defaults

The zero `FileStoreOptions` selects:

| Resource | Default |
| --- | ---: |
| Metadata page | 4 KiB |
| Maximum page/extent | 64 KiB |
| Read cache | 64 MiB |
| Maximum document | 4 MiB |
| Maximum key | 256 bytes |
| Portable read workers | 4 |
| Prefetch queue | 64 references |
| Snapshot leases | 1,024 |
| Retired extents | 65,536 |

All resident, queue, snapshot, and retired-extent capacities are fixed at open.
`FileStoreStats` reports the selected capacities, current use, cache activity,
I/O backends, generations, and reclamation state.

`PageSize` and `MaxPageSize` must be powers of two. A document may exceed the
ordinary page size up to `MaxDocumentBytes`; overflow extents remain bounded by
the transaction limits derived from the options.

### Durability

`Put`, `Delete`, TTL changes, and expiry publish a copy-on-write generation.
Applications do not rewrite a checkpoint after each operation.

With `Synchronous: true`, mutation success means both the data barrier and the
alternate-root barrier completed. In asynchronous mode, a mutation becomes
reader-visible when the bounded committer accepts it. Use:

- `DurableGeneration` to observe the last fenced generation;
- `Flush` to wait until the current visible generation is durable;
- `Close` to stop new work, drain commits, and release owned resources.

`CommitCoalesce` bounds an optional group-commit window. It also affects the
latency of synchronous callers.

Recovery validates both superblocks and their roots and can fall back to the
previous complete generation. Corruption encountered when a lower page is
admitted is returned as an error. These guarantees still depend on the
filesystem and device honoring flush completion.

### Reads, snapshots, and reuse

`FileStore.Snapshot` acquires an explicit generation lease. Close it promptly.
While a snapshot is active, extents reachable from that generation cannot be
reused. A long-lived snapshot therefore increases `PendingRetiredExtents` and
`PendingRetiredBytes`; it does not block newer reads or commits until configured
retirement capacity is exhausted.

`FileSnapshot.AppendRaw` always copies exact JSON into caller storage and never
returns a borrowed cache page. Query execution and range scans use the same
lease.

### Larger-than-RAM operation

`ResidentBytes` bounds the page cache rather than the logical file size. Metadata
and documents enter the cache on demand; eviction uses a bounded CLOCK arena.
The file can therefore be larger than RAM without making the Go heap
proportional to row count.

This is a residency property, not an equal-latency claim. Cold reads still pay
storage latency, and one document may be larger than a query's working-memory
target.

On Linux, `ReadMode` and `WriteMode` can try or require `O_DIRECT` through
independently owned descriptors. `Backend` can select the portable engine or
the pure-Go `io_uring` engine. `FileStoreStats` reports the actual backend and
direct-I/O choices after fallback.

### Durable indexes, TTL, and numeric covers

`FileStoreOptions.Indexes` declares up to 64 exact scalar or compound indexes.
Definitions are fixed at creation and verified when reopened. Each write
maintains its postings transactionally.

`Float64Columns` declares up to 256 RFC 6901 paths. Numeric sidecars support
predicate-free `SUM`, `AVG`, `MIN`, and `MAX` without reopening JSON when the
query is fully covered. Missing, non-numeric, and non-finite values are absent
from the cover.

TTL is durable. `SetTTL`, `SetDeadline`, `Persist`, and `ExpireDue` commit the
same way as document mutations.

### Bulk creation

`Store.WriteFileStore` converts a completed in-memory store directly into one
durable generation. It preserves keys, TTL, schema, and declared exact indexes
while packing documents and configured numeric covers without replaying
individual `Put` calls.

## StorePageDB

`Store.WritePageFile` and `OpenStorePageReader` expose a simpler fixed-page
checkpoint. `StorePageDB` can insert, replace, and delete keys in that format
with append-only copy-on-write commits.

This surface is intentionally narrower than `FileStore`: it does not implement
general secondary indexes, TTL, overflow-value reuse, or the complete
reclamation and query facilities. New durable applications should normally use
`FileStore`.

## Allocation and ownership

The caller-buffered operations are the steady-state allocation boundary:

- `Snapshot.AppendRaw` and `FileSnapshot.AppendRaw`;
- compiled-key reads;
- bitmap and masked-row appenders;
- reusable query `Result`, `Workspace`, and file-execution workspace.

An undersized destination may grow. A new index/query high-water mark, custom
method, or oversized value may allocate. Zero-allocation claims apply only to
the documented warmed path, not every convenience call.

The in-memory store copies `Put` input. `OpenStore` may borrow its image.
`FileStore` copies writes and uses explicit snapshot leases for reads.

## Concurrency model

- `Store` and `FileStore` serialize mutations.
- In-memory `Snapshot` values are immutable and concurrent-safe.
- `FileSnapshot` is immutable but owns a closeable lease.
- Prepared queries are concurrent-safe with a separate result/workspace pair
  per execution.
- `StoreBuilder`, query workspaces, readers, writers, and mutable result buffers
  are single-consumer.

## Current product boundaries

The repository currently has no replication, backup manager, point-in-time
restore, network protocol, distributed execution, cross-file transaction, or
durable multi-collection catalog. Query joins are not implemented. Those
features are not implied by the storage APIs above.
