# vibejson

[![ci](https://github.com/thesyncim/vibejson/actions/workflows/ci.yml/badge.svg)](https://github.com/thesyncim/vibejson/actions/workflows/ci.yml)

`vibejson` is a pure-Go JSON library built for hot paths:

- compiled typed encoding and decoding, cached by Go type;
- strict validation, framed streams, JSON Pointer, and caller-backed
  structural navigation;
- zero heap allocations on warmed hot paths, with explicit borrowed or
  owned lifetimes on every surface;
- an optional Go-native SIMD lane with full portable parity.

The module has no dependencies outside the Go standard library, no
assembly, no C, no `go:linkname`, and no private runtime-layout
assumptions. CI enforces the empty dependency set by name.

The document database that grew inside this repository lives at
[vibedb](https://github.com/thesyncim/vibedb): collections, durable
storage, the query engine, and SQL, built on this library and its `x/`
packages.

The project is pre-v1. APIs may change. See [Status](#status).

## Install

Go 1.26 builds the supported portable implementation:

```sh
go get github.com/thesyncim/vibejson@latest
```

The optional SIMD lane requires the exact development compiler pinned in
`scripts/bootstrap-gotip.sh`; the portable build never needs it.

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
| Low-level shared kernels | the `x/` packages (see below) |

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

`GetRaw` resolves one RFC 6901 pointer. `BuildIndex` validates once and
lays out a structural tape in caller-provided storage:

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

The index borrows both `src` and `storage`. `Parse` is the owning
alternative.

`Reader` accepts NDJSON or concatenated top-level values. `DecodeNext`
combines framing with a compiled decoder. Set
`ReaderOptions.MaxValueBytes` for untrusted input; zero means unbounded.

## The x/ packages

`x/` holds the low-level kernels this library and vibedb share:
`x/scanner` (structural scanning), `x/kernels` (SIMD and portable byte
kernels), `x/byteview`, `x/floatconv`, and `x/jsonfields`. They are
exported so a separate module can build on them and carry the same
contract as their upstream namesake: usable, versioned with the module,
and **not** covered by any stability promise. Reach for the root package
first; reach for `x/` when you are building an engine.

## Allocation and ownership

Caller-buffered hot APIs include `Encoder.AppendJSON`, `BuildIndex`, and
the streaming reader and writer. These paths avoid heap allocation after
their capacities and caches are warm. Custom methods, dynamic interface
types, cold compilation, new high-water marks, and undersized
destinations may allocate. This is a per-operation contract.

`RawValue`, structural indexes, zero-copy decode strings, and reader
cursors have explicit borrowed lifetimes. Default typed decoding and
`Parse` own every string they expose. Never store a Go pointer in
external memory or hide one in an integer; [UNSAFE.md](UNSAFE.md)
records every production unsafe scope.

## SIMD and validation

The optional accelerated source uses Go's `simd/archsimd` API on
supported amd64 and arm64 builds. Every accelerated kernel has a
portable implementation and parity coverage. The source window excludes
unvalidated future compiler families.

Build the pinned compiler and run both modes:

```sh
./scripts/bootstrap-gotip.sh "$HOME/sdk/vibejson-gotip"
"$HOME/sdk/vibejson-gotip/bin/go" test ./...
GOEXPERIMENT=simd "$HOME/sdk/vibejson-gotip/bin/go" test ./...
```

CI also checks stable Go, vet, generated source, race- and
checkptr-sensitive paths, corpora, cross-builds, the unsafe inventory,
test ownership, and ISA guards. Contributor commands and benchmark
policy are in [CONTRIBUTING.md](CONTRIBUTING.md).

## Status

Current truth comes from exported Go documentation and tests; this
README summarizes that surface. Public APIs remain pre-v1, and the `x/`
packages are explicitly unstable.

The repository has no root project license. `LICENSE-GO` and
`LICENSE-SIMDJSON` apply only to identified upstream-derived material.
Source lineage is recorded in [provenance](docs/provenance.md),
disclosure guidance in [security](SECURITY.md), and every production
unsafe scope in the generated [unsafe inventory](UNSAFE.md).
