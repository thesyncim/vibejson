# vibejson

[![ci](https://github.com/thesyncim/vibejson/actions/workflows/ci.yml/badge.svg)](https://github.com/thesyncim/vibejson/actions/workflows/ci.yml)

`vibejson` is a pure-Go JSON stack:

- compiled typed encoding and decoding;
- strict validation, streams, JSON Pointer, and caller-backed navigation;
The document database that grew inside this repository — collections,
durable storage, SQL, and the query engine — now lives at
[vibedb](https://github.com/thesyncim/vibedb), built on this library.

The module has no dependencies outside the Go standard library, no
assembly, no C, no `go:linkname`, and no private runtime-layout
assumptions. CI enforces the empty dependency set by name.

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

## Storage, queries, and SQL

Collections, durable storage, the query engine, and the SQL surface live
in [vibedb](https://github.com/thesyncim/vibedb), which builds on this
library and its `x/` packages.

The `sql` package parses a small SQL dialect — bounded by what the executor can
run, not by what SQL can express — and `query.PrepareStatement` lowers it onto
the same compiled plan the builder produces. The `vibesql` package exposes that
as an in-process `database/sql` driver:

```go
db, _ := sql.Open("vibejson", "/var/lib/app/users.vj")
rows, _ := db.Query(`SELECT name, age FROM users WHERE age >= ? ORDER BY age`, 21)
```

Parsing is not free per query; it is free in steady state. A prepared statement
compiles once, and one without placeholders then executes the identical plan on
the identical executor as the builder. One with placeholders re-lowers per bind
into a retained compiler's arenas: zero allocations, but real work. An ad-hoc
query parses and compiles on every call.

NULL is three-valued as SQL's is: lowering builds separate "is TRUE" and "is
FALSE" predicates and recurses through Kleene's tables, so `NOT (x = 1)` drops a
null `x` rather than keeping it.

`JOIN` is SQL's inner join, and a `WHERE` term over the joined collection is
moved into the join clause's own filter — but only where that is provably the
same question, which is a top-level ANDed term naming one collection. Under an
`OR` the move would drop rows SQL keeps, so such a term is refused instead. The
deviations that remain — absent and null being one value, within-type
comparison, numeric-only `MIN`/`MAX`, null ordering, and what a join still
refuses — are listed in one place in the `vibesql` package documentation.

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
