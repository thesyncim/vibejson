package simdjson

import (
	"errors"
	"fmt"
	"hash/maphash"
	"math/bits"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/thesyncim/simdjson/document"
)

// StoreOptions fixes the representation of chunks created by a Store. The
// zero value selects 64-document chunks with the ordinary DocSet layout.
// ShapeTapes, Postings, ValueDict, and IndexOptions have the same semantics as
// their DocSet counterparts. Options are frozen by the first operation that
// initializes the Store (currently Put or AddIndex).
type StoreOptions struct {
	// ChunkDocuments bounds documents rebuilt by one ordinary mutation. Zero
	// selects 64; valid explicit values are 1 through 64.
	ChunkDocuments int
	// IndexOptions configures each bounded DocSet's structural index.
	IndexOptions document.IndexOptions
	// ShapeTapes enables per-chunk shape-deduplicated tapes.
	ShapeTapes bool
	// Postings builds the physical posting layer from the first Put.
	Postings bool
	// ValueDict enables a value dictionary scoped to each immutable chunk.
	ValueDict bool
	// Schema optionally enforces compiled root and RFC 6901 field constraints
	// on every insert or replacement. Nil preserves the schemaless fast path.
	Schema *StoreSchema
}

// storeStateOptions is the pointer-free subset copied into every immutable
// Store generation. Collection-wide schema belongs to Store, not to each
// publication; keeping it out of this value preserves the schemaless state
// size class and avoids bytes/op growth on every mutation.
type storeStateOptions struct {
	IndexOptions   document.IndexOptions
	ChunkDocuments int
	ShapeTapes     bool
	Postings       bool
	ValueDict      bool
}

func (o StoreOptions) stateOptions() storeStateOptions {
	return storeStateOptions{
		IndexOptions: o.IndexOptions, ChunkDocuments: o.ChunkDocuments,
		ShapeTapes: o.ShapeTapes, Postings: o.Postings,
		ValueDict: o.ValueDict,
	}
}

const storeMaxChunkDocuments = 64

// ErrStoreTooLarge reports that the persistent chunk address space is full.
// The limit is 2^32-1 chunks (at most 274 billion documents with the default
// chunk size), so reaching it indicates a caller architecture error rather
// than an ordinary capacity event. The guard prevents uint32 wraparound.
var ErrStoreTooLarge = errors.New("simdjson: Store chunk address space exhausted")

func maphashString(seed maphash.Seed, key string) uint64 { return maphash.String(seed, key) }

func (o StoreOptions) normalized() (StoreOptions, error) {
	if o.ChunkDocuments == 0 {
		o.ChunkDocuments = storeMaxChunkDocuments
	}
	if o.ChunkDocuments < 1 || o.ChunkDocuments > storeMaxChunkDocuments {
		return StoreOptions{}, fmt.Errorf("simdjson: Store ChunkDocuments must be in [1,%d]", storeMaxChunkDocuments)
	}
	if o.Schema != nil && !o.Schema.valid() {
		return StoreOptions{}, fmt.Errorf(
			"%w: uninitialized compiled schema",
			ErrStoreSchemaDefinition,
		)
	}
	return o, nil
}

// A Store is a keyed, mutable collection of JSON documents with immutable
// snapshots and a lock-free raw read path. Writes are serialized, rebuild at
// most one bounded document chunk, path-copy only bounded-radix metadata, and
// publish one new state through an atomic pointer. A replacement parses only
// its new document; unchanged source and structural-tape storage is immutable
// and shared into the new chunk. Deletes rebuild dense row metadata without the
// document: no tombstone enters a read path and no later compaction is required
// to restore scan speed.
//
// The zero Store is ready to use. Set Options before the first Put, AddIndex,
// or CreateIndex, or use NewStore. A Store is safe for concurrent use.
// Snapshot readers take no writer lock; GetRaw and Range take no lock at all.
// Get may enter the synchronized shape-tape widening cache described on
// [Snapshot.Get]. A Store must not be copied after first use.
type Store struct {
	Options StoreOptions

	mu      sync.Mutex
	state   atomic.Pointer[storeState]
	options StoreOptions

	// Writer-only chunk-id sets make allocation and physical-index tracking
	// O(1). Empty chunk ids are reused, so insert/delete churn cannot grow the
	// chunk address space; reclamation takes indexed ids directly rather than
	// rescanning the entire vector after every bounded batch.
	free          storeIDSet
	postingChunks storeIDSet

	ttl           storeTTLState
	expireScratch []storeExpiryItem
	indexes       map[string]*storeIndexBuild
	reclaim       *storeIndexReclaim
}

