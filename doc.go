// Package vibejson implements strict, allocation-conscious JSON processing:
// compiled typed decoding, validation, caller-backed structural indexes,
// JSON Pointer selectors, dynamic decoding, and transforms.
//
// # Choosing an API
//
// For JSON with a known Go type, use [Unmarshal] for occasional calls and
// compile a [Decoder] for repeated calls. Use [Marshal] and [Encoder] under the
// same rule in the other direction.
//
// For dynamic JSON, choose by access pattern. [GetRaw] reads one RFC 6901
// target. [BuildIndex] performs a full validation and builds caller-owned
// navigation storage; use its [Index] and [Node] handles when one document will
// be traversed repeatedly or out of order. [Parse] returns an owning [Value]
// handle when the parsed document must outlive the input or when ordered,
// lazy navigation is more convenient than managing index storage. Unmarshaling
// into any produces the usual map and slice tree when interoperability matters
// more than preserving order or avoiding materialization.
//
// For a stream, use [DecodeNext] when the destination type is known and
// [ValueCursor] for one forward dynamic pass. Both avoid building a persistent
// index for each streamed value.
//
// # Decoding into structs
//
// [Unmarshal] behaves like encoding/json.Unmarshal and caches one compiled
// decoder per destination type:
//
//	var event Event
//	err := vibejson.Unmarshal(data, &event)
//
// Hot paths should compile the decoder once with [CompileDecoder] and reuse
// it; the returned [Decoder] is immutable and safe for concurrent use.
// [DecoderOptions] selects maximum depth, string ownership (ZeroCopy),
// unknown-field handling, case sensitivity, dynamic number representation,
// merge-versus-replace semantics, and the opt-in inline catch-all extension.
// The default merges like encoding/json. When decoding fails against the Go
// type, the returned [DecodeError] reports the byte offset and the path of the
// offending value, such as "items[3].scores[1]".
//
// # Encoding structs
//
// [Marshal] produces encoding/json.Marshal-compatible output for supported
// values and caches one compiled encoder per source type:
//
//	data, err := vibejson.Marshal(&event)
//
// Hot paths should compile with [CompileEncoder] and reuse the [Encoder];
// its AppendJSON method appends to a caller-owned buffer, so ordinary compiled
// fields can avoid output allocation once that buffer has enough capacity.
// Dynamic values and custom methods may allocate independently. Unrepresentable
// values (NaN, infinities, malformed json.Number) return an [EncodeError]
// carrying the same style of value path as [DecodeError].
//
// Concrete types encountered inside interface values are compiled on first
// use and cached for the process lifetime; encode entries are distinct for the
// HTML-escaping modes. This suits the finite type sets used by ordinary
// programs. Programs that synthesize an unbounded number of reflect types
// should not feed them through interface-valued codec paths.
//
// # Custom marshalers
//
// Types implementing json.Marshaler, json.Unmarshaler,
// encoding.TextMarshaler, or encoding.TextUnmarshaler — including time.Time —
// are dispatched through those interfaces like encoding/json. Ordinary plans
// remain stack eligible. Standard decode methods use a heap-backed receiver
// shadow that is copied back before return. The vibejson-native
// [UnmarshalerSimd] instead follows the same by-value state model as
// [MarshalerSimd]: it receives and returns an owned [DecodeCursor], while its
// addressable receiver uses ordinary Go pointer identity. Native hooks add no
// per-call cursor or receiver shadow allocation for reused destinations.
//
// # Validation and selection
//
// [Valid] and [Validate] check strict JSON syntax without building any
// representation. [GetRaw] resolves RFC 6901 JSON Pointers with last-duplicate
// semantics after validating the document; [ScanFirstRaw] names its
// early-exit, first-duplicate contract explicitly, and [ScanFirstRawTrusted]
// is its non-validating spelling for inputs already known to be valid.
// [CompilePointer] avoids reparsing the pointer on hot paths. [BuildIndex]
// validates the input once and lays out a navigable structural index in
// caller-provided storage, which [Node] and the iterators traverse without
// allocating.
//
// # Dynamic values and transforms
//
// [Parse] produces an owning, ordered [Value] backed by a compact structural
// index. [Unmarshal] into a *any produces ordinary maps, slices, strings,
// float64 values, booleans, and nil through a dedicated one-pass builder;
// [DecoderOptions].UseNumber selects json.Number for dynamic numbers.
// [AppendCompact], [AppendIndent], and [AppendCanonicalize] rewrite
// documents into caller-owned buffers.
//
// # Streaming
//
// [Writer] streams NDJSON or concatenated top-level values through one
// reused buffer, from compiled encoders via [EncodeTo] or through token
// methods that track container state so malformed output is impossible.
// [Reader] iterates validated top-level values from an io.Reader with a
// rolling buffer; a zero-copy [DecodeFrom] result aliases the buffer only
// until the next [Reader.Next] or [DecodeNext], and [DecodeNext] fuses
// iteration and typed decoding into a single pass.
// [NewReader] imposes no per-value size bound. A zero MaxValueBytes in
// [ReaderOptions] is likewise unbounded. Callers reading untrusted or network
// input should use [NewReaderWithOptions] with a positive MaxValueBytes chosen
// for the protocol.
//
// # The document index
//
// [BuildIndex] validates a document once and lays out a structural index — a
// flat tape of 16-byte entries in caller-owned storage, sized exactly by
// [RequiredIndexEntries] — that [Node] then navigates without allocating or
// rescanning text. Lookups resolve like [GetRaw]: the last member in
// document order whose key decodes to the query. Setting HashKeys in
// document.IndexOptions enriches the tape with a per-key content hash, so
// object lookups reject nearly every non-matching member on one word
// compare; flat objects additionally take an unrolled scan over the tape
// itself.
//
// Around that core sit the repeated-lookup tools. [CompileKey] and
// [CompilePointer] move query hashing and pointer parsing to compile time
// for queries reused across calls or documents. [Node.Fields] returns a
// [FieldCursor] that resumes forward scans when several known fields are
// read in roughly document order. [BuildObjectProbe] builds an [ObjectProbe]
// — a constant-time member table over one wide object queried many times.
// [Node.Contains] evaluates PostgreSQL-compatible jsonb containment over
// two indexed documents, with [RawContains] as the one-shot spelling.
//
// Batch document storage, shape-compiled field extraction, keyed mutable
// collections, durable persistence, queries, and SQL live in
// github.com/thesyncim/vibedb, which builds on this package and its x/
// subpackages.
//
// # Pre-v1 API boundary
//
// Document-model APIs, native hook interfaces, and the public SIMD subpackage
// remain pre-v1. Exported declarations and tests define the current surface;
// README.md records its stability and ownership rules.
//
// # Toolchain
//
// The module requires Go 1.26, which builds tuned portable kernels. The pinned
// Go 1.27 development toolchain additionally enables validated vector kernels
// on arm64 or amd64 when built with GOEXPERIMENT=simd. Experimental SIMD files
// are bounded to that compiler family; later releases remain portable until
// they pass release-specific correctness and performance gates.
// Low-level structural and byte-scanning kernels live in the explicitly
// unstable github.com/thesyncim/vibejson/x subpackages. Numeric and time
// formatting helpers plus runtime reporting remain in the pre-v1
// github.com/thesyncim/vibejson/simd subpackage.
package vibejson
