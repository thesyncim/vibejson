package simdjson

import (
	"errors"
	"sync"
	"unsafe"

	"github.com/thesyncim/simdjson/document"
)

// A DocSet indexes a batch of JSON documents into shared storage. Append
// copies each document into a chunked source arena and builds its index in a
// chunked entry arena, so a set of N documents costs a handful of arena
// allocations instead of two per document, and consecutive documents' bytes
// and entries stay adjacent for batch scans. ReadFrom ingests an entire
// stream of documents in bulk, reading straight into the arena.
//
// The two arenas follow the interner's never-moving discipline: a chunk is
// appended to only within its capacity and retired in place when full, so
// nothing handed out is ever invalidated.
//
//	source arena   chunk: [ doc0 | doc1 | doc2 |  spare )
//	entry arena    chunk: [ e(0) | e(1) | e(2) |  spare )
//	docs[i] = Index{src, entries}: subslices of the two arenas
//
// Under the opt-in ShapeTapes mode a conforming document's e(i) holds only
// its value entries — the classic tape's keys live once in a compiled shape
// — with a per-document header alongside; docset_shape.go owns that
// representation and its contracts.
//
// Growth within an Append is transactional: the document's bytes land in the
// source chunk's uncommitted tail first, the index builds into the entry
// chunk's tail, and only success extends the committed lengths over both.
// On failure the tails are truncated, which is a length rollback with no
// copying — the uncommitted bytes were never visible.
//
// Doc returns the ordinary Index over a stored document, with the full
// per-document API. Arena chunks are append-only and never moved, so every
// Index, Node, and RawValue obtained from the set remains valid across later
// Appends. A failed Append leaves the set unchanged. The zero DocSet is empty
// and ready to use. A DocSet is not safe for concurrent use; concurrent reads
// are safe once appending stops.
type DocSet struct {
	// Options configures indexing, read at each Append. Set HashKeys before
	// the first Append for lookup-heavy engines: enrichment costs one linear
	// pass over the entries at build time and accelerates every Get after it.
	Options document.IndexOptions

	// ShapeTapes opts the set into shape-deduplicated tapes, read at each
	// Append like Options: a document whose root is a flat non-empty object
	// byte-matching a compiled shape of the set's internal cache stores one
	// entry per member value instead of the classic tape, roughly halving
	// tape storage on shape-clustered corpora and letting the batch
	// extractors index value arrays directly. Value entries are themselves
	// dual-width: a document whose root span fits 16-bit offsets (under
	// 64 KiB) stores 8-byte entries, halving the value array again; wider
	// documents keep 16-byte entries. Semantics never change — every
	// lookup, extractor, and Doc result is identical to classic storage —
	// but Doc's cost does: its first call on a shape-taped document
	// materializes and permanently caches the classic tape (see Doc). Set it
	// before the first Append. Ingest pays a per-document conformance check;
	// non-conforming documents are stored classic, unchanged. See
	// docset_shape.go for the representation and its proof obligations.
	ShapeTapes bool

	// Postings opts the set into the inverted existence and containment layer,
	// read at each Append like Options: a document's top-level keys and scalar
	// values are folded into DocSet-owned postings so WhereExists and
	// WhereContains answer selective predicates by probing a candidate set
	// rather than scanning every document. Key existence resolves through the
	// shape index, so it is most effective paired with ShapeTapes — with shapes
	// off every document is the non-conforming remainder and existence stays
	// correct but scans; value containment prunes regardless. Set it before the
	// first Append: enabling it later leaves earlier documents unindexed, and
	// the query paths detect the gap and fall back to a full scan. Ingest pays
	// one pass over each document's top-level members. See docset_postings.go
	// for the representation and its proof obligations.
	Postings bool

	docs []Index
	// mappedDocs is Store-only compact metadata for a DocSet page reopened
	// from a validated image. It replaces per-document slice and shape pointers
	// with pointer-free external descriptors; mappedShapes scales with distinct
	// layouts, not documents. Public Open keeps the ordinary representation.
	mappedDocs   *storeMappedDocs
	mappedShapes []*shapeRecord
	mappedBase   uint64
	mappedCount  int
	mappedNarrow int
	srcChunk     []byte       // current source arena chunk
	entryChunk   []IndexEntry // current entry arena chunk
	scratch      []IndexEntry // spill tape for documents the entry chunk cannot hold

	// Shape-tape state (docset_shape.go): tapeRefs is empty or docs-aligned
	// and holds each document's dedup header; narrow is the slab of 8-byte
	// value entries for narrow-width documents, addressed by each ref's
	// offset (it relocates freely as it grows: no pointer into it ever
	// leaves a call); shapes is the internal cache the ingest conformance
	// gate resolves against; widened caches the classic tapes Doc has
	// re-materialized, under widenMu so concurrent reads stay safe once
	// appending stops. wideValueTapes is the width test seam: it forces
	// 16-byte value entries for narrow-eligible documents so the
	// differential tests can hold the two widths against each other on
	// identical documents; nothing outside tests sets it.
	tapeRefs       []shapeTapeRef
	narrow         []shapeNarrowValue
	shapes         ShapeCache
	widened        map[int][]IndexEntry
	widenMu        sync.Mutex
	wideValueTapes bool

	// postings is the inverted existence/containment layer (docset_postings.go),
	// built at commit under the Postings opt-in and nil until the first indexed
	// document. It owns its structures and is read by WhereExists and
	// WhereContains; a partial index (Postings enabled late) is detected and
	// bypassed, so the pointer being non-nil never forces a stale answer.
	postings *docPostings

	// Arena minima are internal construction hints. Zero preserves the bulk
	// DocSet policy below; bounded immutable Store chunks select smaller first
	// allocations so a one-document rewrite does not buy stream-sized arenas.
	// The branch is paid only when a new arena chunk is allocated, never on a
	// read or on an Append that fits the current chunk.
	arenaMinSrc     int
	arenaMinEntries int
	// dropEmptySpill is the bounded-Store policy: if shape compaction removes
	// every entry from a spill-built document, retain no empty entry arena.
	// A bulk DocSet keeps that arena for its next Append; one Store rebuild has
	// exactly one replacement Append, so the capacity has no future consumer.
	dropEmptySpill bool

	// ValueDict opts the set into the corpus-wide value dictionary, read at
	// each Append like ShapeTapes: a value span that recurs across the set —
	// an enum string, a label, a repeated sub-object — is interned once into
	// the shared arena, and each later occurrence records a compact reference
	// in place of the bytes, from which a compacting store drops the repeated
	// source while every value still resolves to a stable arena view in O(1)
	// (no decompression). Shape tapes remove key redundancy; the dictionary
	// removes value redundancy, and the two compose. Semantics never change:
	// every read is byte-identical to classic storage, the arena holding bytes
	// identical to the source they stand in for — so the mode is a space lever,
	// off by default, and the classic paths are untouched when it is off. Set
	// it before the first Append. See docset_valuedict.go for the
	// representation, its read contract, and the read==source invariant.
	ValueDict bool

	// Value-dictionary state (docset_valuedict.go), populated only under
	// ValueDict. values is the corpus-wide interner arena, never-moving like
	// the ShapeCache; valueSplices is the set-wide slab holding one record per
	// dictionary-backed occurrence, in per-document source order; valueRefs is
	// empty or docs-aligned and windows each document's records within the
	// slab; valueSeen gates interning on a value's second sighting, so a
	// singleton never costs an entry it cannot amortize. valueFloor is the
	// interning length floor (zero selects valueDictMinSpan) — a test seam the
	// exhaustive suite lowers to one so every repeated span is dictionary-
	// backed and the arena read path is exercised on every value shape; nothing
	// outside tests sets it.
	values       ValueInterner
	valueRefs    []valueDictRef
	valueSplices []valueSplice
	valueSeen    map[uint64]struct{}
	valueFloor   uint32

	// source is the serialized image an Open'd set borrows its arenas from
	// (docset_persist.go): a set reconstructed by Open holds the bytes here so
	// the zero-copy document sources and entry tapes that view into them stay
	// alive for the set's lifetime, and it is nil for a set built by Append.
	// The field pins the mapping; the caller owns keeping an underlying mmap
	// mapped, and every borrowed view is invalid once it is unmapped.
	source []byte
}

