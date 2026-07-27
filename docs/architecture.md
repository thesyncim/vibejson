# Architecture

vibejson is one JSON library with two implementation lanes: portable Go is the
reference, and a bounded Go-native SIMD experiment may replace selected kernels.
Both lanes expose the same root API and must produce the same bytes, values, and
errors.

## Package tour

The root `vibejson` package owns the public codec and document workflows:

- `Marshal`, `Unmarshal`, `Encoder`, and `Decoder` compile typed plans and cache
  convenience-call plans by Go type;
- `Valid`, `Validate`, and the compact, indent, and canonicalize families cover
  syntax checks and transforms;
- `Reader`, `Writer`, `DecodeNext`, and `EncodeTo` frame streams without making
  framing part of the typed codec;
- `GetRaw` and compiled JSON Pointers select borrowed slices for one-off access;
- `BuildIndex`, `Index`, and `Node` navigate a validated document using
  caller-provided structural storage; and
- `Parse` and `Value` provide an owning, ordered dynamic representation.

The `document` package contains shared index kinds, options, pointer parsing,
and errors. The `simd` package is a pre-v1 public surface for decimal, float,
time, and backend-reporting helpers. Most applications should start at the root
package.

## Shared x packages

The database moved to a separate module but still shares a few low-level pieces.
Those pieces live under `x/` so the module boundary is explicit:

| Package | Responsibility |
| --- | --- |
| `x/scanner` | JSON byte and string scanning with effective backend reporting |
| `x/kernels` | Stage 1 structural classification and Stage 2 grammar/index kernels |
| `x/byteview` | Read-only zero-copy byte and string views |
| `x/floatconv` | Decimal-to-binary float conversion |
| `x/jsonfields` | `encoding/json`-compatible struct-field resolution |

These packages are exported for vibejson and
[vibedb](https://github.com/thesyncim/vibedb), not as a second high-level API.
They are versioned with this module but carry no compatibility promise before
v1. Moving between root APIs and `x/` is an architectural choice, not a
performance toggle.

## Portable and SIMD lanes

Go 1.26 builds the portable implementation. The compiler pinned by
`scripts/bootstrap-gotip.sh` can build the experimental lane with
`GOEXPERIMENT=simd` on validated amd64 and arm64 targets. Build constraints keep
the SIMD source window bounded to the compiler family it was tested against;
other compilers and targets select portable fallbacks.

Acceleration stops at kernel boundaries. Codec plans, ownership rules, stream
states, and public error contracts do not change with the selected backend.
Every accelerated kernel therefore needs differential parity coverage against
its portable implementation. The required contributor lanes are listed in
[CONTRIBUTING.md](../CONTRIBUTING.md).

## Ownership boundaries

Typed decoding owns exposed strings by default. `Parse` is also owning.
`RawValue`, zero-copy typed decoding, reader cursors, byte views, and structural
indexes borrow their documented input or scratch storage. An index borrows both
the JSON source and its caller-provided entry slice.

Caller-buffered APIs may reuse capacity after warm-up, but allocation behavior
is an operation-level contract. Compilation, dynamic types, custom methods,
buffer growth, and new high-water marks are separate allocation boundaries.

## Unsafe philosophy

Unsafe code is allowed only where a normal Go implementation cannot maintain a
measured hot-path contract. Each use must have a portable behavioral reference
and explicit proofs for bounds, layout, lifetime, aliases, and GC visibility.
Go pointers remain visible as pointers: they are not hidden in integers, stored
in external memory, or derived from private runtime layouts.

[UNSAFE.md](../UNSAFE.md) is the generated exhaustive inventory of production
scopes and their required tests. [Provenance](provenance.md) separately records
externally derived source and algorithms; neither document is an implementation
journal.
