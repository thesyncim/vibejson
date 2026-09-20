# Architecture

`vibejson` has one portable JSON implementation and optional SIMD kernels. The
portable implementation defines behavior; accelerated kernels must preserve
accepted input, output bytes, errors, ownership, and retained memory.

## Invariants

Production paths preserve four properties:

1. Validation, framing, conversion, and navigation agree on what constitutes one
   strict JSON value.
2. Every result either owns its storage or documents the source and invalidation
   boundary it borrows.
3. Every architecture-specific path has a portable implementation and parity
   coverage.
4. Pooled or retained scratch is cleared before reuse and bounded when input can
   otherwise control its high-water mark.

Unsafe code is allowed only with explicit bounds, layout, lifetime, aliasing,
and GC-visibility proofs. The [unsafe inventory](../UNSAFE.md) lists every
production scope, its invariant family, required tests, and representative
benchmarks.

## Package map

```text
applications
    |
    v
vibejson (typed codecs, streams, selection, indexes, ordered values)
    |
    +-- document     shared kinds, options, and pointer errors
    +-- simd         numeric/time helpers and backend reporting
    +-- x/scanner    byte and string scanners
    +-- x/kernels    structural classification and Stage 2 machines
    +-- x/byteview   checked read-only byte and string views
    +-- x/floatconv  decimal-to-binary conversion
    +-- x/jsonfields struct-field resolution
```

The root package is the application API. `document` and `simd` are supporting
pre-v1 surfaces. The `x/` packages are exported for shared implementation
contracts and carry no compatibility promise. Database and persistence layers
live in [vibedb](https://github.com/thesyncim/vibedb).

## Typed codecs

`CompileEncoder[T]` and `CompileDecoder[T]` inspect `T` once and build immutable
plans containing field resolution, scalar widths, sequence operations, custom
method dispatch, option behavior, and scratch requirements. Compiled plans are
safe for concurrent use; mutable operation state belongs to the call or to
bounded plan scratch. `Marshal` and `Unmarshal` cache default plans per type.

Encoder scratch contains reflection boxes, map sorting storage, and reusable
value backing. Decoder scratch contains receiver storage, presence sets,
structural storage, and `Replace` alias metadata. Scratch is cleared before it
returns to a pool or cache so it cannot retain an object graph.

Default typed decoding stores retained keys, strings, and number text in
append-only result-owned blocks. Zero-copy mode borrows eligible source spans;
escaped text is always materialized. Existing maps, pointers, and fields follow
`encoding/json` merge behavior unless `DecoderOptions.Replace` is selected.
Replace mode clears absent fields, replaces map contents, reuses unique storage,
and detaches later aliases when shared storage is detected.

Large homogeneous numeric slices have specialized routes. SIMD structural
discovery may feed the shared scalar number parser, which remains authoritative
for grammar, exact conversion, errors, and partial-destination behavior. On
arm64, compact top-level arrays with at least 16 positive 16-digit integer
values can use a validated four-at-a-time conversion loop. Other widths, signs,
spacing, sizes, architectures, and compiler lanes use the ordinary decoder.

## Validation and indexing

Fast validation classifies strings, scalars, and structural characters before
grammar code verifies one complete value. The diagnostic path reports the public
`SyntaxError` and exact byte offset. With an accelerated Stage 1 backend,
eligible large inputs can consume packed structural positions; density sampling
keeps number-dense inputs on the recursive route. Index construction always
builds the structural tape because navigation needs it.

`BuildIndex` writes compact entries into caller-provided storage. Nodes retain
source spans, container metadata, and optional key hashes, so navigation does
not materialize a general-purpose tree. An index borrows both source bytes and
entry storage; reusing either invalidates its nodes. `Parse` owns the source and
entry storage. `GetRaw` validates and resolves one RFC 6901 pointer without
retaining an index, while `ScanFirstRaw` intentionally returns the first
duplicate rather than the last.

## Streaming

`Reader` owns a rolling byte buffer. `Next` frames and validates one top-level
value, and `DecodeNext` frames it through a compiled plan. Fragmented input is
scanned incrementally. `Reader.Bytes`, `ValueCursor`, and zero-copy decoded
values borrow the rolling buffer and become invalid after any reader advance,
including an advance that returns false.

`Writer` owns one reusable output buffer and a container-state stack. Compiled
values enter through `EncodeTo`; token methods share the buffer while rejecting
invalid object and array transitions. Sink and usage errors are sticky because
an `io.Writer` may already have accepted an output prefix.

## Ownership summary

| Operation | Source/result relationship |
| --- | --- |
| Default typed decoding | Result owns retained textual data |
| Zero-copy typed decoding | Eligible results borrow the input |
| `RawValue` and one-pass callbacks | Borrow caller input |
| `BuildIndex` | Borrows source and entry storage |
| `Parse` | Owns source and entry storage |
| `ParseOptions` with `Options.ZeroCopy` | Borrows source and owns entry storage |
| Reader views and cursors | Borrow the rolling reader buffer |
| Append-style encoders and transforms | Return caller-owned output |

## Portable and SIMD lanes

Go 1.27 is the minimum release. `GOEXPERIMENT=simd` enables the validated
Go-native SIMD sources; builds without it and unsupported architectures select
portable fallbacks. `simd.Current()` reports the effective backend and vector
width.

On amd64, string scanning and structural classification select AVX2 at startup
when available, including default `GOAMD64=v1` binaries. Baseline wrappers do
not contain AVX instructions, so older CPUs remain safe; `GOAMD64=v3` builds
use direct AVX2 calls. Structural classification processes two 32-byte vectors
per 64-byte block. On arm64, NEON is the selected backend. Baseline amd64
record decoding keeps its raw cursor route, while direct v3 and fused arm64
routes retain structural execution when measurements justify it.

Architecture-specific implementations must preserve:

- accepted and rejected inputs;
- exact output bytes;
- error types and offsets;
- source and result ownership; and
- retained-memory and concurrency contracts.

On arm64, fixed-width decimal and timestamp formatting use an eight-lane
16-bit digit-pair formatter. Each pair is at most 99, so multiplication by 103
fits in 16 bits and a right shift by ten performs exact division by ten. Tests
cover all four-digit lanes and timestamp output.

## Generated and external material

Generated decoder code, float tables, corpus models, and the unsafe inventory
are reviewable outputs of checked-in generators. Reproduce them with `go
generate ./...` and run the corresponding inventory or corpus checks.

[Provenance](provenance.md) records external source, algorithms, revisions,
licenses, local changes, and integrity evidence. Missing history remains
explicitly unresolved rather than receiving a guessed attribution.