// Arena chunks grow geometrically between fixed bounds, like the interner's:
// small sets stay small, large ones amortize allocation, and a document larger
// than the maximum still gets a chunk of its own.
const (
	docSetMinSrcChunk   = 8 << 10
	docSetMaxSrcChunk   = 1 << 20
	docSetMinEntryChunk = 512
	docSetMaxEntryChunk = 64 << 10
)

func (s *DocSet) sourceChunkMinimum() int {
	if s.arenaMinSrc > 0 {
		return s.arenaMinSrc
	}
	return docSetMinSrcChunk
}

func (s *DocSet) entryChunkMinimum() int {
	if s.arenaMinEntries > 0 {
		return s.arenaMinEntries
	}
	return docSetMinEntryChunk
}

// Len returns the number of stored documents.
func (s *DocSet) Len() int {
	if s.mappedDocs != nil {
		return s.mappedCount
	}
	return len(s.docs)
}

// Doc returns the Index over the ith document. The Index borrows the set's
// arenas and remains valid across later Appends. An out-of-range ordinal
// panics like an out-of-range slice index.
//
// Under ShapeTapes, a shape-taped document's classic tape no longer exists,
// so Doc's first call on it synthesizes one — an allocation and one pass
// over the members — and caches it for the set's lifetime: later calls
// return the same storage, handles stay stable, and concurrent Doc calls
// remain safe once appending stops. The result is identical to the Index
// classic storage would have returned. The space cost is the honest flip
// side: widening a document re-buys the classic tape the mode dropped, so
// engines wanting the space win extract through the batch primitives, which
// read the deduplicated form directly.
func (s *DocSet) Doc(i int) Index {
	if template, ok := s.storeTemplateAt(i); ok {
		return s.widenStoreTemplate(i, template)
	}
	if r := s.shapeTapeRefAt(i); r.rec != nil {
		return s.widenShapeTape(i, r)
	}
	return s.docAt(i)
}

