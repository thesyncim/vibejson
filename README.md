# slopjson

[![ci](https://github.com/thesyncim/slopjson/actions/workflows/ci.yml/badge.svg)](https://github.com/thesyncim/slopjson/actions/workflows/ci.yml)

Strict JSON processing for Go, written entirely in Go. It provides
`encoding/json`-style `Marshal` and `Unmarshal` for supported values, reusable
typed plans, a structural document index with multi-document batch primitives,
and optional Go-native SIMD.
The root module has no third-party module dependencies, assembly, C,
`go:linkname`, or private runtime-layout assumptions.

This project is pre-v1: APIs may change while the accepted
[v1 boundary](docs/adr/0001-v1-api.md) is implemented. It is an independent Go
implementation, not the C++ [`simdjson`](https://github.com/simdjson/simdjson)
project. Algorithm and corpus relationships are recorded in
[the provenance inventory](docs/provenance.md).

## Install

Go 1.26 builds the supported portable backend:

```sh
go get github.com/thesyncim/slopjson@latest
```

Consumers of the former module path should follow the
[migration guide](MIGRATION.md); this repository does not publish a
compatibility module under the former identity.

The optional SIMD backend requires the exact Go 1.27 development compiler
pinned by the repository:

```sh
./scripts/bootstrap-gotip.sh "$HOME/sdk/slopjson-gotip"
GOEXPERIMENT=simd "$HOME/sdk/slopjson-gotip/bin/go" test ./...
```

`GOEXPERIMENT=simd` enables compiler support; CPU selection and supported
compiler windows are documented in the [toolchain policy](docs/toolchain.md).

## Quick start: typed decode

```go
package main

import "github.com/thesyncim/slopjson"

type Event struct {
	ID   int      `json:"id"`
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

func decode(data []byte) (Event, error) {
	var event Event
	err := slopjson.Unmarshal(data, &event)
	return event, err
}
```

For encoding, `Marshal` is the allocating convenience for occasional calls;
hot paths compile once and append into caller-owned capacity:

```go
var eventEncoder slopjson.Encoder[Event]

func init() {
	var err error
	eventEncoder, err = slopjson.CompileEncoder[Event](slopjson.EncoderOptions{})
	if err != nil {
		panic(err)
	}
}

func encode(event *Event) ([]byte, error) {
	buf := make([]byte, 0, 4096)
	return eventEncoder.AppendJSON(buf, event)
}
```

## The document index

`BuildIndex` validates a document once and lays out a structural index in
caller-owned storage — a flat tape of 16-byte entries, sized exactly by
`RequiredIndexEntries`. A `Node` then navigates the document without
allocating or rescanning text, with `encoding/json`-compatible last-duplicate
and escaped-key semantics:

```go
n, err := slopjson.RequiredIndexEntries(data)
if err != nil {
	return err
}
index, err := slopjson.BuildIndex(data, make([]slopjson.IndexEntry, 0, n))
if err != nil {
	return err
}
user, ok := index.Root().Get("user")
name, ok := user.Get("screen_name")
text, ok := name.StringBytes() // zero-copy view into data
```

The layer around that core, one line each:

- **Key-hash enrichment** — `slopjson.BuildIndexOptions(data, storage,
  document.IndexOptions{HashKeys: true})` stores a hash per object key, so
  lookups reject non-matching members on one word compare and flat objects
  take an unrolled scan over the index itself.
- **Compiled queries** — `key := slopjson.CompileKey("user_id")` and
  `ptr := slopjson.MustCompilePointer("/statuses/0/user/name")` hash and
  parse once; `node.GetCompiled(key)` and `index.PointerCompiled(ptr)` reuse
  them across calls and documents.
- **Constant-time member lookup** — `probe, ok :=
  slopjson.BuildObjectProbe(node, slots)` builds an open-addressed table over
  one wide object; `probe.Get(key)` answers hits and misses in constant
  expected time.
- **Document batches** — `var set slopjson.DocSet; set.ReadFrom(r)` ingests a
  stream of NDJSON or concatenated documents straight into shared arenas;
  `set.Doc(i)` returns each document's ordinary `Index`, valid across later
  appends.
- **Columnar extraction** — `vals = cache.AppendField(vals, &set, "user_id")`
  on a `slopjson.ShapeCache` extracts one field across every document through
  cached object layouts; `cache.AppendFieldInt64(dst, valid, &set, "user_id")`
  produces a dense typed column with a validity mask in the same pass.
- **Buffered posting queries** — with `DocSet.Postings` enabled,
  `rows = set.AppendWhereExists(rows[:0], "status")` and
  `rows = set.AppendWhereContainsIndex(rows[:0], "status", needle)` reuse the
  caller's result storage. Build the needle index once; warmed lookups allocate
  nothing, including long escaped strings and exact verification over compact
  shape tapes.
- **Reusable query execution** — `query.Compile` or the builder API produces an
  immutable query. Keep one `query.Result` and `query.Workspace` per worker and
  call `q.RunInto(&result, &set, &workspace)`; after the retained row, posting,
  decoded-text, and group capacities warm, projection, containment, stable
  ordering, aggregates, and grouping execute with zero heap allocations.

Ownership is uniform: an `Index` and its nodes borrow the source and the entry
storage; `DocSet` and the caches own their arenas, and nothing they hand out is
invalidated by growth.

## Mutable documents, TTL, and online indexes

`Store` adds keyed updates and deletes while publishing immutable snapshots.
`Snapshot.GetRaw` takes no lock, makes no clock call, inspects no tombstone, and
allocates nothing:

```go
store := slopjson.NewStore(slopjson.StoreOptions{
	ChunkDocuments: 8, // write-heavy; zero selects the read/space default of 64
	ShapeTapes:      true,
})

if _, err := store.Put("session:42", []byte(`{"user":42,"state":"open"}`)); err != nil {
	return err
}
before := store.Snapshot()
sessionKey := before.CompileKey("session:42")

store.SetTTL("session:42", 30*time.Minute)
store.Put("session:42", []byte(`{"user":42,"state":"active"}`)) // preserves TTL
store.Delete("session:42")

// The old immutable view remains valid after both mutations.
raw, ok := before.GetRaw("session:42")

// Repeated reads can bypass hashing and the key directory through a verified
// stable slot; delete/reinsert movement falls back to the complete lookup.
raw, ok = before.GetRawKey(sessionKey)
```

A `Store` is also the physical boundary for one JSON collection. Named
`Collection` handles group independent keyspaces in an in-memory `Database`
catalog without adding a collection id to rows or a catalog lookup to held
handle operations. Optional schemas compile nested RFC 6901 constraints once:

```go
schema, err := slopjson.CompileStoreSchema(slopjson.StoreSchemaDefinition{
	Root: slopjson.SchemaObject,
	Fields: []slopjson.StoreSchemaField{
		{Path: "/id", Types: slopjson.SchemaInteger, Required: true},
		{Path: "/profile/name", Types: slopjson.SchemaString},
	},
})
if err != nil {
	return err
}
var database slopjson.Database
users, err := database.CreateCollection("users", slopjson.StoreOptions{
	ShapeTapes: true,
	Schema:     schema,
})
```

Validation is fused into the existing structural parse before publication and
shape compaction. Nil schema keeps the specialized schemaless state layout and
write path; successful four-field `ValidateIndex` measured 65-67 ns with zero
allocations locally. Schemas allow unspecified fields, persist in Store images,
and bind FileStore/page-file recovery to the same compiled identity. Each
durable file is currently one collection; the multi-collection catalog itself
is not yet durable. Full semantics, measured overhead, and migration limits are
in [Mutable Store operations](docs/store.md#collections-and-optional-schemas)
and [ADR 0006](docs/adr/0006-collections-and-compiled-schema.md).

For an initial keyed corpus, `StoreBuilder` validates and copies rows directly
into their final micro-pages. Duplicate detection uses a geometric,
pointer-free table that packs a hash fingerprint and row ordinal into one word;
the exact key bytes are still compared on every candidate. `Build` discards
that transient table, compacts the published key directory, and publishes only
the completed Store:

```go
builder, err := slopjson.NewStoreBuilder(slopjson.StoreOptions{ShapeTapes: true})
if err != nil {
	return err
}
if err = builder.CreateIndex(slopjson.StoreIndexDefinition{
	Name: "state", Paths: []string{"/state"},
}); err != nil {
	return err
}
if err = builder.Append("session:42", []byte(`{"state":"open"}`)); err != nil {
	return err
}
store, err := builder.Build()
```

The builder is single-goroutine and accepts each key once. It owns both key and
JSON bytes after `Append`; declared nested or compound indexes are returned
`Ready`, and the Store immediately supports ordinary mutation, snapshots, TTL,
and online index changes.

`Store.WriteTo` checkpoints one generation into a Store-native container of the
same bounded `DocSet` page images. `OpenStore` can borrow a caller-owned
read-only mmap and returns a normally mutable Store:

```go
var image bytes.Buffer
if _, err := store.WriteTo(&image); err != nil {
	return err
}
reopened, err := slopjson.OpenStore(image.Bytes())
if err != nil {
	return err
}
dst, ok := reopened.AppendRaw(make([]byte, 0, 256), "session:42")
```

The image bytes must remain immutable and live until the Store, retained
snapshots, and borrowed values are dead. `AppendRaw`/`AppendRawKey` make an
owned copy into caller capacity and allocate nothing after capacity is warm.
`WriteTo` remains a full checkpoint: later heap-Store mutations do not update
that image. The checkpoint also carries an optional compiled schema definition;
`OpenStore` restores it and fails closed before publication if any row violates
the contract.

For an immutable checkpoint that must be read through an explicit fixed-size
page cache, `Store.WritePageFile` and `OpenStorePageReader` provide a narrower
page-file surface. `StorePageDB` can durably replace or delete existing keys in
that format and can insert a missing key by reusing the first free stable slot
or splitting its copy-on-write key and chunk paths. It is kept as a focused
page-I/O and crash-consistency baseline; it does not support TTL, secondary
indexes, overflow values, or extent reuse. Schema-bound page files require the
same `StorePageOpenOptions.Schema` on reader/database open, and
`StorePageDB.Put` enforces it before page construction. Use `FileStore` for the
general durable collection below.

For incremental durability and a bounded resident set, attach a `FileStore` to
a caller-owned file. Its key, chunk, exact-index, TTL, free-space, document, and
overflow structures are checksummed copy-on-write pages selected by alternating
superblocks:

```go
file, err := os.OpenFile("events.sj", os.O_RDWR|os.O_CREATE, 0o600)
if err != nil {
	return err
}
store, err := slopjson.CreateFileStore(file, slopjson.FileStoreOptions{
	ResidentBytes: 256 << 20,
	ReadQueueDepth: 64,
	ReadMode:      slopjson.FileStoreReadDirectTry,
	WriteMode:     slopjson.FileStoreWriteDirectTry,
	Synchronous:   true,
	Indexes: []slopjson.StoreIndexDefinition{
		{Name: "tenant", Paths: []string{"/tenant"}},
	},
	// Frozen numeric covers make predicate-free aggregates parser-free.
	Float64Columns: []string{"/score"},
})
if err != nil {
	return err
}
defer store.Close() // the caller still closes file

_, err = store.Put("event:42", []byte(`{"tenant":"acme","score":7}`))
snapshot, err := store.Snapshot()
if err != nil {
	return err
}
defer snapshot.Close()
raw, ok, err := snapshot.AppendRaw(nil, "event:42")
```

For bulk creation, finish an in-memory `Store` and call
`Store.WriteFileStore(file, options)` instead of replaying `Put`. It repacks
live rows into the requested stable-slot geometry, builds nested and compound
exact indexes plus TTL directories bottom-up, and publishes one mutable
generation with two durability fences. When it saves physical bytes, up to 128
rows from consecutive logical chunks share one immutable document-group
extent. Static JSON structure is stored once per shape; repeated scalar
spellings use a bounded page dictionary, short literals carry their length in
the token, and keys plus numeric covers remain directly addressable. Numeric
covers are detached from grouped JSON, shared across bounded micro-regions, and
adapt each column to exact unsigned 8-, 16-, or 32-bit lanes before falling
back to IEEE float64. Point and scan output still reconstructs the exact
original JSON spelling into caller capacity. Packed posting and document-group
pages are immutable bases. The first update peels only the affected logical
chunk into an ordinary copy-on-write page; the shared document or typed extent
is retired only after its final mapping is peeled.

An untouched compact generation also carries physically contiguous,
value-only scan stripes under a 64-way ordered copy-on-write directory.
Predicate-free covered aggregates bypass the chunk tree, JSON, masks, and
document I/O. Directory nodes stay at the 4 KiB metadata quantum while stripes
pack up to `MaxPageSize`. A replacement that leaves every configured numeric
projection unchanged reuses the scan byte-for-byte. A changed value or delete
in any covered stripe rebuilds only that stripe and its bounded root-to-leaf
directory path; every other stripe and subtree remains shared. An insert uses
the same path when its chunk is already covered and the rebuilt stripe still
fits. An out-of-range insert, emptied stripe, or oversized rebuild atomically
retires the shortcut and uses the authoritative sidecar/document path.
Recompacting restores it. Both paths are checksummed, pointer-free on disk,
exact-bit deterministic across portable and Go-native SIMD reducers, and
zero-allocation with warmed caller-owned buffers.

Compact generations also derive aggregate-only categorical covers from
eligible single-column exact indexes. Low-cardinality one-page covers are
maintained transactionally across ordinary scalar inserts, updates, and
deletes; semantic no-ops reuse the page. High-cardinality covers stream across
bounded linked pages rather than forcing one giant extent. Their first
document mutation currently retires the immutable chain and falls back to
certified postings. Neither form adds per-row pointers or duplicates posting
state.

`FileStore` opens from bounded root/page scratch instead of walking the corpus.
Its CLOCK arena is divided into 4 KiB allocation quanta: a metadata page uses
one slot and a larger document uses only its exact contiguous span, rather than
every page paying the maximum extent size. A pointer-free buddy allocator
splits and coalesces those power-of-two spans without rescanning the arena on
each miss; pressure performs at most one CLOCK pass instead of repeated
whole-cache scans. The resident lookup directory and one-cache-line frame
records are pointer-free, while independent hot pages use independent locks.
Portable read workers, the native read submission depth, prefetch and commit
queues, active snapshots, and retired extents all have explicit capacities
reported by `Stats`.
Direct read and write modes reopen independent Linux `O_DIRECT` descriptors
when supported and report the actual choices through `Stats.DirectReads` and
`Stats.DirectWrites`; neither changes or closes the caller's descriptor.
With the native backend, one single-owner pure-Go `io_uring` issuer batches
speculative reads directly into the off-heap cache arena: there is no staging
copy, registered-buffer pin, per-request goroutine, or steady-state
allocation. `Stats.ReadBackend`, `AsyncReadBatches`, and `LargestReadBatch`
report the actual path. Demand misses remain authoritative positional reads,
and Auto setup or a later ring-accounting failure falls back safely; a required
native setup fails construction. Direct writes likewise keep sustained
ingestion from filling the kernel page cache and work with either commit
backend.
Async writes become reader-visible when queued; `DurableGeneration` and
`Flush` expose the durability boundary. `CommitCoalesce` can bound a
background group-commit window without delaying async publication.
`Synchronous` waits on each mutation, but waits outside serialized page
construction so concurrent durable writers can share a fence. Close blocks
new construction and drains those waiters before releasing I/O resources.
Within a grouped commit, only the newest state-root page can be selected by the
newest superblock; earlier state-root writes are therefore suppressed while
all data pages remain durable for live intermediate snapshots.
`Stats.SuppressedRootWrites` and `SuppressedRootBytes` expose that saved write
amplification.
`Stats.CommitCapacityBytes` reports the complete fixed staging arena, including
mmap-backed bytes outside Go heap accounting.
The reusable-extent directory is likewise a fixed pointer-free arena and lives
outside the Go heap on supported Unix systems; `ReusableCapacityBytes` and
`ReusableExternalBytes` keep that capacity visible.
Snapshot leases fence physical reuse, while the previous durable generation
remains recoverable if the newest data or root write is torn.
Synchronous success means both data and alternate-root barriers completed;
asynchronous callers must use `DurableGeneration`, `Flush`, or `Close` for the
same acknowledgement. Recovery validates bounded roots and falls back one
whole generation, while cold lower-tree corruption fails closed on admission.
This contract still assumes the filesystem and device honor flush completion;
replication, backups, and point-in-time restore are outside this single-file
engine.

The explicit `SLOPJSON_FILESTORE_100X=1` Linux gate stores 21,347,320 source
key+JSON bytes behind a 200,704-byte page cache (106.4x), reopens twice, and
checks a complete ordered read-ahead scan, distant reads, update, delete, and
mutable TTL with direct reads and writes active. The scan window is bounded by
cache bytes, queue depth, and read concurrency. A 256 MiB Linux container with
a 128 MiB Go limit sampled 17.0 MiB RSS, 18.1 MiB peak RSS, and 3.50 MiB Go
heap while scanning at 78.0 MiB/s. That is a bounded-residency correctness
result, not a claim that cold storage has resident-memory latency.

The separate physical gate compiles outside a 64 MiB Linux cgroup, requires
direct reads and writes, and stores one nested exact index with 2,137 large
documents. Its live key+JSON source was 6,713,852,053 bytes (127.7x the
52,576,256-byte cgroup peak); allocated filesystem blocks were 6,920,364,032
bytes (131.6x peak), and the file high-water was 6,923,669,504 bytes. It
reopened, probed distant keys and the nested index, then updated, deleted, and
changed TTL under eviction in 14.79 seconds on the measured Docker/Linux ARM64
volume. Run `scripts/run-filestore-physical-scale.sh`; it needs roughly 10 GiB
free for the default gate. The large-value fixture makes a physical 100x run
practical and proves the memory boundary, not equal cold-read latency or a
universal throughput result.

`query.Query.RunFileSnapshot` late-binds the frozen exact-index catalog.
Equality and supported containment predicates read candidate chunks and
ordinary row-producing plans recheck complete predicates; fully certified
`COUNT(*)` plans popcount exact masks without opening JSON. Unbounded plans
scan chunk leaves in order.
An unfiltered scalar `COUNT(*)` plus `SUM`/`AVG`/`MIN`/`MAX` plan uses frozen
numeric covers when every aggregate path is configured. Multiple paths fuse
into one typed-extent walk; missing, null, non-numeric, and non-finite cells are
skipped with the ordinary numeric semantics. `RowsScanned` remains zero and
`CoveringColumns` reports the physical lane. `COUNT(path)`, predicates, and
partially covered plans stay on the JSON executor because a numeric cover
cannot represent present non-numeric values.
Execution builds bounded batches in parallel and externally merges ordered
rows or grouped state through temporary spill files with a 32-run fan-in.
`MemoryBytes` bounds working state, not the caller-owned final result or one
document larger than the target. The Store is deliberately single-file and
single-process-writer: there is no replication, distributed execution,
cross-Store transaction, join engine, or server protocol. The `query` package's
SQL subset is only a compile-time adapter to the same typed plan used by the Go
builder; execution does not retain or interpret SQL strings. Exact-index
definitions are frozen when the file is created. The complete contract and
measured limits are in [Mutable Store operations](docs/store.md).

[ADR 0007](docs/adr/0007-compact-document-query-wire.md) locks the next query
architecture without pretending it is already implemented: a typed operation
algebra compiles once to a binary prepared plan, while readable JSON-shaped
queries and SQL remain changeable cold adapters. Point commands bypass the
relation engine, and ordered nested/compound/multikey indexes are the explicit
prerequisite for range, order-plus-limit, and indexed joins. The
[Store completion ledger](docs/store-roadmap.md) separates implemented,
accepted, and missing work and records the post-merge sequence.

An update parses only its replacement. Unchanged source and structural-tape
storage stays immutable and is shared into the next bounded chunk; deletes copy
only dense row metadata and remove the last-row chunk directly. There are no
version chains, tombstones, or later compaction threshold.

TTL lives in a writer-side indexed four-ary heap—one mutable node per expiring
key, no stale deadline generations—and due keys are grouped by chunk and
published in one delete batch. `RunExpiry` sleeps until the next deadline;
ordinary reads pay literally no TTL branch or time lookup.

Declared exact indexes accept one to four RFC 6901 paths, including nested
object fields and array positions. Compound equality, `AND`, `OR`, and exact
`NOT` plans combine stable-slot chunk masks before sparse column extraction:

```go
info, err := store.CreateIndex(slopjson.StoreIndexDefinition{
	Name:  "tenant_country",
	Paths: []string{"/tenant", "/profile/geo/country"},
})
for err == nil && info.State != slopjson.StoreIndexReady {
	info, err = store.BackfillIndex(info.Name, 64)
}
if err != nil {
	return err
}

q := query.Select(query.Path("id")).Where(query.And(
	query.Cmp("tenant", query.Eq, "acme"),
	query.Cmp("profile.geo.country", query.Eq, "PT"),
))
var result query.Result
var workspace query.Workspace
err = q.RunSnapshotInto(&result, store.Snapshot(), &workspace)
```

For a custom planner, `AppendIndexBitmap` and `AppendLiveBitmap` fill reusable
dense page-word buffers. `AppendStoreBitmapAnd`, `AppendStoreBitmapAnd3`,
`AppendStoreBitmapOr`, and `AppendStoreBitmapAndNot` combine them in place with
zero allocation. Pinned Go 1.27 SIMD builds use runtime-gated 256-bit AVX2 on
GOAMD64 v1/v2 and direct AVX2 on v3+, while sparse `(page, mask)` lists keep
their scalar merge path.

Online indexes publish as `Building`, dual-maintain concurrent writes, backfill
in caller-bounded chunk batches, and become `Ready` at complete coverage.
Snapshot probes remain exact during build through scan fallback. Dropping
detaches the logical index immediately. Wildcard-posting reclamation is also
caller-bounded and never performs a hidden full-store completion scan; declared
roots are reclaimed automatically with their last snapshot.

Durable `FileStore` queries bind their frozen nested or compound definitions at
execution time. Collision-free posting certificates can prove scalar and
compound exact masks without opening JSON; a hash collision, legacy posting,
or oversized representative falls back to exact document recheck. Nested
object `@>` needles made entirely of scalar leaves lower to exact path
conjunctions, while arrays and empty objects keep structural evaluation. Index
corruption is returned rather than hidden by a fallback.
`FileExecutionStats` reports total versus scanned, certificate-decided, and
document-rechecked rows, probe count, physical posting-page groups, and
candidate chunks.
`FileIndexWorkspace`, caller-buffered masked ranges, and
`query.FileExecutionWorkspace` retain hot probe and overflow scratch explicitly.

The complete API, ownership rules, expiration semantics, tuning table,
complexity bounds, zero-allocation recipes, operational counters, limits, and
Store evidence boundaries are in
[Mutable Store operations](docs/store.md).

## Performance

Single core, Apple M4 Max, pinned Go development toolchain with
`GOEXPERIMENT=simd`:

| Operation | Measured |
| --- | --- |
| Validate | 4.38 GB/s |
| Build index (validation included) | 3.18 GB/s |
| Ingest a document stream (`DocSet.ReadFrom`) | 7.44 GiB/s |
| Lookup on an indexed document, marginal per path | 107 ns |
| Lookup primitives (probe hit, hashed `Get`) | 3.8–6.4 ns |
| Extract one field across a document set | 8.1 ns/doc |
| Extract a typed `int64` column | 12 ns/doc |

Performance changes must preserve correctness, ownership, retained memory,
`B/op`, and `allocs/op`. Native CI exercises matched portable and SIMD behavior
on amd64 and ARM64.

The [benchmark contract and reproduction commands](benchmarks/README.md) define
the standalone product workloads, regression gates, and pinned toolchains.

## Choose an API

| Need | Start with |
| --- | --- |
| Ordinary typed JSON or strict validation | `Marshal`, `Unmarshal`, `Valid`, `Validate` |
| Repeated typed work and buffer reuse | `CompileEncoder`, `CompileDecoder` |
| Framed JSON input or token output | `Reader`, `Writer` |
| Compact, indented, or canonical output | `Compact`, `Indent`, `Canonicalize` |
| Borrowed selection or repeated document navigation | `RawValue`, `Index`/`Node`, or `Parse`/`Value` |
| Keyed datasets, including bulk construction | `StoreBuilder`, `Store`, `Snapshot` |
| Fixed-cache reads or replacement/deletion of an immutable page checkpoint | `Store.WritePageFile`, `OpenStorePageReader`, `StorePageDB` |
| Incrementally durable, bounded-residency keyed datasets | `CreateFileStore`, `OpenFileStore`, `FileStore`, `FileSnapshot` |
| Low-level immutable arenas and column extraction | `DocSet`, `ShapeCache`, `KeyInterner` |
| SQL-shaped projection, filtering, grouping, and aggregation | `query.Query.RunInto`, `query.Result`, `query.Workspace` |
| Persistent-indexed bounded queries over a durable file snapshot | `query.Query.RunFileSnapshot`, `query.FileExecutionOptions`, `query.FileExecutionWorkspace` |
| Keyed updates, deletes, TTL, snapshots, exact indexes, and wildcard postings | `Store`, `Snapshot`, `StoreIndexDefinition`, `StoreStats` |

The advanced document APIs are moving into `document` during the pre-v1
migration. JSON kind values already use `document.Kind`; the remaining package
boundary and compatibility decisions are documented in the
[API ADR](docs/adr/0001-v1-api.md).

## Streaming input limits

`NewReader` and a zero `ReaderOptions.MaxValueBytes` leave each top-level value
unbounded. Set a positive protocol limit for untrusted input; the exact stream,
depth, index, and retention limits are in the
[resource contract](docs/contracts/limits.md).

## Ownership and concurrency

Default typed decoding and default `Parse` own the string storage they expose.
`ZeroCopy`, `RawValue`, `Index`, Index-derived `Node`, and reader cursors borrow
storage: keep the source alive and unmodified, and observe each API's
invalidation rule. `Index` and its nodes also borrow caller-provided entry
storage; a node obtained from an owning `Value` keeps that value's backing
arrays alive itself. `DocSet`, `ShapeCache`, and `KeyInterner` own their arena
storage and serialize construction: values they hand out stay valid as they
grow, and concurrent reads are safe once writing stops. `Store` serializes
mutations and publishes immutable snapshots; snapshot reads are concurrent-safe
and never block a writer. Values returned by a snapshot borrow that snapshot's
storage.

Compiled encoders, decoders, keys, and pointers are immutable and safe for
concurrent use. Destinations and source buffers remain caller-owned; each
`Reader` or `Writer` belongs to one goroutine. The complete rules are in the
[architecture and safety record](docs/architecture.md).

## Support and project records

- [Toolchain and compiler support](docs/toolchain.md)
- [Resource and input limits](docs/contracts/limits.md)
- [Unsafe inventory](UNSAFE.md)
- [Architecture and safety](docs/architecture.md)
- [Mutable Store operations](docs/store.md)
- [Security policy](SECURITY.md)
- [Contributing and local gates](CONTRIBUTING.md)

The repository does not yet have a root project license. `LICENSE-GO` and
`LICENSE-SIMDJSON` cover identified upstream-derived material only. Selecting a
project license and completing the final notice remain release blockers; no
license is implied by this README.
