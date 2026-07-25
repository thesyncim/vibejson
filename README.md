# vibejson

[![ci](https://github.com/thesyncim/vibejson/actions/workflows/ci.yml/badge.svg)](https://github.com/thesyncim/vibejson/actions/workflows/ci.yml)

`vibejson` is a pure-Go JSON stack:

- compiled typed encoding and decoding;
- strict validation, streams, JSON Pointer, and caller-backed navigation;
- an in-memory keyed store with snapshots, updates, deletes, TTL, optional
  schemas, and nested or compound exact indexes;
- a bounded-residency durable store with automatic copy-on-write persistence;
- a compiled single-collection query engine.

The root module's only dependency outside the standard library is
`golang.org/x/sys`, the syscall shim `internal/storeio` needs for file locking,
`F_FULLFSYNC`, and `O_DIRECT`. It has no assembly, C, `go:linkname`, or private
runtime-layout assumptions. CI enforces the dependency set by name.

The project is pre-v1 and has no root project license. APIs and persistent
formats may change. See [Status](#status).

## Install

Go 1.26 builds the supported portable backend:

```sh
go get github.com/thesyncim/vibejson@latest
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
| Immutable document segments | package `store`: `Segment`, `ShapeCache` |
| Mutable in-memory documents | package `store`: `Collection`, `Snapshot`, `Builder` |
| Durable documents with bounded residency | package `store/durable`: `Collection`, `Snapshot` |
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
if err := vibejson.Unmarshal(src, &event); err != nil {
	return err
}
encoded, err := vibejson.Marshal(&event)
```

Hot paths compile once and retain output capacity:

```go
encoder, err := vibejson.CompileEncoder[Event](vibejson.EncoderOptions{})
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
entries, err := vibejson.RequiredIndexEntries(src)
if err != nil {
	return err
}
storage := make([]vibejson.IndexEntry, 0, entries)
document, err := vibejson.BuildIndex(src, storage)
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

Package `store` is the canonical keyed-storage API: the in-memory engine,
immutable snapshots, and document segments (`Segment`, `ShapeCache`) live there,
separate from the root JSON package. A `Segment` is an immutable, self-contained
batch of documents carrying its own tape and column machinery — what Lucene and
Druid call a segment — and a `Collection` chunk embeds one. One
`store.Collection` is one mutable in-memory JSON collection:

```go
db, err := store.New(store.Options{
	ChunkDocuments: 16,
	ShapeTapes:      true,
})
if err != nil {
	return err
}

_, err = db.Put(
	"user:42",
	[]byte(`{"tenant":"acme","profile":{"country":"PT"},"score":7}`),
)
if err != nil {
	return err
}

snapshot := db.Snapshot()
db.SetTTL("user:42", 30*time.Minute)
db.Delete("user:42")

// The old immutable view remains valid.
raw, ok := snapshot.GetRaw("user:42")
```

`Put` copies its inputs. Updates parse only the replacement. Deletes leave no
tombstone or later compaction work. Snapshot reads do not take the writer lock
or check the clock.

Optional schemas constrain root and nested RFC 6901 paths while allowing
unspecified fields. Exact indexes accept one to four paths, including nested and
order-sensitive compound keys. Online construction remains exact through scan
fallback until `BackfillIndex` reaches `store.IndexReady`. Hashes only select
candidates; values are rechecked exactly. Indexed counts popcount final masks
without decoding projected JSON.

Package `store/durable` is the general durable path. `durable.Create` and
`durable.Open` acquire one exclusive writer lease for the file. Each mutation
automatically publishes checksummed copy-on-write pages through alternating
roots; applications do not rewrite a checkpoint after every change.
`Synchronous: true` waits for the data and root durability barriers. Async
callers use `Flush`, `DurableGeneration`, or `Close` to establish the durable
boundary.

Its fixed page-cache budget allows the file to exceed RAM without making the Go
heap proportional to row count. That is a residency property, not a claim that
cold storage has memory latency. Close every `durable.Snapshot`; its generation
lease delays physical reuse of retired extents.

The exact storage, ownership, TTL, schema, index, recovery, memory, and
larger-than-RAM contracts are in [docs/store.md](docs/store.md).

## Queries

The `query` package compiles the Go builder, a JSON query document, or the
supported SQL subset into the same immutable typed plan. Query text is not
retained or interpreted during execution.

A query is expressible as a JSON document, so it can be stored, logged, or
received over a wire. Sibling keys of a filter conjoin, so an all-of condition
reads as data rather than as a tree of constructors:

```go
q, err := query.New(query.M{
	"select": query.A{
		"profile.country",
		query.M{"total": query.M{"$sum": "score"}},
		query.M{"n": query.M{"$count": nil}},
	},
	"where": query.M{
		"tenant": "acme",
		"score":  query.M{"$gte": 5},
		"tier":   query.M{"$in": query.A{"pro", "team"}},
	},
	"groupBy": "profile.country",
	"orderBy": query.A{query.M{"total": "desc"}},
	"limit":   20,
})
```

`query.Parse` compiles the same document from JSON text, preserving each
number's original spelling so an integer past float64's exact range stays
exact. Filter operators are `$eq`, `$ne`, `$lt`, `$lte`, `$gt`, `$gte`, `$in`,
`$nin`, `$exists`, `$null`, `$contains`, `$and`, `$or`, and `$not`; aggregates
are `$count`, `$sum`, `$avg`, `$min`, and `$max`. Paths are dotted
(`profile.country`) or RFC 6901 pointers.

The builder is unchanged and compiles to the same plan:

```go
q := query.Select(
	query.Path("profile.country"),
	query.Count(),
	query.Sum("score"),
).
	Where(query.And(
		query.Cmp("tenant", query.Eq, "acme"),
		query.In("tier", "pro", "team"),
	)).
	GroupBy("profile.country").
	OrderBy("profile.country", query.Asc).
	Limit(20)
if err := q.Prepare(); err != nil { // optional: fail here rather than at first Run
	return err
}

var e query.Exec
err := q.RunInto(&e, query.FromSnapshot(db.Snapshot()))
```

Execution has two entry points, `Run` and `RunInto`, and one `Source` handle
naming the collection: `query.FromSegment`, `query.FromSnapshot`,
`query.FromFile`, or `query.FromDatabase`. A backend is therefore never a
different call shape from
another — swapping a heap snapshot for a durable one changes the `Source`, not
the call. `Exec` carries the caller-owned storage a hot loop reuses: the
destination `Result`, the scratch `Workspace`, the `ExecOptions` the durable
backend reads, and the `ExecStats` it reports.

Implemented operations are projection; `COUNT`, `SUM`, `AVG`, `MIN`, and `MAX`;
comparisons; membership; existence, null, and containment predicates; Boolean
composition; cross-collection semi-joins; grouping; stable ordering; and
limits.

### Cross-collection semi-joins

A `join` clause filters the driving collection by the existence of a matching
document in another one. It is a filter and only a filter: no inner column
reaches the result, and several matching inner documents keep the outer row
exactly once. Projecting inner columns is a different operator with a different
result cardinality, and a join clause that asks for one is rejected rather than
silently answered as a filter.

```go
q, err := query.Parse([]byte(`{
	"select": ["id"],
	"where":  {"active": true},
	"join":   [{"from": "customers", "on": {"customer_id": "$key"}, "where": {"tier": "pro"}}]
}`))

catalog := db.Snapshot() // one DatabaseSnapshot, every collection at one instant
result, err := q.Run(query.FromDatabase(catalog, "orders"))
```

`"on"` maps an outer path to an inner one; `"$key"` names the inner
collection's primary key, the common foreign-key case, and any other spelling
is an ordinary path into the inner documents. The builder form is
`query.JoinOn("customers", "customer_id", query.JoinKey).Where(...)` passed to
`Query.Join`. A null or absent value on either side joins to nothing, the rule
comparisons and memberships already follow.

Both sides are read from one `store.DatabaseSnapshot`, so snapshot skew across
a join is not expressible: `query.FromDatabase` takes the catalog and the
driving collection's name together, and a plan with a join is rejected by every
`Source` that names one collection.

The strategy is measured at execution rather than estimated, because this
engine keeps no cardinality statistics and the cost of guessing wrong spans two
orders of magnitude. The inner side runs first and its join keys are collected
until they pass a threshold. Under it, the collected values are pushed into the
outer predicate as a membership, which lowers to one index-mask intersection
when the outer path carries a declared exact index. Over it, the set is
abandoned and the join runs as a keyed lookup: scan the outer, probe the inner
once per row, with no build side and nothing materialized.

A lookup-bound join also builds a **semi-join reduction filter** — a blocked
Bloom filter over the inner side's surviving join keys — so that outer rows with
no partner are rejected from one cache line instead of a lookup, a document
decode, and a predicate evaluation. The filter's error is one-sided, so the
exact probe behind it stays authoritative and no configuration of it can return
a wrong row. Building it means finishing the inner scan, so the binder decides
twice: once before the scan from cardinalities it knows exactly, and again after
every batch from the inner predicate's observed selectivity, abandoning a scan
that can no longer pay. Measured over a 20,000-row driving collection against
40,000 inner rows, that is 1.8× faster when 1% of the inner side matches, 1.6×
at 10%, and within noise of not building one at 50% and 100%, where the filter
is abandoned mid-scan (`BenchmarkJoinBloomPrefilter`).

Every one of these choices changes the cost and never the answer, and
`ExecStats` reports which was taken. Against the hand-written two-query
equivalent over 20,000 rows, the join is 1.2× to 2.4× faster and allocates
nothing where the hand-written form allocates up to 20,011 times
(`BenchmarkJoinTwoQueryFilter`).

A disjunction of equalities on one path is a membership by definition, so
compilation rewrites it into one: `Or(Cmp(p, Eq, a), Cmp(p, Eq, b))`, SQL's
`p = a OR p = b`, and `{"p": {"$in": [a, b]}}` all reach the same compiled
form. A membership sorts and deduplicates its alternatives once, so each row
costs a search rather than one comparison per alternative — measured at 50×
fewer nanoseconds per row over 256 alternatives, and flat as the set grows
(`BenchmarkMembershipEval`).

Queries can run over `Segment`, `store.Snapshot`, or `durable.Snapshot`. Heap
snapshots late-bind exact indexes. Durable execution supports persistent index
bounds, bounded parallel batches, numeric covering columns, and spill files for
ordered or grouped state.

Queries are single-collection. Joins, subqueries, mutations, window functions,
full SQL, and a network protocol are not implemented.

## Allocation and ownership

Caller-buffered hot APIs include:

- `Encoder.AppendJSON`;
- `BuildIndex`;
- snapshot `AppendRaw`;
- bitmap/index appenders;
- query `RunInto` with a reusable `Exec`.

These paths can avoid heap allocation after their capacities and caches are
warm. Custom methods, dynamic interface types, cold compilation, new high-water
marks, undersized destinations, and ordinary mutations may allocate. This is a
per-operation contract, not a claim that the current in-memory collection has a
row-count-independent Go heap.

`RawValue`, structural indexes, zero-copy decode strings, reader cursors, and
snapshot results have explicit borrowed lifetimes. Default typed decoding and
`Parse` own exposed strings. Never store a Go pointer in external memory or hide
it in an integer; [UNSAFE.md](UNSAFE.md) records every production unsafe scope.

Collections serialize writes and publish immutable snapshots. A prepared query is
concurrent-safe when each execution has its own result and workspace. Readers,
writers, builders, mutable caches, and workspaces are single-consumer.

## SIMD and validation

The optional accelerated source uses Go's `simd/archsimd` API on supported
amd64 and arm64 builds. Every accelerated kernel has a portable implementation
and parity coverage. The source window excludes unvalidated future compiler
families.

Build the pinned compiler and run both modes:

```sh
./scripts/bootstrap-gotip.sh "$HOME/sdk/vibejson-gotip"
"$HOME/sdk/vibejson-gotip/bin/go" test ./...
GOEXPERIMENT=simd "$HOME/sdk/vibejson-gotip/bin/go" test ./...
```

CI also checks stable Go, vet, generated source, race/checkptr-sensitive paths,
corpora, cross-builds, unsafe inventory, test ownership, and relevant ISA
guards. Contributor commands and benchmark policy are in
[CONTRIBUTING.md](CONTRIBUTING.md).

## Status

Current truth comes from exported Go documentation and tests. This README and
the store guide summarize that surface; historical roadmaps and implementation
journals are intentionally absent.

Current product boundaries:

- no replication, backup manager, or point-in-time restore;
- no server or wire protocol;
- no distributed or cross-file transactions;
- no durable multi-collection catalog;
- no query joins;
- persistent formats and public APIs remain pre-v1.

The repository has no root project license. `LICENSE-GO`, `LICENSE-SIMDJSON`,
and `LICENSE-ROARING` apply only to identified upstream-derived material.
Source lineage is recorded in [provenance](docs/provenance.md), disclosure
guidance in [security](SECURITY.md), and every production unsafe scope in the
generated [unsafe inventory](UNSAFE.md).
