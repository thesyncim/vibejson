# Architecture and safety

This document records the boundaries and invariants that should remain stable
while the pre-v1 API evolves. Exact public limits live in
[`contracts/limits.md`](contracts/limits.md), compiler policy in
[`toolchain.md`](toolchain.md), test coverage in
[`TEST_CONTRACTS.md`](../TEST_CONTRACTS.md), and every production `unsafe`
scope in [`UNSAFE.md`](../UNSAFE.md).

## Package boundaries

The root `simdjson` package owns public JSON semantics, typed plans, streams,
indexes, ownership, and route selection. Coupled hot paths stay there when
extraction would block inlining or duplicate representations.

`internal/kernels` is the leaf structural pipeline. It accepts typed Go
buffers, retains no pointers, and provides architecture-specific Stage 1
classifiers and Go-native Stage 2 machines behind direct calls from the root
package. Portable fallbacks keep the same contracts.

`internal/scanner` is the leaf string and UTF-8 scanning pipeline. The root
package calls its precondition-based entry points directly; no public wrapper
mirrors those implementation entry points. Architecture dispatch, vector loads,
overlap checks, and scanner metadata remain inside this package.

`internal/floatconv` isolates non-inlinable Eisel-Lemire conversion and its
generated table behind one typed call; root retains grammar and fallback policy.

The pre-v1 `simd` package retains decimal classification, eight-digit parsing,
fixed-width decimal formatting, JSON float and time formatting, plus effective
backend reporting. CPU capability checks and selection policy remain internal.
CPU and compiler selection belongs at build or package initialization
boundaries, never in per-byte loops.

`internal/cmd` contains repository tooling, not runtime code. Comparison and
stdlib-corpus dependencies stay in nested modules, keeping the root module free
of third-party dependencies.

Create packages only for cohesive responsibilities with stable typed boundaries
and no reverse dependency on the root package. Do not extract hot paths when
doing so adds conversions, blocks inlining, causes escapes, or only renames them.

## Ownership and lifetimes

Every live Go pointer remains visible to the garbage collector. The
implementation does not use `uintptr` storage, private runtime layouts,
noescape declarations, or interface-header rewriting. `unsafe` is limited to
checked addressing, fixed-width loads and stores, and zero-copy views whose
bounds and lifetime are already established by typed state.

| Operation | Storage relationship | Invalidation rule |
| --- | --- | --- |
| Default typed decode | Unescaped strings may share one private input copy; containers belong to the destination. | Independent of later caller-input mutation. |
| Typed decode with `ZeroCopy` | Unescaped strings may alias caller input; escaped strings use owned storage. | Keep input alive and immutable while results are used. |
| Default `Parse` | `Value` owns the source and structural entries it needs. | Remains valid after the input variable is released. |
| `Parse` with `ZeroCopy` | Tree values may alias caller input. | Keep input alive and immutable while the tree is used. |
| `Index`, Index-derived `Node`, and `RawValue` | Borrow validated source and, for an index, caller-provided entry storage. | Keep both buffers alive and immutable until all handles are discarded. A `Node` obtained from an owning `Value` instead pins that value's backing arrays. |
| Reader views and cursors | Borrow the reader's rolling buffer. | Invalid after the next advancing operation or `Close`. |
| Encoder and writer output | Returned bytes belong to the caller. | Source graphs must not overlap output capacity being appended to. |

Borrowing avoids a copy; it never hides a pointer. Compiled plans contain no
source or destination pointer and are immutable after construction. Mutable
cursor, arena, error, and scratch state belongs to one call.

Destination addressing uses offsets, sizes, and pointer hops supplied by
`reflect.Type`. Pointers are allocated and slice bounds established before an
element address is formed. Default decoding merges like `encoding/json`;
replace mode deliberately resets absent state.

## Typed plans and specialization

Typed encoding and decoding have three layers:

1. a generic correct implementation for every supported shape;
2. an immutable reflect-compiled graph of operations; and
3. isolated executors for shapes with an end-to-end measured benefit.

`typedShape` holds immutable type structure; `typedDecodeProgram` and
`typedEncodeProgram` hold direction-specific execution metadata. `typedOp` is
their shared compiler/executor vocabulary. Its ordering is load-bearing where
range checks classify operations. Repetitive switches are generated from one
declarative table; handwritten branches are reserved for genuinely different
semantics or measured fused shapes. `go generate ./...` must reproduce
committed output exactly.

A specialization requires a named end-to-end benchmark, a forced-route
differential against the generic executor, malformed and partial-failure
coverage, and unchanged memory behavior. The default retention threshold is a
repeatable 3% `sec/op` improvement on its target workload with no `B/op` or
`allocs/op` increase. A miss returns to the generic plan.

The retained decode-shape manifest is deliberately small:

- `typedDecShapeRecord` fuses the common five-field record prefix and is forced
  by `BenchmarkTypedDecodeStructuralRecordSpecializations/record`.
- `typedDecShapeRecordFloat64x3` extends that record with a fixed three-float
  tail and is forced by the same benchmark's `float64x3` case.

