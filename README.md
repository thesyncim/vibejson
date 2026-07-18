# simdjson

[![ci](https://github.com/thesyncim/simdjson/actions/workflows/ci.yml/badge.svg)](https://github.com/thesyncim/simdjson/actions/workflows/ci.yml)

This is an independent Go implementation, not the C++
[`simdjson`](https://github.com/simdjson/simdjson) project. The relationship to
upstream algorithms and test material is recorded in
[`docs/provenance.md`](docs/provenance.md).

Strict, high-performance JSON for Go, written entirely in Go. `Unmarshal` and
`Marshal` are drop-in replacements for their `encoding/json` counterparts;
compiled per-type codecs, structural indexes, and vector kernels built on Go's
experimental `simd/archsimd` package supply the speed. The root module has no
third-party dependencies, generated codecs, assembly, C, `go:linkname`, or
runtime map-layout assumptions.

> [!IMPORTANT]
> **Go 1.26 and the pinned Go 1.27 development compiler are supported.** Go
> 1.26 builds the tuned portable implementation. The revision pinned by
> `scripts/bootstrap-gotip.sh` builds either portable code or the Go-native
> SIMD kernels when `GOEXPERIMENT=simd` is set. Those experimental kernels are
> bounded to the Go 1.27 compiler family; later compiler releases use the
> portable implementation until they pass the same gates and are promoted.

The repository is governed by four priorities, in this order:

1. exact JSON behavior and compatibility;
2. memory-safe, GC-visible ownership in the default build;
3. the fastest path as the ordinary path, without safety or tuning knobs; and
4. small, composable APIs backed by unified implementations.

Performance changes must preserve the correctness and lifetime gates. Safety
fixes are measured immediately so avoidable cost can be recovered with typed,
runtime-managed ownership rather than hidden pointers or runtime-layout tricks.

[Install](#install) | [Quick start](#quick-start) | [Choose an API](#choose-an-api) | [Usage](#usage) | [Performance](#performance) | [Contracts](#compatibility-and-contracts) | [SIMD package](#simd-package) | [Reproduce](#reproduce-benchmarks)

## Install

Install the portable build with Go 1.26:

```sh
go get github.com/thesyncim/simdjson@latest
```

To build the SIMD implementation, first build the supported Go 1.27
development toolchain from a checkout of this repository:

```sh
./scripts/bootstrap-gotip.sh "$HOME/sdk/simdjson-gotip"
```

The moving `gotip` toolchain is convenient for trying a newer revision:

```sh
go install golang.org/dl/gotip@latest
gotip download
```

Then, from your module:

```sh
"$HOME/sdk/simdjson-gotip/bin/go" get github.com/thesyncim/simdjson@latest
```

Enable the SIMD kernels when building or testing:

```sh
GOEXPERIMENT=simd "$HOME/sdk/simdjson-gotip/bin/go" build ./...
```

Without `GOEXPERIMENT=simd`, simdjson keeps the same API and behavior while
using its portable Go implementations. Go 1.26 also selects those portable
files if the experiment name is set, so a stable build never imports the
unreleased `simd/archsimd` API. Ordinary builds on Go 1.28 and later likewise
select portable files until their compiler API, generated code, escape
behavior, and performance have been validated explicitly. The exact support
and compiler-update policy is documented in
[`docs/toolchain.md`](docs/toolchain.md).

`GOEXPERIMENT=simd` is a compiler switch, not a CPU-dispatch override. A SIMD
binary built by the pinned compiler snapshots runtime CPU features during
package initialization and keeps a safe portable implementation for unsupported
machines. The selected scanner calls do not repeat feature probes while
processing input.

## Quick start

```go
import "github.com/thesyncim/simdjson"

type Event struct {
	ID   int      `json:"id"`
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

var event Event
if err := simdjson.Unmarshal(data, &event); err != nil {
	return err
}

out, err := simdjson.Marshal(&event)
```

Both functions behave like `encoding/json` — same tags, same merge semantics,
same output bytes — and cache one compiled plan per type for the life of the
process. Everything below is opt-in.

## Choose an API

Choose by the lifetime and access pattern of the data, not by the name of an
individual benchmark.

| Need | API | Why |
| --- | --- | --- |
| Decode a known Go type occasionally | `Unmarshal` | Default compatible behavior with a cached plan. |
| Decode one Go type repeatedly | `Decoder` | Compile options once and reuse destinations safely. |
| Encode occasionally or repeatedly | `Marshal` or `Encoder` | `Encoder.AppendJSON` reuses caller-owned output. |
| Keep both directions together | `Codec` | One options value and one reusable encoder/decoder pair. |
| Read one JSON Pointer target | `GetRaw` or `ScanFirstRaw` | No persistent tree; the method name states last- versus first-duplicate behavior. |
| Traverse one document repeatedly or out of order | `Index` and `Node` | Validate and build caller-owned navigation storage once, then reuse it without allocating. |
| Retain an ordered, lazy dynamic document | `Parse` and `Value` | The result owns its storage by default and materializes only what is requested. |
| Produce ordinary dynamic Go values | `Unmarshal` into `any` | Builds the familiar `map[string]any` and `[]any` representation. |
| Decode a typed stream | `Reader` with `DecodeNext` | Frames and decodes each value in one forward pass. |
| Inspect a dynamic stream once | `Reader.Cursor` | Forward-only access without constructing an index per value. |

An `Index` makes later navigation fast, but constructing it still scans the
whole document and writes structural entries. That cost is worthwhile when the
same document is revisited or accessed randomly. A one-shot typed decode can
scan and decode in one pass, and a streaming cursor can discard each value as
soon as it is consumed.

`Index`, `Node`, `RawValue`, zero-copy `Value`, and cursor strings borrow their
input. Keep the source alive and unmodified for as long as those values are in
use. Default `Parse` and default typed decoding own the string storage they
expose.

## Usage

Expand the task you care about. Every snippet compiles against the current API.

<details>
<summary><b>Decode into structs, compiled once</b></summary>

Hot paths should compile the decoder once and reuse it; a `Decoder` is
immutable and safe for concurrent use. Options select strictness, ownership,
and merge-versus-replace semantics.

```go
decoder, err := simdjson.CompileDecoder[Event](simdjson.DecoderOptions{
	DisallowUnknownFields: true, // reject keys the struct does not declare
	CaseSensitive:         true, // skip the case-insensitive field fallback
})
if err != nil {
	return err
}

var event Event
if err := decoder.Decode(data, &event); err != nil {
	var decodeErr *simdjson.DecodeError
	if errors.As(err, &decodeErr) {
		fmt.Println(decodeErr.Path, decodeErr.Offset) // e.g. "items[3].id" 42
	}
	return err
}
```

`DecodeError` carries the byte offset and a typed path such as
`items[3].scores[1]`; the path is built only when an error unwinds, so
successful decodes pay nothing for it. `DecoderOptions.Replace` resets
destination state the document does not mention — the right mode for
destinations reused across decodes; the default merges like `encoding/json`.
`Decoder.DecodeArray` decodes a top-level array while reusing caller-provided
slice capacity, and `Decoder.DecodePrefix` decodes one value from the front of
a larger buffer.

</details>

<details>
<summary><b>Zero-copy decode</b></summary>

`ZeroCopy` aliases unescaped strings directly into the source buffer instead
of copying them.

```go
decoder, err := simdjson.CompileDecoder[Event](simdjson.DecoderOptions{
	ZeroCopy: true,
})
if err != nil {
	return err
}

var event Event
if err := decoder.Decode(src, &event); err != nil {
	return err
}
// event's strings alias src: keep src alive and unmodified
// for as long as event is in use.
```

Without `ZeroCopy`, results are independent of `src`: decoded strings alias at
most one private copy of the input, so retaining any decoded string retains
that copy. `Options.ZeroCopy` offers the same choice for the ordered `Parse`
tree.

</details>

<details>
<summary><b>Dynamic values: <code>any</code> trees and ordered trees</b></summary>

`Unmarshal` into a `*any` produces the standard Go JSON shapes through a
dedicated one-pass builder, with no intermediate tree; `Parse` produces an
ordered, on-demand `Value` when member order matters.
Parsing builds only the structural index — scalars decode and containers
materialize when they are actually read, so a document consumed in part never
pays for the whole. A `Value` keeps its backing storage alive, so it stays
usable after the source slice is gone.

```go
var value any
if err := simdjson.Unmarshal(src, &value); err != nil {
	return err
}
object := value.(map[string]any)
fmt.Println(object["scores"].([]any)[1]) // 2.5

// json.Number instead of float64:
decoder, err := simdjson.CompileDecoder[any](simdjson.DecoderOptions{UseNumber: true})
if err != nil {
	return err
}
if err := decoder.Decode(src, &value); err != nil {
	return err
}

// Ordered on-demand value that preserves member order:
root, err := simdjson.Parse(src)
if err != nil {
	return err
}
user, _ := root.Get("user")
name, _ := user.Get("name")
text, _ := name.Text()
fmt.Println(text) // ada
```

</details>

<details>
<summary><b>Stream NDJSON in both directions</b></summary>

`Writer` streams NDJSON or concatenated values through one reused buffer.
`Reader` iterates validated top-level values from any `io.Reader`;
`DecodeNext` fuses iteration and typed decoding into one pass — the decoder
itself finds the value boundary — and is the fastest way through a typed
stream. Steady-state streaming in both directions performs no per-value
allocations.

```go
codec, err := simdjson.CompileCodec[Event](simdjson.CodecOptions{})
if err != nil {
	return err
}

w := simdjson.NewWriter(out)
for i := range events {
	codec.EncodeTo(w, &events[i])
	w.Newline()
}
if err := w.Close(); err != nil {
	return err
}

r := simdjson.NewReader(in)
var event Event
for simdjson.DecodeNext(r, codec.Decoder(), &event) {
	// use event
}
return r.Err()
```

`CompileCodec` bundles both directions for one type plus a per-codec output
size hint. For hand-built documents, `Writer` exposes token methods that track
container state and refuse to emit malformed JSON:

```go
w := simdjson.NewWriter(out)
w.BeginObject()
w.Key("id")
w.Int(7)
w.Key("ok")
w.Bool(true)
w.EndObject()
return w.Close()
```

For dynamic single-pass consumption, `Reader.Cursor` walks the current value
forward without building any tree or index: scalars parse on demand through
the same kernels the compiled decoder uses, `Skip` hops unwanted subtrees
structurally, and a full stream costs two allocations total rather than
allocations per value. The contract is forward-only and explicit —
`BeginObject`/`NextField`, `BeginArray`/`NextElement`, `Skip`, `Finish`:

```go
r := simdjson.NewReader(in)
for r.Next() {
	c := r.Cursor()
	if err := c.BeginObject(); err != nil {
		return err
	}
	for {
		key, ok, err := c.NextField()
		if err != nil || !ok {
			break
		}
		if key == "id" {
			id, _ := c.Int64()
			// use id
		} else {
			c.Skip()
		}
	}
}
return r.Err()
```

`Reader.Bytes`, zero-copy decodes, and cursor strings alias the rolling
buffer and stay valid only until the next `Next`; `SetMaxValueBytes` bounds
buffer growth on untrusted input. Configure a reader before the first `Next`
or `DecodeNext`; later configuration changes are rejected. `Close` is
idempotent and terminal. Reader work stays on the caller's goroutine and does
not create background workers.

</details>

<details>
<summary><b>Encode into a reused buffer</b></summary>

`AppendJSON` appends to a caller-owned buffer, so steady-state encoding does
not allocate. An `Encoder` is immutable and safe for concurrent use.

```go
encoder, err := simdjson.CompileEncoder[Event](simdjson.EncoderOptions{})
if err != nil {
	return err
}

buf := make([]byte, 0, 4096)
for i := range events {
	buf, err = encoder.AppendJSON(buf[:0], &events[i])
	if err != nil {
		return err
	}
	fmt.Println(string(buf)) // use buf before the next iteration reuses it
}
```

Output matches `encoding/json` byte for byte: compact, HTML-escaped (opt out
with `DisableHTMLEscaping`), U+2028/U+2029 escaped, invalid UTF-8 replaced.
`omitempty` and `string` tag options, `json.Marshaler`, `encoding.TextMarshaler`,
and `time.Time` are supported. Unrepresentable values (NaN, infinities,
malformed `json.Number`) return an `EncodeError` with a typed path.

</details>

<details>
<summary><b>Custom marshal/unmarshal hooks</b></summary>

A type can decode from the package cursor or append through the package writer
by implementing `UnmarshalSimdJSON(DecodeCursor) (DecodeCursor, error)` or
`MarshalSimdJSON(TrustedAppender) TrustedAppender`. The hooks avoid reparsing
raw custom marshaler output and are useful for generated or carefully
hand-written hot types.

```go
func (e Event) MarshalSimdJSON(w simdjson.TrustedAppender) simdjson.TrustedAppender {
	w = w.RawByteUnchecked('{').RawUnchecked(`"id":`).Int(int64(e.ID))
	w = w.RawUnchecked(`,"name":`).String(e.Name)
	return w.RawUnchecked(`,"active":`).Bool(e.Active).RawByteUnchecked('}')
}
```

Safety is not a build option. Every build constructs hook interfaces through
the Go runtime. DecodeCursor and TrustedAppender are both transferred by value:
the hook returns the advanced state, so no pointer into a codec stack frame is
exposed and no cursor box is allocated. Addressable decode and encode values
expose their real GC-visible receiver. Reused destinations, struct fields, and
slice elements therefore add no receiver allocation; a fresh stack-local root
may escape once because an arbitrary pointer-receiver method is legally allowed
to retain `*T`. That is one ordinary receiver-lifetime escape, not one
allocation per hook or element.
There is no itab rebinding, layout probe, unsafe fast mode, or alternate
dispatch build.
Hooks must still consume or emit exactly one valid JSON value, and callers must
not retain a `TrustedAppender` across reuse of its caller-owned output buffer.
Raw fragments use deliberately explicit `RawUnchecked`, `RawBytesUnchecked`,
and `RawByteUnchecked` names. Tests and debug builds can use the
`simdjson_validate_hooks` build tag to validate exactly the span emitted by
each encode hook; production builds compile that validation away.

</details>

<details>
<summary><b>Capture unknown members with an <code>,inline</code> catch-all</b></summary>

An opt-in extension routes object members that match no declared field into a
`map[string]T` tagged `json:",inline"`, and re-emits them at the object's own
level. The tag is inert unless you enable it, so types that do not use it
compile to the identical plan and pay nothing.

```go
type Event struct {
	ID    int                        `json:"id"`
	Extra map[string]json.RawMessage `json:",inline"` // unknown members land here
}

decoder, _ := simdjson.CompileDecoder[Event](simdjson.DecoderOptions{InlineFields: true})
encoder, _ := simdjson.CompileEncoder[Event](simdjson.EncoderOptions{InlineFields: true})
```

The catch-all consumes members that `DisallowUnknownFields` would otherwise
reject. On encode the map's members follow the declared fields, sorted by name
for deterministic output (`EncoderOptions.UnsortedInlineFields` emits them in
map order instead); a populated catch-all encodes without allocating once the
encoder's pooled scratch has warmed. With `DecoderOptions.Replace`, a reused
destination's map is cleared before decoding, like any other field; the default
merges.

</details>

<details>
<summary><b>Validate without decoding</b></summary>

`Valid` and `Validate` check strict JSON syntax and full UTF-8 validity
without building any representation.

```go
fmt.Println(simdjson.Valid([]byte(`{"strict":true}`))) // true

err := simdjson.Validate([]byte(`{"trailing":1,}`))
fmt.Println(err)
// json syntax error at byte 14, line 1, column 15: expected object key string
```

`ValidNumber`, `ValidString`, and their `Validate` forms check single JSON
number and string literals.

</details>

<details>
<summary><b>Extract one value with a JSON Pointer</b></summary>

`GetRaw` resolves an RFC 6901 pointer to a raw source slice while validating
the whole document. `ScanFirstRaw` validates only up to and including the target
and stops — the fast choice for plucking one field from a large document.
The names also encode duplicate-key policy: `GetRaw` returns the last matching
member like `encoding/json`, while `ScanFirstRaw` returns the first match it can
stop on.

```go
price, ok, err := simdjson.GetRaw(src, "/items/0/price")
if err != nil || !ok {
	return err
}
v, _ := price.Float64()
fmt.Println(v) // 9.99

// ScanFirstRaw stops as soon as the target has been validated; compile
// the pointer once on hot paths.
pointer := simdjson.MustCompilePointer("/items/0/sku")
sku, ok, err := pointer.ScanFirstRaw(src)
if err != nil || !ok {
	return err
}
text, _, _ := sku.Text()
fmt.Println(text) // a-1
```

For repeated traversal of one document, `BuildIndex` validates the input once
and lays out a navigable structural index in caller-provided storage, which
`Node` and the iterators walk without allocating (`RequiredIndexEntries`
reports the exact storage needed):

```go
var storage [16]simdjson.IndexEntry
index, err := simdjson.BuildIndex(src, storage[:])
if err != nil {
	return err
}

items, ok, err := index.Pointer("/items")
if err != nil || !ok {
	return err
}
iter, _ := items.ArrayIter()
for item, ok := iter.Next(); ok; item, ok = iter.Next() {
	sku, _ := item.Get("sku")
	raw, _ := sku.StringBytes() // aliases src; nothing allocates
	fmt.Println(string(raw))
}
```

</details>

<details>
<summary><b>Transform documents: compact, indent, canonicalize</b></summary>

All three validate their input and append to caller-owned buffers;
`Compact`, `Indent`, and `Canonicalize` are the allocating conveniences.

```go
compact, err := simdjson.AppendCompact(nil, src)
if err != nil {
	return err
}
fmt.Println(string(compact)) // {"b":1,"a":[1,2]}

canonical, err := simdjson.AppendCanonicalize(nil, src)
if err != nil {
	return err
}
fmt.Println(string(canonical)) // members sorted: {"a":[1,2],"b":1}

pretty, err := simdjson.AppendIndent(nil, src, "", "  ")
if err != nil {
	return err
}
fmt.Println(string(pretty)) // reindented, like json.Indent
```

</details>

<details>
<summary><b>Use the SIMD kernels directly</b></summary>

`github.com/thesyncim/simdjson/simd` is independently importable and adds no
module dependencies. The fixed-array digit API makes load and store widths
explicit:

```go
import "github.com/thesyncim/simdjson/simd"

if len(src) >= 16 {
	digits := (*[16]byte)(src)
	if simd.All16Digits(digits) {
		value := simd.Parse16Digits(digits)
		_ = value
	}
}

info := simd.Current()
_ = info.Enabled // true when an accelerated backend is compiled and selected
```

See the [SIMD package](#simd-package) section for the full kernel inventory
and runtime dispatch.

</details>

## Performance

<!-- benchpublish:main-summary:start -->
The current publication is measured from clean library revision
`b05b7ce145bb9a3c53301beb2619241180c786ce`. Measurements use an Apple M4 Max and one CPU. Each row reports the median of
six approximately 300 ms samples; the pinned Go revision is `03845e30f7b73d1703bd8c21017297f6eecb76d6`. Each contract runs in a fresh process so
allocator-heavy dynamic decode cannot perturb later groups. Lower time is
better; speedups are geometric means across the seven exact 6.33 MiB Go
`encoding/json` corpus payloads.

| Operation | Contract | vs stdlib | vs fastest rival | vs native Sonic | SIMD vs pure Go |
|---|---|---:|---:|---:|---:|
| Validate | Strict JSON + UTF-8 | **3.09x** | **2.79x** | **1.37x** | **1.804x** |
| Typed owned decode | Owned strings | **4.17x** | **1.81x** | **1.69x** | **1.124x** |
| Dynamic owned decode | Owned `any` tree | **3.65x** | **1.88x** | **1.14x** | **1.068x** |
| Owned encode | Owned output | **2.55x** | **1.46x** | **2.61x** | **1.318x** |
| Compiled encode reuse | Reused output buffer | **4.63x** | **2.65x** | — | **1.497x** |
| Parse + complete walk | Complete semantic traversal | **6.29x** | — | — | **1.230x** |

![Absolute time for one complete corpus pass](benchmarks/charts/headline.svg)

The chart shows absolute time to process all seven payloads once. It sums the
per-file medians without converting them to ratios; lower bars are faster. The
table keeps geometric means so small and large payloads receive equal weight
in the aggregate comparison.

The fastest-rival column chooses the best compatible result per payload from
go-json, Segment, jsoniter, and fastjson, all built with the pinned Go tip.
Native Sonic uses its stable supported toolchain in an isolated module; its
syntax-only `Valid` result is context rather than a strict-validation peer.

The same corpus puts `encoding/json/v2` behind by 3.35x on typed decode, 2.03x
on dynamic decode, and 2.39x on owned encode. Reusable structural-index
construction is part of the regular benchmark gate and remains zero-allocation.

[Current per-corpus results, allocations, hook cost, SIMD uplift, charts, and exact commands](benchmarks/README.md).
The [cross-language benchmark](benchmarks/crosslang/README.md) publishes only
the enforced parse-plus-semantic-digest contract as a direct comparison.
<!-- benchpublish:main-summary:end -->

## Compatibility and contracts

**Strictness.** Parsing, decoding, validation, and transforms enforce RFC
8259 syntax, full UTF-8 validity, and correct `\uXXXX` escapes including
surrogate pairing, and reject trailing data after the top-level value
(`ScanFirstRaw` deliberately stops validating once its target is found; `Reader`
frames multiple top-level values by design). Where `encoding/json` silently
replaces invalid UTF-8 during decode, simdjson rejects the document with a
positioned error. Depth is limited (default 10000, configurable through the
`MaxDepth` options).

**encoding/json parity.** Encoded output matches `encoding/json` byte for
byte, with one deliberate exception: a custom `json.Marshaler` or
`json.RawMessage` whose bytes contain a lone `\uXXXX` surrogate or invalid
UTF-8 is rejected rather than passed through. simdjson rejects those same
bytes on decode, so accepting them here would emit JSON it could not read
back; the strict-UTF-8 guarantee is symmetric across encode and decode.
Decoding follows stdlib semantics: struct tags, case-insensitive field
fallback (disable with `CaseSensitive`), merge-into-existing destinations
(switch with `Replace`), `json.Unmarshaler`/`json.Marshaler`,
`encoding.TextUnmarshaler`/`encoding.TextMarshaler`, and `time.Time`. Typed
and dynamic differential tests run against `encoding/json`.

**Ownership.** Default decodes return owned results that are independent of
the source. `ZeroCopy` decodes alias the source: keep it alive and unmodified
while results are in use. A `Value` from `Parse` owns a private copy of the
source and its structural index (with `Options.ZeroCopy` it aliases the
caller's bytes instead), and keeps both alive for as long as any `Value`
derived from it is reachable. `RawValue`, `Index`, and `Node` always alias the
source; `Index` and `Node` also alias the caller-provided `IndexEntry`
workspace. `AppendJSON` requires destination storage disjoint from storage
reachable through the source value; on error it returns the original
destination length, but unused capacity may contain partial output.

**Concurrency.** Compiled `Decoder`, `Encoder`, `Codec`, and
`CompiledPointer` values are immutable and safe for concurrent use, as are
the package-level functions backed by cached plans. A `Reader` or `Writer`
belongs to one goroutine.

## SIMD package

`github.com/thesyncim/simdjson/simd` owns every architecture-specific kernel,
runtime feature probe, and dispatch decision. It provides fixed-width decimal
parsing and formatting, `encoding/json`-compatible float formatting, quoted
RFC3339Nano time formatting, JSON and HTML-safe string scans with fused
prefix copies, strict UTF-8 / U+2028-U+2029 / contiguous `\uXXXX` kernels,
safe public scanners plus an explicit precondition-based `simd.Unchecked`
surface, and `Current`, which reports selected backends, vector widths, and
CPU features.

All APIs have portable fallbacks; every vector load is length-guarded.
Runtime capabilities are read once and implementation choices are fixed
during package initialization. On amd64 the hot scanner uses static calls
behind a process-constant level so stack-backed inputs remain allocation-free.
AVX-512 scanner kernels are retained for differential testing and direct
benchmarking, but are not selected automatically until they demonstrate a win
across representative CPU families and short/long input distributions.

| Runtime | String scanning | Decimal parse | Decimal and time format |
|---|---|---|---|
| arm64 | NEON on sustained runs; overlap-vector tails | SWAR reduction; NEON fused validation and reduction | NEON 16-digit and RFC3339 formatting |
| amd64 with AVX2 (including AVX-512 CPUs) | 32-byte AVX2 | Scalar SWAR | Scalar SWAR |
| Other build or CPU | Scalar Go | Scalar Go | Scalar SWAR |

`Current().ParseBackend` and `ParseVectorBytes` describe the unchecked
`Parse16Digits` route. On ARM64, `Parse16DigitsChecked` separately fuses NEON
validation with reduction because that measured faster than checking and then
calling the SWAR reducer. The unchecked NEON reduction measured slower than
SWAR on the Apple M4 Max benchmark runner and is not selected.

ARM64 NEON is the only alternate scanner ISA implemented today. PMULL may be
reported by `Current().Features`, but it does not select a different scanner;
DotProd, SVE, and SVE2 kernels require separate implementations and measured
dispatch policy before they can be advertised as active backends.

Pull requests run matched pure-Go and SIMD correctness, scanner/kernel, and
end-to-end benchmarks on native amd64 and native ARM64 runners. Raw artifacts
are named per architecture so results cannot overwrite each other.

Kernel microbenchmarks live in the root and comparison modules; release claims
use the end-to-end corpus contracts in the [benchmark record](benchmarks/README.md).

## Correctness and safety

Unsafe code is restricted to measured internal paths: complete guarded blocks
around vector loads and stores, clamped public scanners with a documented
`simd.Unchecked` precondition surface, size-proven float and integer stores,
and typed offsets taken from public `reflect` metadata. Source-backed APIs
require immutable input. Standard decode hooks use GC-safe heap-backed shadows;
native decode hooks transfer cursor state by value and expose real addressable
receivers through ordinary runtime-built interfaces. Native and standard encode
hooks use the same ordinary receiver ownership, so the GC
tracks retained pointers exactly as it does for a direct Go method call;
non-addressable value receivers get a value copy. Pooled map storage is cleared
through typed reflection so pointer-bearing elements retain the GC's write
barriers; static and dynamic scratch ownership use separate, bounded slot
spaces. Dynamic interfaces and hook interfaces are constructed only by the Go
runtime, with no build tag or alternate unsafe dispatch.

The test suite covers all 318 JSONTestSuite parsing cases, the seven pinned
Go tip payloads with exact concrete models, typed and dynamic differentials
against `encoding/json`, 500,000 randomized float spellings, 700,000
randomized or boundary timestamps, randomized scalar/SIMD differentials
across lengths and alignments, and 33 automatically discovered fuzz targets
covering validation, transforms, typed decode, encode, Reader lifecycle,
resource spikes, hooks, numbers, and the SIMD kernels. Pull-request CI shards
every target; scheduled CI runs longer pure-Go and SIMD campaigns and uploads
crash corpora.

Correctness fixes that touch a hot path require before-and-after benchmarks.
A regression is optimized back before merge; correctness is never traded away
to recover it.

The [safety-first practical priority order](SAFETY_ROADMAP.md) records the
release freeze, lifetime gates, benchmark contract, and explicitly excluded
parallel work.

<details>
<summary><b>Local release gate</b></summary>

The gate tests stable portable behavior and the pinned compiler's portable and
SIMD builds:

```sh
./scripts/bootstrap-gotip.sh "$HOME/sdk/simdjson-gotip"
TIP_GO="$HOME/sdk/simdjson-gotip/bin/go"

GOTOOLCHAIN=local go test ./...
GOEXPERIMENT=simd GOTOOLCHAIN=local go test ./...
"$TIP_GO" test ./...
GOEXPERIMENT=simd "$TIP_GO" test ./...
GOEXPERIMENT=simd "$TIP_GO" test \
  -tags='go1.28 simdjson_future_compiler_test' ./...
GOEXPERIMENT=simd GOFLAGS=-tags=simdjson_validate_hooks "$TIP_GO" test ./...
"$TIP_GO" vet ./...
"$TIP_GO" test -race \
  -skip 'Alloc|ZeroCost|StaysOnStack|TestParseFloat64' ./...
GOEXPERIMENT=simd "$TIP_GO" test -race \
  -skip 'Alloc|ZeroCost|StaysOnStack|TestParseFloat64' ./...
GOEXPERIMENT=simd "$TIP_GO" test -gcflags='all=-d=checkptr=2' \
  -skip 'Alloc|ZeroCost|StaysOnStack|TestParseFloat64' ./...
GOGC=1 GOEXPERIMENT=simd "$TIP_GO" test \
  -run '^Test(EncoderScratchPoolPoisoning|DynamicMapScratchIsPlanIndependent|EncodeHookArrayUsesStableSourcePointers|HookReceiverLifetimes|HookRetentionTrapAfterPanic)$' \
  -count=10 -cpu=1,4,8 .
GOEXPERIMENT=simd GOFLAGS=-tags=simdjson_validate_hooks \
  ./scripts/fuzz-smoke.sh "$TIP_GO" 1000x
./scripts/check-stdlib-corpus.sh "$TIP_GO"
GOTIP="$TIP_GO" ./scripts/bench-gate.sh -b HEAD~1
```

The full vet suite, including `unsafeptr`, runs without exceptions.

</details>

## Reproduce benchmarks

Comparison libraries live in nested modules and never enter the root module.
The complete suite builds the pinned Go tip compiler, verifies the copied
stdlib corpus, and runs pure Go, SIMD, jsonv2, and native Sonic controls:

```sh
./scripts/bootstrap-gotip.sh "$HOME/sdk/simdjson-gotip"
TIP_GO="$HOME/sdk/simdjson-gotip/bin/go" ./benchmarks/run-comparison.sh
```

[Benchmark methodology and individual commands](benchmarks/README.md)

## Status

simdjson is pre-release. The API may change until the first tagged release.
