# Architecture

`vibejson` is one JSON engine with several access models. The portable
implementation is the behavioral reference; optional SIMD code may replace
selected low-level kernels without changing public results, errors, ownership,
or output bytes.

## Design invariants

Every production path is expected to preserve four properties:

1. **Strict correctness.** Validation, framing, typed conversion, and document
   navigation agree on what constitutes one JSON value.
2. **Explicit ownership.** An API either owns its result or documents the exact
   storage and invalidation boundary it borrows.
3. **Portable parity.** Architecture-specific kernels have a portable
   implementation with differential coverage.
4. **Bounded reuse.** Retained scratch is cleared before reuse and capped where
   an input-controlled high-water mark could otherwise retain arbitrary memory.

Performance work is accepted only after those invariants are demonstrated.

## Package map

```text
applications
    |
    v
vibejson (typed codecs, streams, selection, indexes, ordered values)
    |
    +-- document     shared kinds, index options, pointer errors
    +-- simd         numeric/time helpers and effective backend reporting
    |
    +-- x/scanner    byte and string scanners
    +-- x/kernels    structural classification and Stage 2 machines
    +-- x/byteview   checked-by-caller read-only views
    +-- x/floatconv  decimal-to-binary conversion
    +-- x/jsonfields struct-field resolution
```

The root package is the application API. `document` and `simd` are pre-v1
supporting surfaces. The `x/` packages are public only so sibling modules can
share implementation contracts; they are versioned with this repository but
carry no compatibility promise.

