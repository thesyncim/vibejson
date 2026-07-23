// Package slopjson implements strict, allocation-conscious JSON processing:
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
//	err := slopjson.Unmarshal(data, &event)
//
// Hot paths should compile the decoder once with [CompileDecoder] and reuse
// it; the returned [Decoder] is immutable and safe for concurrent use.
// [DecoderOptions] selects string ownership (ZeroCopy), unknown-field
// handling, case sensitivity, and merge-versus-replace semantics for existing
// destination state. The default merges like encoding/json. When decoding
// fails against the Go type, the returned [DecodeError] reports the byte
// offset and the path of the offending value, such as "items[3].scores[1]".
//
// # Encoding structs
//
// [Marshal] produces encoding/json.Marshal-compatible output for supported
// values and caches one compiled encoder per source type:
//
//	data, err := slopjson.Marshal(&event)
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
// shadow that is copied back before return. The slopjson-native
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
// [CompilePointer] avoids reparsing the pointer on hot paths. [BuildIndex] validates the input once and
// lays out a navigable structural index in caller-provided storage, which
// [Node] and the iterators traverse without allocating.
//
// # Dynamic values and transforms
//
// [Parse] produces an ordered syntax tree of [Value] nodes. [Unmarshal]
// into a *any produces ordinary maps, slices, strings, float64 values,
// booleans, and nil through a dedicated one-pass builder;
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
// For batches of documents, a [DocSet] indexes every appended document into
// shared arena storage whose bytes never move, so handles stay valid as the
// set grows; its ReadFrom ingests an entire stream of concatenated or
// NDJSON documents directly into the arena. [DocSet.AppendPointer] resolves
// one compiled pointer across every document into a columnar result. With
// postings enabled, [DocSet.WhereExists] and [DocSet.WhereContains] provide
// convenience queries; [DocSet.AppendWhereExists] and
// [DocSet.AppendWhereContainsIndex] reuse caller-owned result and prebuilt
// needle storage for zero-allocation warmed lookups. A [KeyInterner] maps
// object keys to dense identifiers for engines that group fields across
// documents.
//
// A [Store] is one physical JSON collection and adds keyed insert, replace,
// delete, TTL, immutable [Snapshot] publication, declared single/compound
// exact indexes, and wildcard postings. [Database] catalogs named [Collection]
// handles without adding namespace metadata or catalog lookup to their Store
// hot paths. [CompileStoreSchema] builds an optional immutable nested-field
// contract shared by Store, StoreBuilder, FileStore, and page-backed writes;
// successful validation uses the document's existing structural index and
// allocates no memory.
// [StoreBuilder] bulk-loads unique keyed documents directly into final bounded
// chunks, bulk-builds declared exact indexes, and publishes one completed
// Store, avoiding per-row persistent path copies while retaining the same
// ownership and validation rules.
// [Store.WriteTo] emits one immutable Store generation and [OpenStore]
// reconstructs a mutable Store whose source/native tape bytes borrow the
// caller's image. The image must remain immutable and live as long as the Store,
// retained snapshots, or derived borrowed values; [Snapshot.AppendRaw] and
// [Snapshot.AppendRawKey] provide lifetime-independent caller-buffered copy-out.
// Snapshot GetRaw reads are lock-free, clock-free, and allocation-free;
// [StoreKey] lets repeated reads verify a cached stable slot and bypass both
// hashing and the key trie, with a safe full-lookup fallback after movement.
// Updates parse only their replacement and share unchanged immutable
// source/tape storage; deletes rebuild bounded dense row metadata without
// tombstones or later compaction. [Store.CreateIndex] accepts nested RFC 6901
// paths. [Snapshot.AppendIndexMasks] exposes stable-slot Boolean words, while
// [Snapshot.AppendIndexRows] and [Snapshot.AppendPointerRows] support sparse
// late materialization. [Store.ExpireDue] groups expiry deletes by chunk;
// [Store.BackfillIndex] and [Store.ReclaimIndexes] let an event loop bound
// maintenance work. See docs/store.md for ownership, expiry semantics,
// operational counters, and tuning.
//
// [ShapeCache] compiles the layouts of recurring flat objects: Resolve
// fingerprints an object's key sequence, [Shape.Field] resolves a field
// name to its fixed position once per layout, and [FieldRef.In] reads that
// position from any same-shape document after verifying the key bytes, so a
// mis-routed document degrades to a Get fallback rather than a wrong field.
// [ShapeCache.AppendField] and [ShapeCache.AppendFields] fuse that machinery
// into batch extraction over a DocSet; [ShapeCache.AppendFieldRows] and
// [DocSet.AppendPointerRows] gather caller-selected ordinals without scanning
// or widening the rest. AppendFieldInt64,
// AppendFieldFloat64, and AppendFieldBool produce dense typed columns with
// validity masks in the same pass.
//
// # Pre-v1 API boundary
//
// Document-model APIs, native hook interfaces, and the public SIMD subpackage
// remain pre-v1 migration surfaces. The accepted boundary and migration order
// are recorded in docs/adr/0001-v1-api.md.
//
// # Toolchain
//
// The module requires Go 1.26, which builds tuned portable kernels. The pinned
// Go 1.27 development toolchain additionally enables validated vector kernels
// on arm64 or amd64 when built with GOEXPERIMENT=simd. Experimental SIMD files
// are bounded to that compiler family; later releases remain portable until
// they pass release-specific correctness and performance gates.
// Structural and byte-scanning kernels live behind internal package
// boundaries. Numeric and time formatting helpers plus runtime reporting
// remain in the pre-v1 github.com/thesyncim/slopjson/simd subpackage.
package slopjson