// NewStore returns an empty Store configured with options. Invalid chunk
// bounds are reported by the first operation that initializes the Store, so
// construction itself cannot fail.
func NewStore(options StoreOptions) *Store {
	return &Store{Options: options}
}

type storeState struct {
	generation uint64
	count      int
	chunkCount uint32
	// mappedDocChunks counts current chunks that still borrow mappedDocs. A
	// rebuilt chunk becomes ordinary owned Go state; the last detach drops the
	// state-level owner while retained snapshots keep their own chunk owners.
	mappedDocChunks uint32
	seed            maphash.Seed
	options         storeStateOptions
	keys            *storeKeyNode
	// baseKeys is the compact immutable directory created by StoreBuilder or
	// OpenStore. keys is then only the path-copied overlay for later insertions
	// and moved keys.
	baseKeys   *storeMappedKeys
	mappedDocs *storeMappedDocs
	chunks     storeChunkVector
	indexes    []StoreIndexInfo
	secondary  []storeIndexSnapshot
	// source pins a Store image borrowed by OpenStore. Ordinary heap-built
	// states leave it nil. Every path copy carries the slice, so mapped source
	// bytes remain reachable for the lifetime of current and retained snapshots;
	// the caller still owns when an underlying mapping is unmapped.
	source []byte
}

type storeChunk struct {
	docs       DocSet
	keys       []string
	keyBytes   []byte
	mappedKeys *storeMappedKeys
	mappedBase uint64
	ord        [storeMaxChunkDocuments]uint8
	live       uint64
	count      uint8
}

type storeIDSet struct {
	ids []uint32
	pos map[uint32]int
}

func (s *storeIDSet) add(id uint32) {
	if s.pos == nil {
		s.pos = make(map[uint32]int)
	}
	if _, exists := s.pos[id]; exists {
		return
	}
	s.pos[id] = len(s.ids)
	s.ids = append(s.ids, id)
}

func (s *storeIDSet) remove(id uint32) {
	pos, exists := s.pos[id]
	if !exists {
		return
	}
	last := len(s.ids) - 1
	other := s.ids[last]
	s.ids[pos] = other
	s.ids = s.ids[:last]
	delete(s.pos, id)
	if pos != last {
		s.pos[other] = pos
	}
}

func initChunkDocSet(
	docs *DocSet,
	options storeStateOptions,
	postings bool,
) {
	*docs = DocSet{
		Options:    options.IndexOptions,
		ShapeTapes: options.ShapeTapes,
		Postings:   postings,
		ValueDict:  options.ValueDict,
		// A Store carries unchanged document sources directly into the next
		// immutable chunk. Exact first source chunks prevent a short document
		// from pinning stream-sized spare capacity for its whole live tenure.
		arenaMinSrc:     1,
		arenaMinEntries: 16,
		dropEmptySpill:  true,
	}
	// ShapeCache's default arenas amortize compilation across an unbounded
	// DocSet. A Store chunk is capped at 64 documents and is rebuilt by copy;
	// exact minima prevent one page-local shape from pinning bulk-sized field,
	// table, record, and spelling slabs. The compiler and read representation
	// stay identical, so this policy change has no query-path branch.
	docs.shapes.arenaMinRecords = 1
	docs.shapes.arenaMinFields = 1
	docs.shapes.arenaMinSlots = 1
	docs.shapes.arenaMinBytes = 1
}