The database, persistence, query, and SQL layers live in
[vibedb](https://github.com/thesyncim/vibedb). They are consumers of this
module, not hidden parts of the JSON package.

## Typed codec pipeline

`CompileEncoder[T]` and `CompileDecoder[T]` inspect `T` once and build immutable
node graphs. The graphs capture:

- field visibility, dominance, tags, and embedded-pointer hops;
- scalar width and specialized sequence operations;
- standard and native custom-method dispatch;
- option-specific behavior such as HTML escaping, `Replace`, and `InlineFields`;
  and
- bounded scratch requirements.

Convenience `Marshal` and `Unmarshal` calls cache a default plan per Go type.
Explicitly compiled codecs hold their own option-specific plan and may be reused
concurrently.

Mutable state is operation-local or checked out from bounded plan scratch.
Encoder scratch contains reflection boxes, map sorting storage, and reusable
value backing. Decoder scratch contains reflection boxes, receiver arenas,
presence sets, structural storage, and Replace-mode alias metadata. Scratch is
cleared before it returns to a pool or plan cache so it cannot retain a decoded
or encoded object graph.

Default typed decoding packs retained keys, string values, and textual numbers
into append-only result-owned blocks. It does not keep the complete input alive,
and an existing independently owned string is reused when the next document
contains the same value. Zero-copy mode instead borrows eligible source spans.
Escaped text is always materialized into independent storage. Dynamic
whole-document decoding sizes its retained-text arena before materialization;
nested dynamic fields share the typed cursor's current owned block.

Large structurally eligible record roots use the raw compiled cursor when the
selected Stage 1 backend is scalar: producing a structural tape costs more than
the record can recover. Architecture-accelerated backends retain the structural
executor. Root slices and arrays keep their compiled shape routes, including
the homogeneous numeric paths below.

### Homogeneous numeric slices

Large built-in `[]float64` values can consume the Stage 1 structural-position
stream directly. Delimiter discovery is vectorized when the SIMD lane is
selected, while the shared scalar number scanner remains authoritative for
grammar, exact conversion, errors, and partial-destination behavior.

On arm64 SIMD builds, compact top-level arrays containing at least 16 positive
16-digit `int64` or `uint64` values have a narrower batch route. It validates
the complete array shape before touching the destination, converts four values
per loop, and otherwise falls back to the ordinary typed decoder. The route
also preserves named integer types, destination reuse, and zero-allocation hot
calls. Other widths, signs, spacing, sizes, architectures, and compiler lanes
continue through the shared decoder.

### Merge and Replace decoding

Default decoding follows `encoding/json` merge semantics. `Replace` is compiled
into distinct reference operations so ordinary decoders do not pay an option
branch on each value. Replace-mode decoding:

- clears fields the document does not mention;
- replaces map contents rather than merging stale entries;
- reuses unique pointer, slice, and map storage;
- tracks shared or overlapping reference storage and detaches later aliases;
  and
- uses scalable operation-local presence sets for wide records.

The alias tracker stores Go pointers as GC-visible pointers. It does not hide
them in integers or external memory.

## Validation and structural indexing

Validation has a fast classification path and a diagnostic path. Fast kernels
locate strings, scalars, and structural characters; grammar code then verifies
that those tokens form one complete JSON value. When a fast path rejects input,
the diagnostic parser supplies the public `SyntaxError` and exact offset.

Portable `Valid` and `Validate` use the recursive word-at-a-time validator.
With an architecture-accelerated Stage 1 backend, eligible large
whitespace-heavy or string-heavy documents can instead consume packed
structural positions; a density sample keeps number-dense documents on the
recursive route. Index construction uses Stage 1 and Stage 2 in every build
because it must produce the structural tape rather than only a validity bit.

`BuildIndex` extends that pipeline with a compact structural tape in
caller-provided `[]IndexEntry` storage. Each `Node` is a lightweight handle into
the source bytes and tape:

- scalars retain their original source span;
- containers record direct-child counts and the next tape position;
- object keys can be enriched with content hashes; and
- iterators and compiled pointers traverse the tape without materializing a
  general-purpose tree.

An index borrows both its JSON source and entry storage. `Parse` wraps the same
navigation model in an owning root so derived `Value` handles keep their source
and tape alive.

`GetRaw` is the one-shot alternative. It validates and resolves an RFC 6901
pointer without retaining an index. `ScanFirstRaw` is an explicitly different
early-exit contract for callers that want the first duplicate rather than the
last.

## Streaming

`Reader` owns a rolling byte buffer. `Next` frames and validates one top-level
value, while `DecodeNext` frames it and decodes through a compiled plan.
Fragmented input is scanned incrementally so a value spanning many reads is not
reframed from byte zero on every refill.

The current `Reader.Bytes` and `ValueCursor` borrow the rolling buffer. Any call
that advances the reader invalidates that view, even when the advance returns
false. Owned typed decoding copies retained string data; zero-copy decoding
inherits the reader's invalidation window.

`Writer` holds one reusable output buffer and a container-state stack. Compiled
values enter through `EncodeTo`; token methods share the same buffer while
preventing invalid object/array transitions. Sink and usage errors are sticky
because an `io.Writer` may already have accepted an output prefix.

## Ownership model

The primary ownership boundaries are:

| Operation | Source/result relationship |
| --- | --- |
| Default typed decoding | Result owns retained textual data |
| Zero-copy typed decoding | Eligible results borrow the input |
| `RawValue` and one-pass callbacks | Borrow the caller's input |
| `BuildIndex` | Borrows source and entry storage |
| `Parse` | Owns source and entry storage |
| `ParseOptions` with `Options.ZeroCopy` | Borrows source and owns entry storage |
| `Reader` views and cursors | Borrow the rolling reader buffer |
| Append-style encoders/transforms | Return caller-owned output |

Unsafe code is permitted only at a measured boundary with explicit bounds,
layout, lifetime, aliasing, and GC-visibility proofs. The generated
[unsafe inventory](../UNSAFE.md) names every production scope, its invariant
family, required tests, and representative benchmarks.

## Portable and SIMD lanes

Go 1.26 selects portable source. The development compiler pinned by
[`scripts/bootstrap-gotip.sh`](../scripts/bootstrap-gotip.sh) can additionally
select Go-native SIMD files on validated amd64 and arm64 builds with
`GOEXPERIMENT=simd`.

Build constraints bound the experimental source to the compiler family it was
validated against. Unsupported architectures, stable compilers, and future
compiler families select portable fallbacks.

Backend selection is an implementation detail below the public API. Accelerated
implementations must preserve:

- accepted and rejected inputs;
- exact output bytes;
- error types and offsets;
- source/result ownership; and
- retained-memory and concurrency contracts.

The required validation lanes are listed in [CONTRIBUTING.md](../CONTRIBUTING.md).

## Generated and externally derived material

Generated decoder code, float conversion tables, corpus models, and the unsafe
inventory are reproducible inputs to review, not opaque vendored artifacts.
Their generators and validation commands live in the repository.

[Provenance](provenance.md) records external source, algorithms,
revisions, licenses, local changes, and integrity evidence. Missing historical
information stays explicitly unresolved instead of receiving a guessed
attribution.