// Append copies src into the set, validates and indexes the copy, and returns
// the new document's ordinal. src may be reused or discarded after the call.
// Invalid input returns the same error BuildIndexOptions reports and leaves
// the set unchanged: no partial document is ever visible.
func (s *DocSet) Append(src []byte) (int, error) {
	// The copy lands first: the index must alias arena bytes, not the
	// caller's buffer. Appending within capacity never moves a chunk, so
	// previously returned views survive; on failure the copied bytes are
	// still uncommitted arena tail and restoring the length removes them.
	if len(s.srcChunk)+len(src) > cap(s.srcChunk) {
		s.srcChunk = make([]byte, 0, docSetChunkCap(cap(s.srcChunk), len(src), s.sourceChunkMinimum(), docSetMaxSrcChunk))
	}
	mark := len(s.srcChunk)
	s.srcChunk = append(s.srcChunk, src...)
	index, ref, err := s.buildDoc(s.srcChunk[mark:len(s.srcChunk):len(s.srcChunk)])
	if err != nil {
		s.srcChunk = s.srcChunk[:mark]
		return 0, err
	}
	return s.commitDoc(index, ref), nil
}

// appendStoreSchema is Store's fused parse-and-schema path. The schema sees
// the complete structural index before optional shape compaction, so a valid
// write is parsed once and a rejected write commits neither source nor tape.
func (s *DocSet) appendStoreSchema(
	src []byte,
	schema *StoreSchema,
) (int, error) {
	if len(s.srcChunk)+len(src) > cap(s.srcChunk) {
		s.srcChunk = make(
			[]byte, 0,
			docSetChunkCap(
				cap(s.srcChunk), len(src), s.sourceChunkMinimum(),
				docSetMaxSrcChunk,
			),
		)
	}
	mark := len(s.srcChunk)
	s.srcChunk = append(s.srcChunk, src...)
	index, ref, err := s.buildDocSchema(
		s.srcChunk[mark:len(s.srcChunk):len(s.srcChunk)],
		schema,
	)
	if err != nil {
		s.srcChunk = s.srcChunk[:mark]
		return 0, err
	}
	return s.commitDoc(index, ref), nil
}