// prepareStoreDocSet reserves the dense per-chunk tables and seeds its shape
// cache with exactly the immutable shape records referenced by surviving rows.
// Source bytes and classic tapes remain independently immutable and may be
// shared; row tables and the narrow-value slab are copied because their offsets
// are chunk-local. Excluding replaceSlot is what prevents an updated-away
// shape from becoming historical cache debt.
func prepareStoreDocSet(
	docs *DocSet,
	options storeStateOptions,
	postings bool,
	old *storeChunk,
	live uint64,
	replaceSlot int,
) {
	initChunkDocSet(docs, options, postings)
	count := bits.OnesCount64(live)
	docs.docs = make([]Index, 0, count)
	if !options.ShapeTapes {
		return
	}
	// A one-row replacement cannot reach the repeat-sighting gate, and has no
	// shape ref to store. Let commitDoc allocate lazily for the uncommon case
	// where a delete leaves one already-shaped survivor; this keeps the
	// ChunkDocuments=1 update path at its original allocation count.
	if count > 1 {
		docs.tapeRefs = make([]shapeTapeRef, 0, count)
	}
	if old == nil {
		return
	}
	narrowCap := 0
	entryCap := 0
	for bitsLeft := live; bitsLeft != 0; bitsLeft &= bitsLeft - 1 {
		slot := bits.TrailingZeros64(bitsLeft)
		if slot == replaceSlot || old.live&(uint64(1)<<uint(slot)) == 0 {
			continue
		}
		if template, ok := old.docs.storeTemplateAt(int(old.ord[slot])); ok {
			entryCap += len(template.index.entries)
			continue
		}
		ref := old.docs.shapeTapeRefAt(int(old.ord[slot]))
		if ref.narrow {
			narrowCap += len(ref.rec.fields)
		}
		docs.shapes.seedRecord(ref.rec)
	}
	if replaceSlot >= 0 {
		if old.live&(uint64(1)<<uint(replaceSlot)) != 0 {
			oldOrd := int(old.ord[replaceSlot])
			if template, ok := old.docs.storeTemplateAt(oldOrd); ok {
				entryCap += len(template.index.entries)
			} else {
				ref := old.docs.shapeTapeRefAt(oldOrd)
				if ref.narrow {
					narrowCap += len(ref.rec.fields)
				}
			}
		} else if old.count != 0 {
			// An insertion has no old row to size from. Reserve the current
			// average so a same-shape replacement—the common case when a freed
			// slot is reused—does not grow and recopy the whole narrow slab.
			narrowCap += old.docs.narrowLen() / int(old.count)
		}
	}
	if narrowCap != 0 {
		docs.narrow = make([]shapeNarrowValue, 0, narrowCap)
	}
	if entryCap != 0 {
		docs.entryChunk = make([]IndexEntry, 0, entryCap)
	}
}

// appendStoreDoc carries one validated immutable document into a new dense
// DocSet without copying its source or rebuilding its structural tape. Narrow
// shape values are the sole set-relative storage: copy them into the new slab
// and rewrite the private offset before commit. commitDoc then rebuilds any
// enabled chunk-local postings or value dictionary against the new ordinal.
func appendStoreDoc(dst *DocSet, old *DocSet, oldOrd int) int {
	if template, ok := old.storeTemplateAt(oldOrd); ok {
		used := len(dst.entryChunk)
		dst.entryChunk = old.synthStoreTemplate(oldOrd, template, dst.entryChunk)
		index := Index{src: old.docAt(oldOrd).src, entries: dst.entryChunk[used:]}
		return dst.commitDoc(index, shapeTapeRef{})
	}
	index := old.docAt(oldOrd)
	ref := old.shapeTapeRefAt(oldOrd)
	promoted := false
	if ref.rec == nil && dst.ShapeTapes {
		index, ref, promoted = copyStoreShapeTape(dst, index)
	}
	if ref.narrow && !promoted {
		n := uint32(len(ref.rec.fields))
		oldRef := ref
		ref.off = uint32(len(dst.narrow))
		for i := uint32(0); i < n; i++ {
			dst.narrow = append(dst.narrow, old.narrowAt(oldOrd, oldRef, int(i)))
		}
	}
	return dst.commitDoc(index, ref)
}

