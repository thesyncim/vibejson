package simdjson

import (
	"errors"
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

	docs       []Index
	srcChunk   []byte       // current source arena chunk
	entryChunk []IndexEntry // current entry arena chunk
	scratch    []IndexEntry // spill tape for documents the entry chunk cannot hold
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

// Len returns the number of stored documents.
func (s *DocSet) Len() int {
	return len(s.docs)
}

// Doc returns the Index over the ith document. The Index borrows the set's
// arenas and remains valid across later Appends. An out-of-range ordinal
// panics like an out-of-range slice index.
func (s *DocSet) Doc(i int) Index {
	return s.docs[i]
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
		s.srcChunk = make([]byte, 0, docSetChunkCap(cap(s.srcChunk), len(src), docSetMinSrcChunk, docSetMaxSrcChunk))
	}
	mark := len(s.srcChunk)
	s.srcChunk = append(s.srcChunk, src...)
	index, err := s.buildDoc(s.srcChunk[mark:len(s.srcChunk):len(s.srcChunk)])
	if err != nil {
		s.srcChunk = s.srcChunk[:mark]
		return 0, err
	}
	s.docs = append(s.docs, index)
	return len(s.docs) - 1, nil
}

// buildDoc indexes one arena-resident document into the entry arena. It first
// builds directly into the current chunk's free tail — the common case, one
// validation pass and no copy. A document that outgrows the tail builds once
// into the spill tape and moves its exact entry count into a fresh chunk while
// the entries are cache-hot; a precount pass (RequiredIndexEntries) would
// instead rescan every document's source, which benchmarks slower than the
// occasional spill copy.
func (s *DocSet) buildDoc(src []byte) (Index, error) {
	if cap(s.entryChunk) == 0 {
		s.entryChunk = make([]IndexEntry, 0, docSetMinEntryChunk)
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
			return index, nil
		}
		index.entries = index.entries[:n:n]
		s.entryChunk = s.entryChunk[:used+n]
		return index, nil
	}
	if !errors.Is(err, document.ErrIndexFull) {
		return Index{}, err
	}
	// One entry is recorded per value or key, and each spans at least one
	// distinct source byte, so len(src)+2 entries always suffice.
	if cap(s.scratch) < len(src)+2 {
		s.scratch = make([]IndexEntry, 0, len(src)+2)
	}
	index, err = buildIndexOptions(src, s.scratch[:0], s.Options)
	if err != nil {
		return Index{}, err
	}
	n := len(index.entries)
	chunk := make([]IndexEntry, n, docSetChunkCap(cap(s.entryChunk), n, docSetMinEntryChunk, docSetMaxEntryChunk))
	copy(chunk, index.entries)
	s.entryChunk = chunk
	return Index{src: src, entries: chunk[:n:n]}, nil
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
func (s *DocSet) AppendPointer(dst []RawValue, pointer CompiledPointer) ([]RawValue, error) {
	mark := len(dst)
	for i := range s.docs {
		node, ok, err := s.docs[i].PointerCompiled(pointer)
		if err != nil {
			return dst[:mark], err
		}
		if !ok {
			dst = append(dst, RawValue{})
			continue
		}
		dst = append(dst, node.Raw())
	}
	return dst, nil
}