// buildDoc indexes one arena-resident document into the entry arena. It first
// builds directly into the current chunk's free tail — the common case, one
// validation pass and no copy. A document that outgrows the tail builds once
// into the spill tape and moves its exact entry count into a fresh chunk while
// the entries are cache-hot; a precount pass (`RequiredIndexEntries`) would
// instead rescan every document's source on the common path. Under ShapeTapes
// a conforming document is compacted
// to its value entries between build and commit (shapeTapeCompact), so key
// entries never reach committed storage; the returned ref is its dedup header,
// zero for classic documents. The spill path avoids a mandatory source
// precount and therefore preserves the one-pass common case.
func (s *DocSet) buildDoc(src []byte) (Index, shapeTapeRef, error) {
	if cap(s.entryChunk) == 0 {
		s.entryChunk = make([]IndexEntry, 0, s.entryChunkMinimum())
	}
	used := len(s.entryChunk)
	free := s.entryChunk[used:]
	index, err := buildIndexOptions(src, free, s.Options)
	if err == nil {
		n := len(index.entries)
		if n > 0 &&
			unsafe.SliceData(index.entries) != unsafe.SliceData(free) {
			// Both builders write into the storage they are handed; the
			// commit below extends the chunk over exactly those entries. If
			// the invariant ever broke, extending would expose garbage, so
			// fail closed: the document keeps the storage it was built in and
			// the chunk stays unchanged.
			return index, shapeTapeRef{}, nil
		}
		index.entries = index.entries[:n:n]
		index, ref := s.shapeTapeCompact(index)
		s.entryChunk = s.entryChunk[:used+len(index.entries)]
		return index, ref, nil
	}
	if !errors.Is(err, document.ErrIndexFull) {
		return Index{}, shapeTapeRef{}, err
	}
	// One entry is recorded per value or key, and each spans at least one
	// distinct source byte, so len(src)+2 entries always suffice.
	if cap(s.scratch) < len(src)+2 {
		s.scratch = make([]IndexEntry, 0, len(src)+2)
	}
	index, err = buildIndexOptions(src, s.scratch[:0], s.Options)
	if err != nil {
		return Index{}, shapeTapeRef{}, err
	}
	index, ref := s.shapeTapeCompact(index)
	n := len(index.entries)
	if n == 0 && s.dropEmptySpill {
		return Index{src: src}, ref, nil
	}
	chunk := make(
		[]IndexEntry, n,
		docSetChunkCap(
			cap(s.entryChunk), n, s.entryChunkMinimum(),
			docSetMaxEntryChunk,
		),
	)
	copy(chunk, index.entries)
	s.entryChunk = chunk
	return Index{src: src, entries: chunk[:n:n]}, ref, nil
}

// buildDocSchema intentionally specializes buildDoc rather than adding a
// validator callback or branch to DocSet's public hot path. The duplicated
// arena choreography is small and mechanically parallel; keeping it here
// preserves identical schemaless code generation while placing validation
// between the one structural parse and shape-tape compaction.
func (s *DocSet) buildDocSchema(
	src []byte,
	schema *StoreSchema,
) (Index, shapeTapeRef, error) {
	if cap(s.entryChunk) == 0 {
		s.entryChunk = make([]IndexEntry, 0, s.entryChunkMinimum())
	}
	used := len(s.entryChunk)
	free := s.entryChunk[used:]
	index, err := buildIndexOptions(src, free, s.Options)
	if err == nil {
		n := len(index.entries)
		if n > 0 && unsafe.SliceData(index.entries) != unsafe.SliceData(free) {
			// Both builders write into the storage they are handed; the
			// commit below extends the chunk over exactly those entries. If
			// the invariant ever broke, extending would expose garbage, so
			// fail closed: the document keeps the storage it was built in and
			// the chunk stays unchanged.
			if err := schema.ValidateIndex(index); err != nil {
				return Index{}, shapeTapeRef{}, err
			}
			return index, shapeTapeRef{}, nil
		}
		index.entries = index.entries[:n:n]
		if err := schema.ValidateIndex(index); err != nil {
			return Index{}, shapeTapeRef{}, err
		}
		index, ref := s.shapeTapeCompact(index)
		s.entryChunk = s.entryChunk[:used+len(index.entries)]
		return index, ref, nil
	}
	if !errors.Is(err, document.ErrIndexFull) {
		return Index{}, shapeTapeRef{}, err
	}
	// One entry is recorded per value or key, and each spans at least one
	// distinct source byte, so len(src)+2 entries always suffice.
	if cap(s.scratch) < len(src)+2 {
		s.scratch = make([]IndexEntry, 0, len(src)+2)
	}
	index, err = buildIndexOptions(src, s.scratch[:0], s.Options)
	if err != nil {
		return Index{}, shapeTapeRef{}, err
	}
	if err := schema.ValidateIndex(index); err != nil {
		return Index{}, shapeTapeRef{}, err
	}
	index, ref := s.shapeTapeCompact(index)
	n := len(index.entries)
	if n == 0 && s.dropEmptySpill {
		return Index{src: src}, ref, nil
	}
	chunk := make([]IndexEntry, n, docSetChunkCap(cap(s.entryChunk), n, s.entryChunkMinimum(), docSetMaxEntryChunk))
	copy(chunk, index.entries)
	s.entryChunk = chunk
	return Index{src: src, entries: chunk[:n:n]}, ref, nil
}