// copyStoreShapeTape promotes one reused classic flat object into the new
// chunk's compact representation without modifying its immutable old tape.
// Resolve preserves the ordinary repeat-sighting economics, and the exact key
// comparison preserves shapeTapeCompact's collision-proof trust boundary.
func copyStoreShapeTape(dst *DocSet, index Index) (Index, shapeTapeRef, bool) {
	entries := index.entries
	if len(entries) == 0 {
		return index, shapeTapeRef{}, false
	}
	root := &entries[0]
	count := int(root.Count())
	if root.Kind() != document.Object || count == 0 {
		return index, shapeTapeRef{}, false
	}
	shape, ok := dst.shapes.Resolve(nodeFromStorage(index.src, entries))
	if !ok || shape.rec.dupKeys {
		return index, shapeTapeRef{}, false
	}
	rec := shape.rec
	if !shapeTapeConforms(index, rec) {
		return index, shapeTapeRef{}, false
	}
	ref := shapeTapeRef{
		rec:      rec,
		start:    root.start,
		end:      root.end,
		enriched: root.keysHashed(),
	}
	if root.end <= shapeNarrowMaxEnd && !dst.wideValueTapes &&
		uint64(len(dst.narrow))+uint64(count) <= uint64(^uint32(0)) {
		ref.narrow = true
		ref.off = dst.appendNarrowShapeValues(entries, count)
		return Index{src: index.src}, ref, true
	}
	values := make([]IndexEntry, count)
	for m := range values {
		values[m] = entries[2*m+2]
	}
	return Index{src: index.src, entries: values}, ref, true
}

// buildStoreChunk is the single bounded rebuild primitive used by inserts,
// replacements, deletes, expiry batches, index backfill, and index reclaim.
// live is the exact post-edit slot mask. replaceSlot selects one slot whose
// bytes come from src; -1 means every remaining document comes from old.
func buildStoreChunk(
	options storeStateOptions,
	postings bool,
	old *storeChunk,
	live uint64,
	replaceSlot int,
	key string,
	src []byte,
) (*storeChunk, error) {
	if live == 0 {
		// Deleting or expiring the final row removes the vector leaf outright.
		// There is no replacement to validate and no empty chunk object to
		// publish, so avoid constructing tables that would be discarded below.
		return nil, nil
	}
	chunk := &storeChunk{
		keys: make([]string, options.ChunkDocuments),
	}
	prepareStoreDocSet(&chunk.docs, options, postings, old, live, replaceSlot)
	if old != nil {
		for bitsLeft := old.live; bitsLeft != 0; bitsLeft &= bitsLeft - 1 {
			slot := bits.TrailingZeros64(bitsLeft)
			chunk.keys[slot] = old.key(slot)
		}
	}
	chunk.live = live
	if old != nil {
		for removed := old.live &^ live; removed != 0; removed &= removed - 1 {
			chunk.keys[bits.TrailingZeros64(removed)] = ""
		}
	}
	if replaceSlot >= 0 {
		chunk.keys[replaceSlot] = key
	}
	for bitsLeft := chunk.live; bitsLeft != 0; bitsLeft &= bitsLeft - 1 {
		i := bits.TrailingZeros64(bitsLeft)
		var ord int
		if i == replaceSlot {
			var err error
			ord, err = chunk.docs.Append(src)
			if err != nil {
				return nil, err
			}
		} else {
			ord = appendStoreDoc(&chunk.docs, &old.docs, int(old.ord[i]))
		}
		chunk.ord[i] = uint8(ord)
		chunk.count++
	}
	if chunk.count == 0 {
		return nil, nil
	}
	return chunk, nil
}

