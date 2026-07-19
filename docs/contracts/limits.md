# Resource and input limits

This page records the limits that affect acceptance, memory growth, and
retention. A per-value or per-state limit is not a process-wide memory budget;
applications still need protocol-level limits for total input, work, and
concurrency.

## Configured input limits

### Nesting depth

Parsing, validation, selection, indexing, decoding, and transforms default to
10,000 nested arrays and objects. A positive `Options.MaxDepth`,
`DecoderOptions.MaxDepth`, or `IndexOptions.MaxDepth` replaces that default for
the APIs that accept the corresponding options. Values at or below zero select
the default. Convenience APIs without a depth option use the default.

`MaxDepth` counts container nesting. It does not limit bytes, string or number
length, members in one container, or members in the whole document.

Encoding follows the supported compiler's `encoding/json` behavior. Go 1.26
has no fixed encoder nesting limit; it begins identity-based cycle detection
after a recursive path crosses 1,000 pointer-, map-, or slice-bearing values.
Go 1.27 and later reject encoding beyond 10,000 nested containers and also
bound a chain of pointer hops. There is no encoder option that changes these
toolchain-specific guards.

### Streamed value size

`ReaderOptions.MaxValueBytes` and `Reader.SetMaxValueBytes` limit one top-level
value when positive. Zero, the default, means unbounded. The limit does not cap
the number of values, total stream bytes, elapsed work, or the Reader's initial
buffer capacity. A value over the limit stops iteration and is reported by
`Reader.Err`.

`NewReader` and a zero `ReaderOptions.BufferSize` use a 64 KiB initial buffer.
`NewReaderSize` and a positive `BufferSize` choose initial capacity only;
values below 512 bytes are rounded up to 512. Negative option values are
rejected. The rolling buffer grows when a complete value does not fit, so
callers handling untrusted input should set a positive `MaxValueBytes` and an
application-level total-stream policy.

One-shot byte-slice APIs do not impose a separate package-wide limit on input,
string, or number length. Available memory, Go's `int` and address space, and
the representation-specific index limits below still apply.

## Structural-index limits

`IndexEntry` is four `uint32` words (16 bytes). `BuildIndexOptions` rejects a
source length or caller-provided storage capacity greater than `math.MaxUint32`
with `ErrIndexTooLarge`; offsets and entry links must fit the same 32-bit
representation. Insufficient caller storage returns `ErrIndexFull`.

A container's direct member count occupies 26 bits, so one array or object can
hold at most 2^26 - 1 direct members in an `Index`. Exceeding that count returns
`ErrIndexTooLarge`. This is not a separate total-member limit across the whole
document. `RequiredIndexEntries` reports the exact caller storage needed at the
default depth limit.

The 64-level bitmap index machine is an internal fast-path limit, not an input
contract. Deeper accepted documents fall back to the general builder, which
enforces the caller's `MaxDepth`.

## Retained memory

### Reader, Writer, and returned values

- A `Reader` retains its rolling buffer, which can grow to accommodate the
  largest value encountered. `Reader.Close` releases the Reader's references to
  its input and buffer. Any slice already returned by `Bytes` remains retained
  by its caller.
- A `Writer` buffers one complete top-level value before flushing. Its size is
  a flush threshold, not a maximum value size. `Flush` and `Close` reuse the
  grown buffer; `Close` does not shrink it, release it, or close the underlying
  writer.
- Caller-owned output capacity, decoded destination storage, an owning
  `Value`'s source copy and index, and caller-provided `Index` storage live as
  long as their owners retain them. Pool budgets do not apply to these values.

### Pooled operation state

The following are retention budgets for one reusable state, not aggregate
process limits:

- Encoder map-entry storage and rendered map-key bytes each have an independent
  512 KiB retention budget. Each eligible typed-value backing slot also has a
  512 KiB budget. A compiled plan can own multiple fixed-type slots; oversized
  maps use one-call storage instead of replacing pooled scratch.
- A structural decoder state returned to the package pool retains at most
  2 MiB of `uint32` positions. Larger tapes are dropped on release.
- A compiled decoder caches at most four operation states. The cumulative
  shallow map key/value box budget is at most 64 KiB per state; ineligible or
  oversized boxes use operation-local storage.
- `Parse` seeds each pooled estimate buffer with 1,024 `IndexEntry` values
  (16 KiB). If a document needs a larger index, that grown index belongs to the
  returned `Value` rather than replacing the pooled estimate.

These states use `sync.Pool`. The runtime may discard pooled objects during
garbage collection, but the package does not place a deterministic aggregate
cap on the number of states retained across processors, compiled plans, or
concurrent calls.

### Compiled-plan caches and Marshal hints

The convenience `Marshal` and `Unmarshal` paths cache compiled plans by Go
type. Concrete types reached through interface values have additional encode
and decode plan caches; encode entries distinguish HTML-escaping mode. These
`sync.Map` caches have no entry-count limit or eviction policy and live for the
process lifetime. Programs that synthesize an unbounded number of `reflect`
types should avoid sending them through these paths. Explicitly compiled plans
live as long as their `Encoder`, `Decoder`, or `Codec` owner.

`Marshal` and `Codec.Marshal` retain an integer output-size estimate, not an
output buffer. Ordinary observations through 256 KiB update it immediately,
with a 64-byte minimum initial capacity. A first larger observation is stored
as an unconfirmed outlier and the next call starts with 512 bytes; a repeated
equal large observation confirms exact presizing, even above 256 KiB. A smaller
observation replaces the estimate. Therefore 256 KiB is an
outlier-confirmation threshold, not an output-size or retained-memory ceiling.

## Limits that applications must add

The package does not provide a process-wide memory budget, total-stream byte or
value count, deadline, concurrency cap, output-size cap, or type-cache
cardinality cap. Apply those limits at the protocol or service boundary. In
particular, `BufferSize`, a Writer size, `MaxDepth`, per-state scratch budgets,
and the 256 KiB Marshal threshold must not be treated as substitutes for a
total resource budget.