// docSetChunkCap sizes the next arena chunk: double the previous within
// [min, max], then at least need.
func docSetChunkCap(prev, need, min, max int) int {
	size := 2 * prev
	if size < min {
		size = min
	}
	if size > max {
		size = max
	}
	if size < need {
		size = need
	}
	return size
}

// AppendPointer resolves one compiled pointer against every document in the
// set, in ordinal order, appending one RawValue per document to dst: the
// target's exact source bytes when present, the zero RawValue when absent. It
// returns the extended slice. The zero RawValue is the library's standing
// invalid value — no bytes, Kind Invalid — and a present target always has at
// least one byte, so absence needs no side channel and dst[i] stays aligned
// with document i. Appended values borrow the set's arenas under the usual
// RawValue lifetime rules.
//
// Each pointer token carries its content hash, precomputed once by
// CompilePointer, so per-document resolution rehashes nothing on
// key-hash-enriched objects (see document.IndexOptions.HashKeys).
// Resolution semantics per document are exactly Index.PointerCompiled's: an
// invalid array-index token for an array target stops the batch, returning
// dst truncated to its original length and the token's error.
//
// A shape-taped document resolves the first token against its stored shape —
// one memoized ordinal per shape, no key bytes touched — and descends any
// remaining tokens from the value through the ordinary compiled-pointer
// loop, so both storage forms share one resolution semantics.
func (s *DocSet) AppendPointer(dst []RawValue, pointer CompiledPointer) ([]RawValue, error) {
	mark := len(dst)
	var hint shapeTapeHint
	var templateHint storeTemplatePointerHint
	var key0 CompiledKey
	if len(pointer.tokens) > 0 {
		key0 = CompiledKey{key: pointer.tokens[0].text, hash: pointer.tokens[0].hash}
	}
	for i := 0; i < s.Len(); i++ {
		if template, templateOK := s.storeTemplateAt(i); templateOK {
			ordinal, ok, err := templateHint.resolve(template, pointer)
			if err != nil {
				return dst[:mark], err
			}
			if !ok {
				dst = append(dst, RawValue{})
				continue
			}
			span := s.storeTemplateSpan(i, template, ordinal)
			doc := s.docAt(i)
			raw := RawValue{src: doc.src[span&0xffff : span>>16]}
			if s.ValueDict {
				raw = s.valueRaw(i, span&0xffff, raw)
			}
			dst = append(dst, raw)
			continue
		}
		if r := s.shapeTapeRefAt(i); r.rec != nil {
			doc := s.docAt(i)
			if len(pointer.tokens) == 0 {
				// The empty pointer selects the root. Compact Store rows recover
				// this otherwise-cold span from their validated source.
				start, end := s.shapeTapeRootSpan(doc, r)
				dst = append(dst, RawValue{src: doc.src[start:end]})
				continue
			}
			ord := hint.lookup(r.rec, key0)
			if ord < 0 {
				dst = append(dst, RawValue{})
				continue
			}
			rest := pointer.tokens[1:]
			if len(rest) == 0 {
				// The common single-token pointer names the value itself: its
				// span is already in hand at both entry widths, so the raw
				// slice is taken with no node to reconstitute or escape. Under
				// ValueDict a dictionary-backed value reads its interned span
				// from the shared arena instead — byte-identical.
				if r.narrow {
					nv := s.narrowAt(i, r, int(ord))
					raw := RawValue{src: doc.src[nv.span&0xFFFF : nv.span>>16]}
					if s.ValueDict {
						raw = s.valueRaw(i, nv.span&0xFFFF, raw)
					}
					dst = append(dst, raw)
				} else {
					e := &doc.entries[ord]
					raw := RawValue{src: doc.src[e.start:e.end]}
					if s.ValueDict {
						raw = s.valueRaw(i, e.start, raw)
					}
					dst = append(dst, raw)
				}
				continue
			}
			// Deeper tokens descend into the value. A flat value has no
			// children, so the descent resolves to absence except for an
			// array-index token error on an empty-array value; only this
			// uncommon path reconstitutes a narrow entry, which the descent
			// never lets outlive the iteration.
			var wide IndexEntry
			var node Node
			if r.narrow {
				wide = s.narrowAt(i, r, int(ord)).widen()
				node = Node{src: &doc.src[0], entry: &wide}
			} else {
				node = Node{src: &doc.src[0], entry: &doc.entries[ord]}
			}
			next, ok, err := node.pointerTokens(rest)
			if err != nil {
				return dst[:mark], err
			}
			if !ok {
				dst = append(dst, RawValue{})
				continue
			}
			raw := next.Raw()
			if s.ValueDict {
				raw = s.valueRaw(i, next.entry.start, raw)
			}
			dst = append(dst, raw)
			continue
		}
		node, ok, err := s.docAt(i).PointerCompiled(pointer)
		if err != nil {
			return dst[:mark], err
		}
		if !ok {
			dst = append(dst, RawValue{})
			continue
		}
		raw := node.Raw()
		if s.ValueDict {
			raw = s.valueRaw(i, node.entry.start, raw)
		}
		dst = append(dst, raw)
	}
	return dst, nil
}