// buildStoreChunkSchema is the schema-on specialization of buildStoreChunk.
// Keeping the replacement append explicit gives the much more common
// schemaless build a direct DocSet.Append call; a callback or inner-loop mode
// branch would add work to every schemaless mutation.
func buildStoreChunkSchema(
	options storeStateOptions,
	schema *StoreSchema,
	postings bool,
	old *storeChunk,
	live uint64,
	replaceSlot int,
	key string,
	src []byte,
) (*storeChunk, error) {
	if live == 0 {
		return nil, nil
	}
	chunk := &storeChunk{
		keys: make([]string, options.ChunkDocuments),
	}
	prepareStoreDocSet(
		&chunk.docs, options, postings, old, live, replaceSlot,
	)
	if old != nil {
		for bitsLeft := old.live; bitsLeft != 0; bitsLeft &= bitsLeft - 1 {
			slot := bits.TrailingZeros64(bitsLeft)
			chunk.keys[slot] = old.key(slot)
		}
	}
	chunk.live = live
	if old != nil {
		for removed := old.live &^ live; removed != 0; removed &= removed - 1 {
			chunk.keys[bits.TrailingZeros64(removed)] = ""
		}
	}
	if replaceSlot >= 0 {
		chunk.keys[replaceSlot] = key
	}
	for bitsLeft := chunk.live; bitsLeft != 0; bitsLeft &= bitsLeft - 1 {
		i := bits.TrailingZeros64(bitsLeft)
		var ord int
		if i == replaceSlot {
			var err error
			ord, err = chunk.docs.appendStoreSchema(src, schema)
			if err != nil {
				return nil, err
			}
		} else {
			ord = appendStoreDoc(
				&chunk.docs, &old.docs, int(old.ord[i]),
			)
		}
		chunk.ord[i] = uint8(ord)
		chunk.count++
	}
	if chunk.count == 0 {
		return nil, nil
	}
	return chunk, nil
}

func rebuildStoreChunk(
	options storeStateOptions,
	postings bool,
	old *storeChunk,
	slot int,
	key string,
	src []byte,
	keep bool,
) (*storeChunk, error) {
	var live uint64
	if old != nil {
		live = old.live
	}
	mask := uint64(1) << uint(slot)
	if keep {
		live |= mask
		return buildStoreChunk(
			options, postings, old, live, slot, key, src,
		)
	}
	return buildStoreChunk(
		options, postings, old, live&^mask, -1, "", nil,
	)
}

func rebuildStoreChunkSchema(
	options storeStateOptions,
	schema *StoreSchema,
	postings bool,
	old *storeChunk,
	slot int,
	key string,
	src []byte,
) (*storeChunk, error) {
	var live uint64
	if old != nil {
		live = old.live
	}
	live |= uint64(1) << uint(slot)
	return buildStoreChunkSchema(
		options, schema, postings, old, live, slot, key, src,
	)
}

func cloneStoreChunk(
	options storeStateOptions,
	postings bool,
	old *storeChunk,
) (*storeChunk, error) {
	if old == nil {
		return nil, nil
	}
	return buildStoreChunk(
		options, postings, old, old.live, -1, "", nil,
	)
}

func (c *storeChunk) key(slot int) string {
	if c.keys != nil {
		return c.keys[slot]
	}
	return c.mappedKeys.keyAt(c.mappedBase, c.ord[slot])
}

func (s *Store) initLocked() (*storeState, error) {
	if state := s.state.Load(); state != nil {
		return state, nil
	}
	options, err := s.Options.normalized()
	if err != nil {
		return nil, err
	}
	s.options = options
	s.free.pos = make(map[uint32]int)
	state := &storeState{
		seed: maphash.MakeSeed(), options: options.stateOptions(),
	}
	s.state.Store(state)
	return state, nil
}

