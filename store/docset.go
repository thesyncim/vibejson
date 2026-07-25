package store

import (
	"errors"
	"sync"
	"unsafe"

	"github.com/thesyncim/vibejson"
	"github.com/thesyncim/vibejson/document"
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
	// Append like Options: a conforming document stores one entry per member
	// value instead of the classic tape, roughly halving tape storage on
	// shape-clustered corpora and letting the batch extractors index value
	// arrays directly. Value entries are themselves dual-width: a document
	// whose root span fits 16-bit offsets (under 64 KiB) stores 8-byte
	// entries, halving the value array again; wider documents keep 16-byte
	// entries. Semantics never change — every lookup, extractor, and Doc
	// result is identical to classic storage — but Doc's cost does: its first
	// call on a shape-taped document materializes and permanently caches the
	// classic tape (see Doc). Set it before the first Append.
	//
	// Conformance decides whether the mode helps, and it is strict, so read it
	// before enabling: a document conforms only if its root is a non-empty
	// object whose every member value is a single tape entry — that is, a
	// scalar or an empty container. One nested object or array anywhere at the
	// root disqualifies the whole document, and its members are not considered
	// separately. The root's key sequence must also byte-match a shape the
	// set's cache has already compiled, which takes a second sighting of that
	// layout, and the layout must not repeat a decoded key name. Whatever
	// fails is stored classic, unchanged and correct.
	//
	// The consequence is that the option is a lever for flat corpora, not a
	// general one. On a flat corpus it is a large win; on a corpus whose
	// documents all carry a nested value it can store nothing at all, and
	// enabling it there buys a per-document conformance check that always
	// fails. Measure rather than assume: DocSet.Stats reports ShapeTaped, so a
	// short ingest tells you which corpus you have. See docset_shape.go for
	// the representation and its proof obligations.
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

	docs []vibejson.Index
	// mappedDocs is collection-only compact metadata for a DocSet page reopened
	// from a validated image. It replaces per-document slice and shape pointers
	// with pointer-free external descriptors; mappedShapes scales with distinct
	// layouts, not documents. Public Open keeps the ordinary representation.
	mappedDocs   *storeMappedDocs
	mappedShapes []*ShapeRecord
	mappedBase   uint64
	mappedCount  int
	mappedNarrow int
	srcChunk     []byte                // current source arena chunk
	entryChunk   []vibejson.IndexEntry // current entry arena chunk
	scratch      []vibejson.IndexEntry // spill tape for documents the entry chunk cannot hold

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
	tapeRefs       []ShapeTapeRef
	narrow         []ShapeNarrowValue
	shapes         ShapeCache
	widened        map[int][]vibejson.IndexEntry
	widenMu        sync.Mutex
	wideValueTapes bool

	// postings is the inverted existence/containment layer (docset_postings.go),
	// built at commit under the Postings opt-in and nil until the first indexed
	// document. It owns its structures and is read by WhereExists and
	// WhereContains; a partial index (Postings enabled late) is detected and
	// bypassed, so the pointer being non-nil never forces a stale answer.
	postings *docPostings

	// Arena minima are internal construction hints. Zero preserves the bulk
	// DocSet policy below; bounded immutable collection chunks select smaller first
	// allocations so a one-document rewrite does not buy stream-sized arenas.
	// The branch is paid only when a new arena chunk is allocated, never on a
	// read or on an Append that fits the current chunk.
	arenaMinSrc     int
	arenaMinEntries int
	// dropEmptySpill is the bounded-collection policy: if shape compaction removes
	// every entry from a spill-built document, retain no empty entry arena.
	// A bulk DocSet keeps that arena for its next Append; one collection rebuild has
	// exactly one replacement Append, so the capacity has no future consumer.
	dropEmptySpill bool
	// singleAppend states that at most one document is ever indexed into this
	// set through buildDoc, so the entry arena's growth policy has nothing left
	// to amortize. A bulk DocSet sizes a spilled document's replacement chunk
	// for the appends that follow it; a chunk rebuilt by buildStoreChunk takes
	// its other documents by reference through appendStoreDoc and parses only
	// the one replacement, so spare entries bought here would be retained
	// unwritten for the published chunk's whole live tenure. Only
	// prepareStoreDocSet sets it: a Builder page shares initChunkDocSet
	// but fills up to ChunkDocuments documents out of one entry arena, where
	// the geometric policy still pays for itself.
	singleAppend bool

	// ValueDict opts the set into the corpus-wide value dictionary, read at
	// each Append like ShapeTapes: a value span that recurs across the set —
	// an enum string, a label, a repeated sub-object — is interned once into
	// the shared arena, and each later occurrence records a compact reference
	// in place of the bytes, from which a compacting store drops the repeated
	// source while every value still resolves to a stable arena view in O(1)
	// (no decompression). Shape tapes remove key redundancy; the dictionary
	// removes value redundancy, and the two compose. Semantics never change:
	// every read is byte-identical to classic storage, the arena holding bytes
	// identical to the source they stand in for. Set it before the first
	// Append.
	//
	// It costs memory rather than saving it, and this is structural, not a
	// tuning failure. A live set retains every document's verbatim source so
	// that reads stay zero-copy, so enabling the dictionary removes nothing
	// from the source: the arena, the splice records, and the sighting set are
	// all additions on top of bytes that stay resident. Measured on a corpus
	// of long repeated enum strings — the case the mode exists for — it added
	// 36 B per document to a DocSet and 64 B per document to a collection, whose
	// dictionary is per chunk. Its payoff is entirely at rest: the repeated
	// source it lets a compacting or persisting writer drop, which
	// DocSetStats.DictSavedBytes models and which the same corpus put at 103 B
	// per document. Enable it when you are writing the set out or compacting
	// it, not to shrink a live set. See docset_valuedict.go for the
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
func (s *DocSet) Doc(i int) vibejson.Index {
	if template, ok := s.TemplateAt(i); ok {
		return s.widenStoreTemplate(i, template)
	}
	if r := s.ShapeTapeRefAt(i); r.Rec != nil {
		return s.widenShapeTape(i, r)
	}
	return s.DocAt(i)
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

// appendStoreSchema is collection's fused parse-and-schema path. The schema sees
// the complete structural index before optional shape compaction, so a valid
// write is parsed once and a rejected write commits neither source nor tape.
func (s *DocSet) appendStoreSchema(
	src []byte,
	schema *Schema,
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
func (s *DocSet) buildDoc(src []byte) (vibejson.Index, ShapeTapeRef, error) {
	if cap(s.entryChunk) == 0 {
		s.entryChunk = make([]vibejson.IndexEntry, 0, s.entryChunkMinimum())
	}
	used := len(s.entryChunk)
	free := s.entryChunk[used:]
	index, err := vibejson.BuildIndexOptions(src, free, s.Options)
	if err == nil {
		n := len(index.Entries)
		if n > 0 &&
			unsafe.SliceData(index.Entries) != unsafe.SliceData(free) {
			// Both builders write into the storage they are handed; the
			// commit below extends the chunk over exactly those entries. If
			// the invariant ever broke, extending would expose garbage, so
			// fail closed: the document keeps the storage it was built in and
			// the chunk stays unchanged.
			return index, ShapeTapeRef{}, nil
		}
		index.Entries = index.Entries[:n:n]
		index, ref := s.shapeTapeCompact(index)
		s.entryChunk = s.entryChunk[:used+len(index.Entries)]
		return index, ref, nil
	}
	if !errors.Is(err, document.ErrIndexFull) {
		return vibejson.Index{}, ShapeTapeRef{}, err
	}
	// One entry is recorded per value or key, and each spans at least one
	// distinct source byte, so len(src)+2 entries always suffice.
	if cap(s.scratch) < len(src)+2 {
		s.scratch = make([]vibejson.IndexEntry, 0, len(src)+2)
	}
	index, err = vibejson.BuildIndexOptions(src, s.scratch[:0], s.Options)
	if err != nil {
		return vibejson.Index{}, ShapeTapeRef{}, err
	}
	index, ref := s.shapeTapeCompact(index)
	n := len(index.Entries)
	if n == 0 && s.dropEmptySpill {
		return vibejson.Index{Src: src}, ref, nil
	}
	if tape, ok := s.exactSpillTape(index.Entries); ok {
		return vibejson.Index{Src: src, Entries: tape}, ref, nil
	}
	chunk := make(
		[]vibejson.IndexEntry, n,
		docSetChunkCap(
			cap(s.entryChunk), n, s.entryChunkMinimum(),
			docSetMaxEntryChunk,
		),
	)
	copy(chunk, index.Entries)
	s.entryChunk = chunk
	return vibejson.Index{Src: src, Entries: chunk[:n:n]}, ref, nil
}

// exactSpillTape gives a spilled document storage sized to the document itself
// rather than to a fresh arena chunk, and reports whether it applied. It is the
// singleAppend specialization of the tail both build paths share: the ordinary
// replacement chunk is an arena, sized by docSetChunkCap for the appends that
// follow it and installed as s.entryChunk for them to build into, so it carries
// max(2*prev, min, n) entries where only n are wanted. Under singleAppend those
// appends do not exist, and the document's tape pins the whole array for the
// published chunk's live tenure, so the surplus is retained and never written.
//
// The arena the failed build was attempted in is deliberately left in place
// rather than replaced: its free tail is the capacity prepareStoreDocSet
// reserved for the carried-over template tapes appendStoreDoc has still to
// synthesize, and installing an exact-fit chunk over it would force those
// appends to grow — copying this document's entries into the replacement and
// re-buying every byte just released. sealIngest drops it at publication if
// they never came.
func (s *DocSet) exactSpillTape(entries []vibejson.IndexEntry) ([]vibejson.IndexEntry, bool) {
	if !s.singleAppend {
		return nil, false
	}
	tape := make([]vibejson.IndexEntry, len(entries))
	copy(tape, entries)
	return tape, true
}

// sealIngest releases the ingest-only working state of a completed immutable
// chunk. A collection chunk is published once and every later edit rebuilds it by
// copy into a new DocSet, so after its final commitDoc no document can be
// indexed into this one again and two fields become pure debt: scratch, the
// spill build buffer, which is one entry per source byte of the widest
// document that overflowed the entry arena and whose contents are always
// copied into the document's own tape rather than handed out; and valueSeen,
// the value dictionary's first-sighting gate, which is one entry per distinct
// value hash and is read only by valueDictScan while ingesting. Neither is
// reachable from a read: the documents, tapes, narrow slab, splice slab, and
// interner arena are untouched, so a sealed chunk answers byte-identically.
//
// The entry arena goes only when it holds no committed entries, which is
// exactly the condition under which no document's tape aliases it: it is then
// a build target — for a spilled document that outgrew it, for one whose tape
// shape compaction removed entirely, or for template capacity a rebuild
// reserved and did not spend — and this set indexes nothing further. An arena
// with committed entries backs live tapes and must outlive the seal.
func (s *DocSet) sealIngest() {
	s.scratch = nil
	s.valueSeen = nil
	if len(s.entryChunk) == 0 {
		s.entryChunk = nil
	}
}

// buildDocSchema intentionally specializes buildDoc rather than adding a
// validator callback or branch to DocSet's public hot path. The duplicated
// arena choreography is small and mechanically parallel; keeping it here
// preserves identical schemaless code generation while placing validation
// between the one structural parse and shape-tape compaction.
func (s *DocSet) buildDocSchema(
	src []byte,
	schema *Schema,
) (vibejson.Index, ShapeTapeRef, error) {
	if cap(s.entryChunk) == 0 {
		s.entryChunk = make([]vibejson.IndexEntry, 0, s.entryChunkMinimum())
	}
	used := len(s.entryChunk)
	free := s.entryChunk[used:]
	index, err := vibejson.BuildIndexOptions(src, free, s.Options)
	if err == nil {
		n := len(index.Entries)
		if n > 0 && unsafe.SliceData(index.Entries) != unsafe.SliceData(free) {
			// Both builders write into the storage they are handed; the
			// commit below extends the chunk over exactly those entries. If
			// the invariant ever broke, extending would expose garbage, so
			// fail closed: the document keeps the storage it was built in and
			// the chunk stays unchanged.
			if err := schema.ValidateIndex(index); err != nil {
				return vibejson.Index{}, ShapeTapeRef{}, err
			}
			return index, ShapeTapeRef{}, nil
		}
		index.Entries = index.Entries[:n:n]
		if err := schema.ValidateIndex(index); err != nil {
			return vibejson.Index{}, ShapeTapeRef{}, err
		}
		index, ref := s.shapeTapeCompact(index)
		s.entryChunk = s.entryChunk[:used+len(index.Entries)]
		return index, ref, nil
	}
	if !errors.Is(err, document.ErrIndexFull) {
		return vibejson.Index{}, ShapeTapeRef{}, err
	}
	// One entry is recorded per value or key, and each spans at least one
	// distinct source byte, so len(src)+2 entries always suffice.
	if cap(s.scratch) < len(src)+2 {
		s.scratch = make([]vibejson.IndexEntry, 0, len(src)+2)
	}
	index, err = vibejson.BuildIndexOptions(src, s.scratch[:0], s.Options)
	if err != nil {
		return vibejson.Index{}, ShapeTapeRef{}, err
	}
	if err := schema.ValidateIndex(index); err != nil {
		return vibejson.Index{}, ShapeTapeRef{}, err
	}
	index, ref := s.shapeTapeCompact(index)
	n := len(index.Entries)
	if n == 0 && s.dropEmptySpill {
		return vibejson.Index{Src: src}, ref, nil
	}
	if tape, ok := s.exactSpillTape(index.Entries); ok {
		return vibejson.Index{Src: src, Entries: tape}, ref, nil
	}
	chunk := make([]vibejson.IndexEntry, n, docSetChunkCap(cap(s.entryChunk), n, s.entryChunkMinimum(), docSetMaxEntryChunk))
	copy(chunk, index.Entries)
	s.entryChunk = chunk
	return vibejson.Index{Src: src, Entries: chunk[:n:n]}, ref, nil
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
func (s *DocSet) AppendPointer(dst []vibejson.RawValue, pointer vibejson.CompiledPointer) ([]vibejson.RawValue, error) {
	mark := len(dst)
	var hint shapeTapeHint
	var templateHint storeTemplatePointerHint
	var key0 vibejson.CompiledKey
	if len(pointer.Tokens) > 0 {
		key0 = vibejson.CompiledKey{Key: pointer.Tokens[0].Text, Hash: pointer.Tokens[0].Hash}
	}
	for i := 0; i < s.Len(); i++ {
		if template, templateOK := s.TemplateAt(i); templateOK {
			ordinal, ok, err := templateHint.resolve(template, pointer)
			if err != nil {
				return dst[:mark], err
			}
			if !ok {
				dst = append(dst, vibejson.RawValue{})
				continue
			}
			span := s.TemplateSpan(i, template, ordinal)
			doc := s.DocAt(i)
			raw := vibejson.RawValue{Src: doc.Src[span&0xffff : span>>16]}
			if s.ValueDict {
				raw = s.valueRaw(i, span&0xffff, raw)
			}
			dst = append(dst, raw)
			continue
		}
		if r := s.ShapeTapeRefAt(i); r.Rec != nil {
			doc := s.DocAt(i)
			if len(pointer.Tokens) == 0 {
				// The empty pointer selects the root. Compact collection rows recover
				// this otherwise-cold span from their validated source.
				start, end := s.shapeTapeRootSpan(doc, r)
				dst = append(dst, vibejson.RawValue{Src: doc.Src[start:end]})
				continue
			}
			ord := hint.lookup(r.Rec, key0)
			if ord < 0 {
				dst = append(dst, vibejson.RawValue{})
				continue
			}
			rest := pointer.Tokens[1:]
			if len(rest) == 0 {
				// The common single-token pointer names the value itself: its
				// span is already in hand at both entry widths, so the raw
				// slice is taken with no node to reconstitute or escape. Under
				// ValueDict a dictionary-backed value reads its interned span
				// from the shared arena instead — byte-identical.
				if r.Narrow {
					nv := s.NarrowAt(i, r, int(ord))
					raw := vibejson.RawValue{Src: doc.Src[nv.Span&0xFFFF : nv.Span>>16]}
					if s.ValueDict {
						raw = s.valueRaw(i, nv.Span&0xFFFF, raw)
					}
					dst = append(dst, raw)
				} else {
					e := &doc.Entries[ord]
					raw := vibejson.RawValue{Src: doc.Src[e.Start:e.End]}
					if s.ValueDict {
						raw = s.valueRaw(i, e.Start, raw)
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
			var wide vibejson.IndexEntry
			var node vibejson.Node
			if r.Narrow {
				wide = s.NarrowAt(i, r, int(ord)).widen()
				node = vibejson.Node{Src: &doc.Src[0], Entry: &wide}
			} else {
				node = vibejson.Node{Src: &doc.Src[0], Entry: &doc.Entries[ord]}
			}
			next, ok, err := node.PointerTokens(rest)
			if err != nil {
				return dst[:mark], err
			}
			if !ok {
				dst = append(dst, vibejson.RawValue{})
				continue
			}
			raw := next.Raw()
			if s.ValueDict {
				raw = s.valueRaw(i, next.Entry.Start, raw)
			}
			dst = append(dst, raw)
			continue
		}
		node, ok, err := s.DocAt(i).PointerCompiled(pointer)
		if err != nil {
			return dst[:mark], err
		}
		if !ok {
			dst = append(dst, vibejson.RawValue{})
			continue
		}
		raw := node.Raw()
		if s.ValueDict {
			raw = s.valueRaw(i, node.Entry.Start, raw)
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
func (s *DocSet) AppendPointerRows(dst []vibejson.RawValue, rows []int, pointer vibejson.CompiledPointer) ([]vibejson.RawValue, error) {
	mark := len(dst)
	var hint shapeTapeHint
	var templateHint storeTemplatePointerHint
	var key0 vibejson.CompiledKey
	if len(pointer.Tokens) > 0 {
		key0 = vibejson.CompiledKey{Key: pointer.Tokens[0].Text, Hash: pointer.Tokens[0].Hash}
	}
	for _, i := range rows {
		if template, templateOK := s.TemplateAt(i); templateOK {
			ordinal, ok, err := templateHint.resolve(template, pointer)
			if err != nil {
				return dst[:mark], err
			}
			if !ok {
				dst = append(dst, vibejson.RawValue{})
				continue
			}
			span := s.TemplateSpan(i, template, ordinal)
			doc := s.DocAt(i)
			raw := vibejson.RawValue{Src: doc.Src[span&0xffff : span>>16]}
			if s.ValueDict {
				raw = s.valueRaw(i, span&0xffff, raw)
			}
			dst = append(dst, raw)
			continue
		}
		if r := s.ShapeTapeRefAt(i); r.Rec != nil {
			doc := s.DocAt(i)
			if len(pointer.Tokens) == 0 {
				start, end := s.shapeTapeRootSpan(doc, r)
				dst = append(dst, vibejson.RawValue{Src: doc.Src[start:end]})
				continue
			}
			ord := hint.lookup(r.Rec, key0)
			if ord < 0 {
				dst = append(dst, vibejson.RawValue{})
				continue
			}
			rest := pointer.Tokens[1:]
			if len(rest) == 0 {
				if r.Narrow {
					nv := s.NarrowAt(i, r, int(ord))
					raw := vibejson.RawValue{Src: doc.Src[nv.Span&0xFFFF : nv.Span>>16]}
					if s.ValueDict {
						raw = s.valueRaw(i, nv.Span&0xFFFF, raw)
					}
					dst = append(dst, raw)
				} else {
					e := &doc.Entries[ord]
					raw := vibejson.RawValue{Src: doc.Src[e.Start:e.End]}
					if s.ValueDict {
						raw = s.valueRaw(i, e.Start, raw)
					}
					dst = append(dst, raw)
				}
				continue
			}
			var wide vibejson.IndexEntry
			var node vibejson.Node
			if r.Narrow {
				wide = s.NarrowAt(i, r, int(ord)).widen()
				node = vibejson.Node{Src: &doc.Src[0], Entry: &wide}
			} else {
				node = vibejson.Node{Src: &doc.Src[0], Entry: &doc.Entries[ord]}
			}
			next, ok, err := node.PointerTokens(rest)
			if err != nil {
				return dst[:mark], err
			}
			if !ok {
				dst = append(dst, vibejson.RawValue{})
				continue
			}
			raw := next.Raw()
			if s.ValueDict {
				raw = s.valueRaw(i, next.Entry.Start, raw)
			}
			dst = append(dst, raw)
			continue
		}
		node, ok, err := s.DocAt(i).PointerCompiled(pointer)
		if err != nil {
			return dst[:mark], err
		}
		if !ok {
			dst = append(dst, vibejson.RawValue{})
			continue
		}
		raw := node.Raw()
		if s.ValueDict {
			raw = s.valueRaw(i, node.Entry.Start, raw)
		}
		dst = append(dst, raw)
	}
	return dst, nil
}