// AppendPointerRows is the sparse-gather form of [DocSet.AppendPointer]. It
// resolves pointer only for the document ordinals in rows, in the order
// supplied, and appends one RawValue per ordinal to dst. Duplicate ordinals
// produce duplicate values; an out-of-range ordinal panics like [DocSet.Doc].
// Absence, error rollback, borrowing, compiled-token, duplicate-key, and value
// dictionary semantics are exactly AppendPointer's.
//
// A shape-taped document resolves the first token against its proven shape and
// reads its narrow or wide value entry directly, so gathering selected rows
// never widens their compact tapes. This makes the method suitable for query
// engines applying an inverted posting list before materializing projected or
// aggregate columns: its work is O(len(rows)), not O(s.Len()).
func (s *DocSet) AppendPointerRows(dst []RawValue, rows []int, pointer CompiledPointer) ([]RawValue, error) {
	mark := len(dst)
	var hint shapeTapeHint
	var templateHint storeTemplatePointerHint
	var key0 CompiledKey
	if len(pointer.tokens) > 0 {
		key0 = CompiledKey{key: pointer.tokens[0].text, hash: pointer.tokens[0].hash}
	}
	for _, i := range rows {
		if template, templateOK := s.storeTemplateAt(i); templateOK {
			ordinal, ok, err := templateHint.resolve(template, pointer)
			if err != nil {
				return dst[:mark], err
			}
			if !ok {
				dst = append(dst, RawValue{})
				continue
			}
			span := s.storeTemplateSpan(i, template, ordinal)
			doc := s.docAt(i)
			raw := RawValue{src: doc.src[span&0xffff : span>>16]}
			if s.ValueDict {
				raw = s.valueRaw(i, span&0xffff, raw)
			}
			dst = append(dst, raw)
			continue
		}
		if r := s.shapeTapeRefAt(i); r.rec != nil {
			doc := s.docAt(i)
			if len(pointer.tokens) == 0 {
				start, end := s.shapeTapeRootSpan(doc, r)
				dst = append(dst, RawValue{src: doc.src[start:end]})
				continue
			}
			ord := hint.lookup(r.rec, key0)
			if ord < 0 {
				dst = append(dst, RawValue{})
				continue
			}
			rest := pointer.tokens[1:]
			if len(rest) == 0 {
				if r.narrow {
					nv := s.narrowAt(i, r, int(ord))
					raw := RawValue{src: doc.src[nv.span&0xFFFF : nv.span>>16]}
					if s.ValueDict {
						raw = s.valueRaw(i, nv.span&0xFFFF, raw)
					}
					dst = append(dst, raw)
				} else {
					e := &doc.entries[ord]
					raw := RawValue{src: doc.src[e.start:e.end]}
					if s.ValueDict {
						raw = s.valueRaw(i, e.start, raw)
					}
					dst = append(dst, raw)
				}
				continue
			}
			var wide IndexEntry
			var node Node
			if r.narrow {
				wide = s.narrowAt(i, r, int(ord)).widen()
				node = Node{src: &doc.src[0], entry: &wide}
			} else {
				node = Node{src: &doc.src[0], entry: &doc.entries[ord]}
			}
			next, ok, err := node.pointerTokens(rest)
			if err != nil {
				return dst[:mark], err
			}
			if !ok {
				dst = append(dst, RawValue{})
				continue
			}
			raw := next.Raw()
			if s.ValueDict {
				raw = s.valueRaw(i, next.entry.start, raw)
			}
			dst = append(dst, raw)
			continue
		}
		node, ok, err := s.docAt(i).PointerCompiled(pointer)
		if err != nil {
			return dst[:mark], err
		}
		if !ok {
			dst = append(dst, RawValue{})
			continue
		}
		raw := node.Raw()
		if s.ValueDict {
			raw = s.valueRaw(i, node.entry.start, raw)
		}
		dst = append(dst, raw)
	}
	return dst, nil
}