// Put validates src and atomically inserts or replaces key. It copies src and
// a newly inserted key; callers may reuse them after return. created reports
// whether key was absent.
//
// A failed validation leaves the Store and every Snapshot unchanged.
func (s *Store) Put(key string, src []byte) (created bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.initLocked()
	if err != nil {
		return false, err
	}
	hash := maphash.String(state.seed, key)
	old, loc, found := storeStateKeyLookupChunk(state, hash, key)
	if found {
		storedKey := old.key(int(loc.slot))
		var chunk *storeChunk
		if schema := s.options.Schema; schema != nil {
			chunk, err = rebuildStoreChunkSchema(
				state.options, schema,
				s.postingsRequiredLocked(), old, int(loc.slot),
				storedKey, src,
			)
		} else {
			chunk, err = rebuildStoreChunk(
				state.options, s.postingsRequiredLocked(), old,
				int(loc.slot), storedKey, src, true,
			)
		}
		if err != nil {
			return false, err
		}
		next := *state
		next.generation++
		next.detachMappedDocuments(old)
		next.chunks = state.chunks.set(loc.chunk, chunk)
		s.noteChunkPostingsLocked(loc.chunk, old, chunk)
		catalogChanged, secondaryChanged := s.noteIndexesForChunkLocked(loc.chunk, old, chunk, uint64(1)<<loc.slot)
		if catalogChanged {
			next.indexes = s.indexInfosLocked()
		}
		if secondaryChanged {
			next.secondary = s.indexSnapshotsLocked()
		}
		s.state.Store(&next)
		return false, nil
	}

	if len(s.free.ids) == 0 && state.chunks.count == ^uint32(0) {
		return false, ErrStoreTooLarge
	}
	chunkID, slot, old := s.allocateSlotLocked(state)
	var chunk *storeChunk
	if schema := s.options.Schema; schema != nil {
		chunk, err = rebuildStoreChunkSchema(
			state.options, schema, s.postingsRequiredLocked(),
			old, slot, key, src,
		)
	} else {
		chunk, err = rebuildStoreChunk(
			state.options, s.postingsRequiredLocked(), old,
			slot, key, src, true,
		)
	}
	if err != nil {
		return false, err
	}
	// Validation is complete and publication cannot fail. Only now take key
	// ownership, so malformed JSON and schema violations do not allocate a
	// transient key that is immediately discarded.
	key = strings.Clone(key)
	chunk.keys[slot] = key
	next := *state
	next.generation++
	next.count++
	next.detachMappedDocuments(old)
	loc = storeLocation{chunk: chunkID, slot: uint8(slot)}
	next.keys = storeKeyInsert(state.keys, hash, key, loc)
	if chunkID == state.chunks.count {
		next.chunks, _ = state.chunks.append(chunk)
	} else {
		next.chunks = state.chunks.set(chunkID, chunk)
	}
	if old == nil {
		next.chunkCount++
	}
	s.noteChunkPostingsLocked(chunkID, old, chunk)
	if int(chunk.count) == state.options.ChunkDocuments {
		s.removeFreeLocked(chunkID)
	} else {
		s.addFreeLocked(chunkID)
	}
	catalogChanged, secondaryChanged := s.noteIndexesForChunkLocked(chunkID, old, chunk, uint64(1)<<uint(slot))
	if catalogChanged {
		next.indexes = s.indexInfosLocked()
	}
	if secondaryChanged {
		next.secondary = s.indexSnapshotsLocked()
	}
	s.state.Store(&next)
	return true, nil
}

// Delete atomically removes key and reports whether it existed. The affected
// chunk is rebuilt without the document, so scans see a dense DocSet and the
// delete creates neither a tombstone nor future compaction work. Snapshots
// obtained before Delete remain valid and continue to see their old version.
func (s *Store) Delete(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteLocked(key)
}

