# slopjson

[![ci](https://github.com/thesyncim/slopjson/actions/workflows/ci.yml/badge.svg)](https://github.com/thesyncim/slopjson/actions/workflows/ci.yml)

`slopjson` is a pure-Go JSON stack:

- compiled typed encoding and decoding;
- strict validation, streams, JSON Pointer, and caller-backed navigation;
- an in-memory keyed store with snapshots, updates, deletes, TTL, optional
  schemas, and nested or compound exact indexes;
- a bounded-residency durable store with automatic copy-on-write persistence;
- a compiled single-collection query engine.

The root module has no third-party dependencies, assembly, C, `go:linkname`, or
private runtime-layout assumptions.

The project is pre-v1 and has no root project license. APIs and persistent
formats may change. See [Status](#status).

## Install

Go 1.26 builds the supported portable backend:

```sh
go get github.com/thesyncim/slopjson@latest
```

The optional Go-native SIMD backend requires the exact development compiler
pinned in `scripts/bootstrap-gotip.sh`. It is not required for the portable
build.

Users of the former module path should read [MIGRATION.md](MIGRATION.md).

## API map

| Need | Start with |
| --- | --- |
| Typed JSON | `Marshal`, `Unmarshal`, `CompileEncoder`, `CompileDecoder` |
| Validation and formatting | `Valid`, `Validate`, `Compact`, `Indent`, `Canonicalize` |
| Framed input or token output | `Reader`, `Writer`, `DecodeNext`, `EncodeTo` |
| One borrowed selection | `GetRaw`, `CompilePointer` |
| Repeated document navigation | `BuildIndex`, `Index`, `Node` |
| Owning ordered dynamic data | `Parse`, `Value` |
| Immutable document batches | `DocSet`, `ShapeCache` |
| Mutable in-memory documents | `Store`, `Snapshot`, `StoreBuilder` |
| Durable documents with bounded residency | `FileStore`, `FileSnapshot` |
| Filtering, grouping, ordering, and aggregation | package `query` |

## Typed JSON

Convenience calls compile and cache a plan by Go type:

```go
type Event struct {
	ID     int      `json:"id"`
	Name   string   `json:"name"`
	Labels []string `json:"labels"`
}

var event Event
if err := slopjson.Unmarshal(src, &event); err != nil {
	return err
}
encoded, err := slopjson.Marshal(&event)
```

Hot paths compile once and retain output capacity:

```go
encoder, err := slopjson.CompileEncoder[Event](slopjson.EncoderOptions{})
if err != nil {
	return err
}

buf = buf[:0]
buf, err = encoder.AppendJSON(buf, &event)
```

Compiled encoders and decoders are immutable and concurrent-safe.

## Documents and streams

`GetRaw` resolves one RFC 6901 pointer. `BuildIndex` validates once and lays out
a structural tape in caller-provided storage:

```go
entries, err := slopjson.RequiredIndexEntries(src)
if err != nil {
	return err
}
storage := make([]slopjson.IndexEntry, 0, entries)
document, err := slopjson.BuildIndex(src, storage)
if err != nil {
	return err
}
name, ok := document.Root().Get("profile")
```

The index borrows both `src` and `storage`. `Parse` is the owning alternative.

`Reader` accepts NDJSON or concatenated top-level values. `DecodeNext` combines
framing with a compiled decoder. Set `ReaderOptions.MaxValueBytes` for untrusted
input; zero means unbounded.

## Store

`Store` is one mutable in-memory JSON collection:

```go
store := slopjson.NewStore(slopjson.StoreOptions{
	ChunkDocuments: 16,
	ShapeTapes:      true,
})

_, err := store.Put(
	"user:42",
	[]byte(`{"tenant":"acme","profile":{"country":"PT"},"score":7}`),
)
if err != nil {
	return err
}

snapshot := store.Snapshot()
store.SetTTL("user:42", 30*time.Minute)
store.Delete("user:42")

// The old immutable view remains valid.
raw, ok := snapshot.GetRaw("user:42")
```

`Put` copies its inputs. Updates parse only the replacement. Deletes leave no
tombstone or later compaction work. Snapshot reads do not take the writer lock
or check the clock.

Optional schemas constrain root and nested RFC 6901 paths while allowing
unspecified fields. Exact indexes accept one to four paths, including nested and
order-sensitive compound keys. Online construction remains exact through scan
fallback until `BackfillIndex` reaches `StoreIndexReady`.

`FileStore` is the general durable path. Each mutation automatically publishes
checksummed copy-on-write pages through alternating roots; applications do not
rewrite a checkpoint after every change. `Synchronous: true` waits for the data
and root durability barriers. Async callers use `Flush`, `DurableGeneration`,
or `Close` to establish the durable boundary.

Its fixed page-cache budget allows the file to exceed RAM without making the Go
heap proportional to row count. That is a residency property, not a claim that
cold storage has memory latency. Close every `FileSnapshot`; its generation
lease delays physical reuse of retired extents.

The exact storage, ownership, TTL, schema, index, recovery, memory, and
larger-than-RAM contracts are in [docs/store.md](docs/store.md).

## Queries

The `query` package compiles the Go builder or supported SQL subset into the
same immutable typed plan. SQL text is not retained or interpreted during
execution.

```go
plan, err := query.Select(
	query.Path("profile.country"),
	query.Count(),
	query.Sum("score"),
).
	Where(query.Cmp("tenant", query.Eq, "acme")).
	GroupBy("profile.country").
	OrderBy("profile.country", query.Asc).
	Limit(20).
	Prepare()
if err != nil {
	return err
}

var result query.Result
var workspace query.Workspace
err = plan.RunSnapshotInto(&result, store.Snapshot(), &workspace)
```

Implemented operations are projection; `COUNT`, `SUM`, `AVG`, `MIN`, and `MAX`;
comparisons; existence, null, and containment predicates; Boolean composition;
grouping; stable ordering; and limits.

Plans can run over `DocSet`, `Snapshot`, or `FileSnapshot`. Heap snapshots
late-bind exact indexes. Durable execution supports persistent index bounds,
bounded parallel batches, numeric covering columns, and spill files for ordered
or grouped state.

Queries are single-collection. Joins, subqueries, mutations, window functions,
full SQL, and a network protocol are not implemented.

## Allocation and ownership

Caller-buffered hot APIs include:

- `Encoder.AppendJSON`;
- `BuildIndex`;
- snapshot `AppendRaw`;
- bitmap/index appenders;
- query `RunInto` methods with reusable results and workspaces.

These paths can avoid heap allocation after their capacities and caches are
warm. Custom methods, dynamic interface types, cold compilation, new high-water
marks, and undersized destinations may allocate.

`RawValue`, structural indexes, zero-copy decode strings, reader cursors, and
snapshot results have explicit borrowed lifetimes. Default typed decoding and
`Parse` own exposed strings. Never store a Go pointer in external memory or hide
it in an integer; [UNSAFE.md](UNSAFE.md) records every production unsafe scope.

Stores serialize writes and publish immutable snapshots. A prepared query is
concurrent-safe when each execution has its own result and workspace. Readers,
writers, builders, mutable caches, and workspaces are single-consumer.

## SIMD and validation

The optional accelerated source uses Go's `simd/archsimd` API on supported
amd64 and arm64 builds. Every accelerated kernel has a portable implementation
and parity coverage. The source window excludes unvalidated future compiler
families.

Build the pinned compiler and run both modes:

```sh
./scripts/bootstrap-gotip.sh "$HOME/sdk/slopjson-gotip"
"$HOME/sdk/slopjson-gotip/bin/go" test ./...
GOEXPERIMENT=simd "$HOME/sdk/slopjson-gotip/bin/go" test ./...
```

CI also checks stable Go, vet, generated source, race/checkptr-sensitive paths,
corpora, cross-builds, unsafe inventory, test ownership, and relevant ISA
guards. Contributor commands and benchmark policy are in
[CONTRIBUTING.md](CONTRIBUTING.md).

## Status

Current truth comes from exported Go documentation and tests. This README and
the Store guide summarize that surface; historical roadmaps and implementation
journals are intentionally absent.

Current product boundaries:

- no replication, backup manager, or point-in-time restore;
- no server or wire protocol;
- no distributed or cross-file transactions;
- no durable multi-collection catalog;
- no query joins;
- persistent formats and public APIs remain pre-v1.

The repository has no root project license. `LICENSE-GO` and
`LICENSE-SIMDJSON` apply only to identified upstream-derived material. The
canonical records are [provenance](docs/provenance.md),
[security](SECURITY.md), [test ownership](TEST_CONTRACTS.md), and the generated
[unsafe inventory](UNSAFE.md).