`TestTypedDecodeForcedRouteParity` compares the specialized, record-fallback,
generic structural, cursor, automatic, zero-copy, and standard-library routes.
The malformed fixtures in that test prove transactional fallback. No other
production decode-shape specialization is retained.

Concrete types seen through interfaces are compiled on first use and cached by
the keys needed for their semantics. These process-lifetime caches are intended
for the finite type sets of ordinary programs, not unbounded synthesized types.

## Structural routes

The raw typed cursor validates and decodes in one pass. The structural typed
route performs stage 1 once and advances a transient forward-only position
tape. `Index` builds persistent caller-owned navigation entries for repeated or
out-of-order access. One-shot typed decode does not build an index because the
extra whole-document storage is often unnecessary.

Structural decoding is selected only for eligible plans and documents that meet
the size bounds in `decoderStructuralWorthwhile`: currently at least 4 KiB and
small enough for `uint32` tape positions. Smaller documents stay on the cursor
route. These thresholds are tuning constants, not API contracts; source
comments and route fixtures are their current record.

The transient tape contains delimiter and string positions but omits colons.
Executors validate any colon gap they cannot prove from a packed expected
field. Escaped or shuffled keys fall back to the raw key parser and realign the
tape. Unsupported or malformed structure fails closed or returns to the
generic cursor; it never relaxes JSON grammar.

Validation follows the same policy: portable stage 1 exists on every build and
supported SIMD builds replace only the kernel. Sampling may choose a packed
position consumer, but scalar/SIMD and route differentials must agree exactly.
Threshold changes require whole-document interleaved measurements and a clear
margin in maintained route fixtures.

## Hooks and methods

Standard `json.Marshaler`, `json.Unmarshaler`, `encoding.TextMarshaler`, and
`encoding.TextUnmarshaler` semantics are preserved. Native hooks are for
generated or deliberately custom codecs; ordinary structs should use compiled
encoders and decoders.

Field lookup uses exact spelling first, then unambiguous ASCII folding, then an
ordered `strings.EqualFold` fallback. This preserves declaration precedence and
never displaces an exact match.

Trusted appenders accept already-valid JSON and intentionally skip validation.
Malformed hook output can corrupt the surrounding document, though it cannot
violate memory safety. The `simdjson_validate_hooks` tag validates only emitted
hook spans in tests and fuzzing.

Encode methods receive ordinary GC-visible values. Non-addressable values use
typed addressable storage when compatibility requires it. Decode hooks receive
`DecodeCursor` by value and return the advanced value. A retained cursor owns
ordinary Go state and cannot alias a decoder stack frame; only the returned
copy advances the enclosing decode. User code may retain receivers, so normal
escape behavior is part of the safety contract.

## Pools and retained resources

Pools must have fixed cleanup rules and retained-capacity bounds. An exceptional
input must not permanently price later small operations.

- Encoder scratch has a plan-fixed layout. Pointer-bearing slots retain their
  concrete type and are never reinterpreted. Map entries, rendered keys, and
  typed backing each have a 512 KiB retention budget.
- Structural tapes retain at most 2 MiB of `uint32` positions. Larger storage is
  dropped. Escaped-string arenas detach before pooling because results may own
  their backing.
- `Marshal` caches only a size estimate, never output storage. Observations up
  to 256 KiB update it directly; a larger value must repeat before exact
  presizing is trusted. Long-lived callers should reuse `Encoder.AppendJSON`
  output capacity.

Cleanup clears populated pointer-bearing prefixes, resets iterators, and drops
oversized backing. Every new pool needs a documented bound, forced-GC retention
coverage, a huge-then-small benchmark, and error-path cleanup tests.

## Unsafe boundary

Prefer small typed helpers whose preconditions state the required byte count,
alignment or layout, ownership, and retention behavior. Raw pointer arithmetic
should remain local to those helpers or a measured loop that cannot express the
same work safely without regression.

Do not hide pointers from escape analysis, manufacture slice headers, store Go
pointers as integers, depend on private runtime layout, or use unsafe as a
package-boundary adapter. A wrapper is not an improvement if it adds a function
value call, prevents inlining, or moves stack-backed buffers to the heap.

The unsafe inventory is generated and checked in CI. Changes also require the
ordinary reference path, bounds/layout proof, lifetime tests, race and
`checkptr` coverage, escape/allocation checks, and relevant end-to-end
benchmarks.

## Change policy

Correctness and ownership precede throughput. Runtime changes must preserve
grammar, error behavior, lifetimes, retained memory, `B/op`, and `allocs/op`.
Measure with the same compiler, CPU, inputs, and process contract as the merge
base. Synthetic kernels can diagnose code generation but do not justify an
end-to-end specialization by themselves.

Release candidates keep aggressive-GC, stack-growth, receiver-retention,
pool-poisoning, route differential, fuzz, race, and `checkptr` coverage. New
optimization work should target measured bottlenecks rather than broadening
from an isolated microbenchmark.