func (s *Store) deleteLocked(key string) bool {
	state := s.state.Load()
	if state == nil {
		return false
	}
	hash := maphash.String(state.seed, key)
	old, loc, found := storeStateKeyLookupChunk(state, hash, key)
	if !found {
		return false
	}
	chunk, err := rebuildStoreChunk(
		state.options, s.postingsRequiredLocked(), old,
		int(loc.slot), "", nil, false,
	)
	if err != nil {
		panic("simdjson: rebuilding validated Store chunk: " + err.Error())
	}
	next := *state
	next.generation++
	next.count--
	next.detachMappedDocuments(old)
	next.keys = storeKeyDelete(state.keys, hash, key)
	next.chunks = state.chunks.set(loc.chunk, chunk)
	if chunk == nil {
		next.chunkCount--
	}
	s.noteChunkPostingsLocked(loc.chunk, old, chunk)
	s.addFreeLocked(loc.chunk)
	if s.ttl.remove(storeTTLKeyOf(loc)) {
		s.notifyExpiryLocked()
	}
	catalogChanged, secondaryChanged := s.noteIndexesForChunkLocked(loc.chunk, old, chunk, uint64(1)<<loc.slot)
	if catalogChanged {
		next.indexes = s.indexInfosLocked()
	}
	if secondaryChanged {
		next.secondary = s.indexSnapshotsLocked()
	}
	s.state.Store(&next)
	return true
}

func (s *storeState) detachMappedDocuments(chunk *storeChunk) {
	if s.mappedDocs == nil || chunk == nil || chunk.docs.mappedDocs != s.mappedDocs {
		return
	}
	s.detachMappedDocumentChunks(1)
}

func (s *storeState) detachMappedDocumentChunks(count uint32) {
	if count == 0 {
		return
	}
	if count > s.mappedDocChunks {
		panic("simdjson: mapped Store document count invariant")
	}
	s.mappedDocChunks -= count
	if s.mappedDocChunks == 0 {
		s.mappedDocs = nil
	}
}

func (s *Store) allocateSlotLocked(state *storeState) (uint32, int, *storeChunk) {
	if len(s.free.ids) == 0 {
		return state.chunks.count, 0, nil
	}
	id := s.free.ids[len(s.free.ids)-1]
	chunk := state.chunks.get(id)
	if chunk == nil {
		return id, 0, nil
	}
	limitMask := ^uint64(0)
	if state.options.ChunkDocuments < 64 {
		limitMask = uint64(1)<<uint(state.options.ChunkDocuments) - 1
	}
	free := ^chunk.live & limitMask
	if free == 0 {
		panic("simdjson: full Store chunk in free set")
	}
	return id, bits.TrailingZeros64(free), chunk
}

func (s *Store) addFreeLocked(id uint32) {
	s.free.add(id)
}

func (s *Store) removeFreeLocked(id uint32) {
	s.free.remove(id)
}

func (s *Store) noteChunkPostingsLocked(id uint32, old, next *storeChunk) {
	oldIndexed := old != nil && old.docs.Postings
	nextIndexed := next != nil && next.docs.Postings
	if oldIndexed == nextIndexed {
		return
	}
	if nextIndexed {
		s.postingChunks.add(id)
	} else {
		s.postingChunks.remove(id)
	}
}

// Snapshot returns the Store's current immutable view. It is O(1), never
// blocks a writer, and remains valid while later writes publish new views.
func (s *Store) Snapshot() Snapshot {
	return Snapshot{state: s.state.Load()}
}

// Len returns the number of keys in the current snapshot.
func (s *Store) Len() int {
	state := s.state.Load()
	if state == nil {
		return 0
	}
	return state.count
}

// Generation returns the monotonically increasing publication number. Zero is
// the empty initial state; every successful mutation publishes the next value.
func (s *Store) Generation() uint64 {
	state := s.state.Load()
	if state == nil {
		return 0
	}
	return state.generation
}

// A Snapshot is a logically immutable Store view. Its zero value is an empty
// snapshot. It is safe for concurrent use and remains valid independently of
// later Store mutations. GetRaw takes no lock, clock call, TTL branch, or
// allocation; Get may populate an equivalent memoized shape-tape widening.
type Snapshot struct {
	state *storeState
}

// Len returns the number of keys visible in s.
func (s Snapshot) Len() int {
	if s.state == nil {
		return 0
	}
	return s.state.count
}

// Generation returns the publication generation captured by s.
func (s Snapshot) Generation() uint64 {
	if s.state == nil {
		return 0
	}
	return s.state.generation
}

// GetRaw returns key's exact JSON bytes as a read-only borrowed RawValue.
func (s Snapshot) GetRaw(key string) (RawValue, bool) {
	if s.state == nil {
		return RawValue{}, false
	}
	hash := maphash.String(s.state.seed, key)
	// An untouched mapped directory has no heap overlay. Replacements keep a
	// base key at its stable slot and deletes clear the live bit; inserting any
	// non-base location creates the overlay. This dominant reopen/read path can
	// therefore avoid the general overlay router and a duplicate chunk walk.
	if s.state.baseKeys != nil && s.state.keys == nil {
		loc, ok := s.state.baseKeys.lookup(hash, key)
		if !ok {
			return RawValue{}, false
		}
		chunk := s.state.chunks.get(loc.chunk)
		if chunk == nil || chunk.live&(uint64(1)<<loc.slot) == 0 {
			return RawValue{}, false
		}
		return RawValue{src: chunk.docs.rawAt(int(chunk.ord[loc.slot]))}, true
	}
	chunk, loc, ok := storeStateKeyLookupChunk(s.state, hash, key)
	if !ok {
		return RawValue{}, false
	}
	return RawValue{src: chunk.docs.rawAt(int(chunk.ord[loc.slot]))}, true
}

// Get returns key's navigable Index. Shape-taped chunks may take their widening
// mutex and allocate once to memoize this document's equivalent classic tape,
// exactly like DocSet.Doc; GetRaw is the lock- and allocation-free path when
// exact JSON bytes are sufficient.
func (s Snapshot) Get(key string) (Index, bool) {
	if s.state == nil {
		return Index{}, false
	}
	hash := maphash.String(s.state.seed, key)
	if s.state.baseKeys != nil && s.state.keys == nil {
		loc, ok := s.state.baseKeys.lookup(hash, key)
		if !ok {
			return Index{}, false
		}
		chunk := s.state.chunks.get(loc.chunk)
		if chunk == nil || chunk.live&(uint64(1)<<loc.slot) == 0 {
			return Index{}, false
		}
		return chunk.docs.Doc(int(chunk.ord[loc.slot])), true
	}
	chunk, loc, ok := storeStateKeyLookupChunk(s.state, hash, key)
	if !ok {
		return Index{}, false
	}
	return chunk.docs.Doc(int(chunk.ord[loc.slot])), true
}

// Range visits live keys in stable chunk/slot order until fn returns false.
// Values borrow the Snapshot. Range itself allocates nothing.
func (s Snapshot) Range(fn func(key string, value RawValue) bool) {
	if s.state == nil {
		return
	}
	s.state.chunks.each(func(_ uint32, chunk *storeChunk) bool {
		for live := chunk.live; live != 0; live &= live - 1 {
			slot := bits.TrailingZeros64(live)
			if !fn(chunk.key(slot), RawValue{src: chunk.docs.rawAt(int(chunk.ord[slot]))}) {
				return false
			}
		}
		return true
	})
}

// GetRaw is the current-snapshot convenience form of Snapshot.GetRaw.
func (s *Store) GetRaw(key string) (RawValue, bool) { return s.Snapshot().GetRaw(key) }

// Get is the current-snapshot convenience form of Snapshot.Get.
func (s *Store) Get(key string) (Index, bool) { return s.Snapshot().Get(key) }

// postingsRequiredLocked includes online index builds in addition to the
// representation selected at construction. store_index.go supplies the
// dynamic half; this default keeps the core independent when no DDL exists.
func (s *Store) postingsRequiredLocked() bool {
	if s.options.Postings {
		return true
	}
	return s.hasPostingsIndexLocked()
}
