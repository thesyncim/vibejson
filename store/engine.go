package store

import (
	"errors"
	"fmt"
	"hash/maphash"
	"math/bits"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/thesyncim/vibejson"
	"github.com/thesyncim/vibejson/document"
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

// StoreStateOptions is the pointer-free subset copied into every immutable
// Store generation. Collection-wide schema belongs to Store, not to each
// publication; keeping it out of this value preserves the schemaless state
// size class and avoids bytes/op growth on every mutation.
type StoreStateOptions struct {
	IndexOptions   document.IndexOptions
	ChunkDocuments int
	ShapeTapes     bool
	Postings       bool
	ValueDict      bool
}

func (o StoreOptions) stateOptions() StoreStateOptions {
	return StoreStateOptions{
		IndexOptions: o.IndexOptions, ChunkDocuments: o.ChunkDocuments,
		ShapeTapes: o.ShapeTapes, Postings: o.Postings,
		ValueDict: o.ValueDict,
	}
}

const StoreMaxChunkDocuments = 64

// ErrStoreTooLarge reports that the persistent chunk address space is full.
// The limit is 2^32-1 chunks (at most 274 billion documents with the default
// chunk size), so reaching it indicates a caller architecture error rather
// than an ordinary capacity event. The guard prevents uint32 wraparound.
var ErrStoreTooLarge = errors.New("vibejson: Store chunk address space exhausted")

func maphashString(seed maphash.Seed, Key string) uint64 { return maphash.String(seed, Key) }

func (o StoreOptions) Normalized() (StoreOptions, error) {
	if o.ChunkDocuments == 0 {
		o.ChunkDocuments = StoreMaxChunkDocuments
	}
	if o.ChunkDocuments < 1 || o.ChunkDocuments > StoreMaxChunkDocuments {
		return StoreOptions{}, fmt.Errorf("vibejson: Store ChunkDocuments must be in [1,%d]", StoreMaxChunkDocuments)
	}
	if o.Schema != nil && !o.Schema.Valid() {
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
	state   atomic.Pointer[StoreState]
	options StoreOptions

	// Writer-only chunk-id sets make allocation and physical-index tracking
	// O(1). Empty chunk ids are reused, so insert/delete churn cannot grow the
	// chunk address space; reclamation takes indexed ids directly rather than
	// rescanning the entire vector after every bounded batch.
	free          storeIDSet
	postingChunks storeIDSet

	ttl           StoreTTLState
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

// WithBulkSnapshot runs fn with s's current StoreState (materializing an
// empty one from s.Options if the Store has never been written to) and its
// TTL state, holding s's writer lock for fn's duration. It exists so
// package store/durable can bulk-serialize a Store as a durable FileStore
// without exposing the mutex, atomic state pointer, or option-normalization
// internals that a direct field read would require.
func (s *Store) WithBulkSnapshot(fn func(state *StoreState, ttl *StoreTTLState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state.Load()
	if state == nil {
		normalized, err := s.Options.Normalized()
		if err != nil {
			return err
		}
		state = &StoreState{StateOptions: normalized.stateOptions()}
	}
	return fn(state, &s.ttl)
}

type StoreState struct {
	Generation uint64
	Count      int
	ChunkCount uint32
	// mappedDocChunks counts current Chunks that still borrow mappedDocs. A
	// rebuilt chunk becomes ordinary owned Go state; the last detach drops the
	// state-level owner while retained snapshots keep their own chunk owners.
	mappedDocChunks uint32
	seed            maphash.Seed
	StateOptions    StoreStateOptions
	keys            *storeKeyNode
	// baseKeys is the compact immutable directory created by StoreBuilder or
	// Open. keys is then only the path-copied overlay for later insertions
	// and moved keys.
	baseKeys   *storeMappedKeys
	mappedDocs *storeMappedDocs
	Chunks     storeChunkVector
	Indexes    []StoreIndexInfo
	secondary  []storeIndexSnapshot
	// source pins a Store image borrowed by Open. Ordinary heap-built
	// states leave it nil. Every path copy carries the slice, so mapped source
	// bytes remain reachable for the lifetime of current and retained snapshots;
	// the caller still owns when an underlying mapping is unmapped.
	source []byte
}

type StoreChunk struct {
	Docs       DocSet
	keys       []string
	keyBytes   []byte
	mappedKeys *storeMappedKeys
	mappedBase uint64
	Ord        [StoreMaxChunkDocuments]uint8
	Live       uint64
	Count      uint8
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
	options StoreStateOptions,
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
	options StoreStateOptions,
	postings bool,
	old *StoreChunk,
	live uint64,
	replaceSlot int,
) {
	initChunkDocSet(docs, options, postings)
	// Every surviving row is carried in by reference through appendStoreDoc;
	// only the replacement slot is parsed, so this set indexes at most one
	// document through buildDoc. The entry arena's growth headroom therefore
	// has no future writer and would be retained unwritten for the published
	// chunk's live tenure. The StoreBuilder shares initChunkDocSet but parses
	// every row of its page, so the flag belongs here rather than there.
	docs.singleAppend = true
	count := bits.OnesCount64(live)
	docs.docs = make([]vibejson.Index, 0, count)
	if !options.ShapeTapes {
		return
	}
	// A one-row replacement cannot reach the repeat-sighting gate, and has no
	// shape ref to store. Let commitDoc allocate lazily for the uncommon case
	// where a delete leaves one already-shaped survivor; this keeps the
	// ChunkDocuments=1 update path at its original allocation count.
	if count > 1 {
		docs.tapeRefs = make([]ShapeTapeRef, 0, count)
	}
	if old == nil {
		return
	}
	narrowCap := 0
	entryCap := 0
	for bitsLeft := live; bitsLeft != 0; bitsLeft &= bitsLeft - 1 {
		slot := bits.TrailingZeros64(bitsLeft)
		if slot == replaceSlot || old.Live&(uint64(1)<<uint(slot)) == 0 {
			continue
		}
		if template, ok := old.Docs.StoreTemplateAt(int(old.Ord[slot])); ok {
			entryCap += len(template.Index.Entries)
			continue
		}
		ref := old.Docs.ShapeTapeRefAt(int(old.Ord[slot]))
		if ref.Narrow {
			narrowCap += len(ref.Rec.Fields)
		}
		docs.shapes.seedRecord(ref.Rec)
	}
	if replaceSlot >= 0 {
		if old.Live&(uint64(1)<<uint(replaceSlot)) != 0 {
			oldOrd := int(old.Ord[replaceSlot])
			if template, ok := old.Docs.StoreTemplateAt(oldOrd); ok {
				entryCap += len(template.Index.Entries)
			} else {
				ref := old.Docs.ShapeTapeRefAt(oldOrd)
				if ref.Narrow {
					narrowCap += len(ref.Rec.Fields)
				}
			}
		} else if old.Count != 0 {
			// An insertion has no old row to size from. Reserve the current
			// average so a same-shape replacement—the common case when a freed
			// slot is reused—does not grow and recopy the whole narrow slab.
			narrowCap += old.Docs.narrowLen() / int(old.Count)
		}
	}
	if narrowCap != 0 {
		docs.narrow = make([]ShapeNarrowValue, 0, narrowCap)
	}
	if entryCap != 0 {
		docs.entryChunk = make([]vibejson.IndexEntry, 0, entryCap)
	}
}

// appendStoreDoc carries one validated immutable document into a new dense
// DocSet without copying its source or rebuilding its structural tape. Narrow
// shape values are the sole set-relative storage: copy them into the new slab
// and rewrite the private offset before commit. commitDoc then rebuilds any
// enabled chunk-local postings or value dictionary against the new ordinal.
func appendStoreDoc(dst *DocSet, old *DocSet, oldOrd int) int {
	if template, ok := old.StoreTemplateAt(oldOrd); ok {
		used := len(dst.entryChunk)
		dst.entryChunk = old.synthStoreTemplate(oldOrd, template, dst.entryChunk)
		index := vibejson.Index{Src: old.DocAt(oldOrd).Src, Entries: dst.entryChunk[used:]}
		return dst.commitDoc(index, ShapeTapeRef{})
	}
	index := old.DocAt(oldOrd)
	ref := old.ShapeTapeRefAt(oldOrd)
	promoted := false
	if ref.Rec == nil && dst.ShapeTapes {
		index, ref, promoted = copyStoreShapeTape(dst, index)
	}
	if ref.Narrow && !promoted {
		n := uint32(len(ref.Rec.Fields))
		oldRef := ref
		ref.off = uint32(len(dst.narrow))
		for i := uint32(0); i < n; i++ {
			dst.narrow = append(dst.narrow, old.NarrowAt(oldOrd, oldRef, int(i)))
		}
	}
	return dst.commitDoc(index, ref)
}

// copyStoreShapeTape promotes one reused classic flat object into the new
// chunk's compact representation without modifying its immutable old tape.
// Resolve preserves the ordinary repeat-sighting economics, and the exact key
// comparison preserves shapeTapeCompact's collision-proof trust boundary.
func copyStoreShapeTape(dst *DocSet, index vibejson.Index) (vibejson.Index, ShapeTapeRef, bool) {
	entries := index.Entries
	if len(entries) == 0 {
		return index, ShapeTapeRef{}, false
	}
	root := &entries[0]
	count := int(root.Count())
	if root.Kind() != document.Object || count == 0 {
		return index, ShapeTapeRef{}, false
	}
	shape, ok := dst.shapes.Resolve(vibejson.NodeFromEntries(index.Src, entries))
	if !ok || shape.rec.dupKeys {
		return index, ShapeTapeRef{}, false
	}
	rec := shape.rec
	if !shapeTapeConforms(index, rec) {
		return index, ShapeTapeRef{}, false
	}
	ref := ShapeTapeRef{
		Rec:      rec,
		Start:    root.Start,
		End:      root.End,
		enriched: root.KeysHashed(),
	}
	if root.End <= ShapeNarrowMaxEnd && !dst.wideValueTapes &&
		uint64(len(dst.narrow))+uint64(count) <= uint64(^uint32(0)) {
		ref.Narrow = true
		ref.off = dst.appendNarrowShapeValues(entries, count)
		return vibejson.Index{Src: index.Src}, ref, true
	}
	values := make([]vibejson.IndexEntry, count)
	for m := range values {
		values[m] = entries[2*m+2]
	}
	return vibejson.Index{Src: index.Src, Entries: values}, ref, true
}

// buildStoreChunk is the single bounded rebuild primitive used by inserts,
// replacements, deletes, expiry batches, index backfill, and index reclaim.
// live is the exact post-edit slot mask. replaceSlot selects one slot whose
// bytes come from src; -1 means every remaining document comes from old.
func buildStoreChunk(
	options StoreStateOptions,
	postings bool,
	old *StoreChunk,
	live uint64,
	replaceSlot int,
	key string,
	src []byte,
) (*StoreChunk, error) {
	if live == 0 {
		// Deleting or expiring the final row removes the vector leaf outright.
		// There is no replacement to validate and no empty chunk object to
		// publish, so avoid constructing tables that would be discarded below.
		return nil, nil
	}
	chunk := &StoreChunk{
		keys: make([]string, options.ChunkDocuments),
	}
	prepareStoreDocSet(&chunk.Docs, options, postings, old, live, replaceSlot)
	if old != nil {
		for bitsLeft := old.Live; bitsLeft != 0; bitsLeft &= bitsLeft - 1 {
			slot := bits.TrailingZeros64(bitsLeft)
			chunk.keys[slot] = old.Key(slot)
		}
	}
	chunk.Live = live
	if old != nil {
		for removed := old.Live &^ live; removed != 0; removed &= removed - 1 {
			chunk.keys[bits.TrailingZeros64(removed)] = ""
		}
	}
	if replaceSlot >= 0 {
		chunk.keys[replaceSlot] = key
	}
	for bitsLeft := chunk.Live; bitsLeft != 0; bitsLeft &= bitsLeft - 1 {
		i := bits.TrailingZeros64(bitsLeft)
		var ord int
		if i == replaceSlot {
			var err error
			ord, err = chunk.Docs.Append(src)
			if err != nil {
				return nil, err
			}
		} else {
			ord = appendStoreDoc(&chunk.Docs, &old.Docs, int(old.Ord[i]))
		}
		chunk.Ord[i] = uint8(ord)
		chunk.Count++
	}
	if chunk.Count == 0 {
		return nil, nil
	}
	// The chunk is complete and about to be published immutable: no further
	// document can be indexed into it, so its ingest-only working state is
	// released before it acquires the lifetime of the generation holding it.
	chunk.Docs.sealIngest()
	return chunk, nil
}

// buildStoreChunkSchema is the schema-on specialization of buildStoreChunk.
// Keeping the replacement append explicit gives the much more common
// schemaless build a direct DocSet.Append call; a callback or inner-loop mode
// branch would add work to every schemaless mutation.
func buildStoreChunkSchema(
	options StoreStateOptions,
	schema *StoreSchema,
	postings bool,
	old *StoreChunk,
	live uint64,
	replaceSlot int,
	key string,
	src []byte,
) (*StoreChunk, error) {
	if live == 0 {
		return nil, nil
	}
	chunk := &StoreChunk{
		keys: make([]string, options.ChunkDocuments),
	}
	prepareStoreDocSet(
		&chunk.Docs, options, postings, old, live, replaceSlot,
	)
	if old != nil {
		for bitsLeft := old.Live; bitsLeft != 0; bitsLeft &= bitsLeft - 1 {
			slot := bits.TrailingZeros64(bitsLeft)
			chunk.keys[slot] = old.Key(slot)
		}
	}
	chunk.Live = live
	if old != nil {
		for removed := old.Live &^ live; removed != 0; removed &= removed - 1 {
			chunk.keys[bits.TrailingZeros64(removed)] = ""
		}
	}
	if replaceSlot >= 0 {
		chunk.keys[replaceSlot] = key
	}
	for bitsLeft := chunk.Live; bitsLeft != 0; bitsLeft &= bitsLeft - 1 {
		i := bits.TrailingZeros64(bitsLeft)
		var ord int
		if i == replaceSlot {
			var err error
			ord, err = chunk.Docs.appendStoreSchema(src, schema)
			if err != nil {
				return nil, err
			}
		} else {
			ord = appendStoreDoc(
				&chunk.Docs, &old.Docs, int(old.Ord[i]),
			)
		}
		chunk.Ord[i] = uint8(ord)
		chunk.Count++
	}
	if chunk.Count == 0 {
		return nil, nil
	}
	chunk.Docs.sealIngest()
	return chunk, nil
}

func rebuildStoreChunk(
	options StoreStateOptions,
	postings bool,
	old *StoreChunk,
	slot int,
	key string,
	src []byte,
	keep bool,
) (*StoreChunk, error) {
	var live uint64
	if old != nil {
		live = old.Live
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
	options StoreStateOptions,
	schema *StoreSchema,
	postings bool,
	old *StoreChunk,
	slot int,
	key string,
	src []byte,
) (*StoreChunk, error) {
	var live uint64
	if old != nil {
		live = old.Live
	}
	live |= uint64(1) << uint(slot)
	return buildStoreChunkSchema(
		options, schema, postings, old, live, slot, key, src,
	)
}

func cloneStoreChunk(
	options StoreStateOptions,
	postings bool,
	old *StoreChunk,
) (*StoreChunk, error) {
	if old == nil {
		return nil, nil
	}
	return buildStoreChunk(
		options, postings, old, old.Live, -1, "", nil,
	)
}

func (c *StoreChunk) Key(slot int) string {
	if c.keys != nil {
		return c.keys[slot]
	}
	return c.mappedKeys.keyAt(c.mappedBase, c.Ord[slot])
}

func (s *Store) initLocked() (*StoreState, error) {
	if state := s.state.Load(); state != nil {
		return state, nil
	}
	options, err := s.Options.Normalized()
	if err != nil {
		return nil, err
	}
	s.options = options
	s.free.pos = make(map[uint32]int)
	state := &StoreState{
		seed: maphash.MakeSeed(), StateOptions: options.stateOptions(),
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
		storedKey := old.Key(int(loc.Slot))
		var chunk *StoreChunk
		if schema := s.options.Schema; schema != nil {
			chunk, err = rebuildStoreChunkSchema(
				state.StateOptions, schema,
				s.postingsRequiredLocked(), old, int(loc.Slot),
				storedKey, src,
			)
		} else {
			chunk, err = rebuildStoreChunk(
				state.StateOptions, s.postingsRequiredLocked(), old,
				int(loc.Slot), storedKey, src, true,
			)
		}
		if err != nil {
			return false, err
		}
		next := *state
		next.Generation++
		next.detachMappedDocuments(old)
		next.Chunks = state.Chunks.set(loc.Chunk, chunk)
		s.noteChunkPostingsLocked(loc.Chunk, old, chunk)
		catalogChanged, secondaryChanged := s.noteIndexesForChunkLocked(loc.Chunk, old, chunk, uint64(1)<<loc.Slot)
		if catalogChanged {
			next.Indexes = s.indexInfosLocked()
		}
		if secondaryChanged {
			next.secondary = s.indexSnapshotsLocked()
		}
		s.state.Store(&next)
		return false, nil
	}

	if len(s.free.ids) == 0 && state.Chunks.Count == ^uint32(0) {
		return false, ErrStoreTooLarge
	}
	chunkID, slot, old := s.allocateSlotLocked(state)
	var chunk *StoreChunk
	if schema := s.options.Schema; schema != nil {
		chunk, err = rebuildStoreChunkSchema(
			state.StateOptions, schema, s.postingsRequiredLocked(),
			old, slot, key, src,
		)
	} else {
		chunk, err = rebuildStoreChunk(
			state.StateOptions, s.postingsRequiredLocked(), old,
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
	next.Generation++
	next.Count++
	next.detachMappedDocuments(old)
	loc = StoreLocation{Chunk: chunkID, Slot: uint8(slot)}
	next.keys = storeKeyInsert(state.keys, hash, key, loc)
	if chunkID == state.Chunks.Count {
		next.Chunks, _ = state.Chunks.append(chunk)
	} else {
		next.Chunks = state.Chunks.set(chunkID, chunk)
	}
	if old == nil {
		next.ChunkCount++
	}
	s.noteChunkPostingsLocked(chunkID, old, chunk)
	if int(chunk.Count) == state.StateOptions.ChunkDocuments {
		s.removeFreeLocked(chunkID)
	} else {
		s.addFreeLocked(chunkID)
	}
	catalogChanged, secondaryChanged := s.noteIndexesForChunkLocked(chunkID, old, chunk, uint64(1)<<uint(slot))
	if catalogChanged {
		next.Indexes = s.indexInfosLocked()
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
// The error return always reports nil; it exists so Store satisfies the same
// [Table] shape as durable.Store, whose Delete can fail on I/O.
func (s *Store) Delete(key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteLocked(key), nil
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
		state.StateOptions, s.postingsRequiredLocked(), old,
		int(loc.Slot), "", nil, false,
	)
	if err != nil {
		panic("vibejson: rebuilding validated Store chunk: " + err.Error())
	}
	next := *state
	next.Generation++
	next.Count--
	next.detachMappedDocuments(old)
	next.keys = storeKeyDelete(state.keys, hash, key)
	next.Chunks = state.Chunks.set(loc.Chunk, chunk)
	if chunk == nil {
		next.ChunkCount--
	}
	s.noteChunkPostingsLocked(loc.Chunk, old, chunk)
	s.addFreeLocked(loc.Chunk)
	if s.ttl.remove(StoreTTLKeyOf(loc)) {
		s.notifyExpiryLocked()
	}
	catalogChanged, secondaryChanged := s.noteIndexesForChunkLocked(loc.Chunk, old, chunk, uint64(1)<<loc.Slot)
	if catalogChanged {
		next.Indexes = s.indexInfosLocked()
	}
	if secondaryChanged {
		next.secondary = s.indexSnapshotsLocked()
	}
	s.state.Store(&next)
	return true
}

func (s *StoreState) detachMappedDocuments(chunk *StoreChunk) {
	if s.mappedDocs == nil || chunk == nil || chunk.Docs.mappedDocs != s.mappedDocs {
		return
	}
	s.detachMappedDocumentChunks(1)
}

func (s *StoreState) detachMappedDocumentChunks(count uint32) {
	if count == 0 {
		return
	}
	if count > s.mappedDocChunks {
		panic("vibejson: mapped Store document count invariant")
	}
	s.mappedDocChunks -= count
	if s.mappedDocChunks == 0 {
		s.mappedDocs = nil
	}
}

func (s *Store) allocateSlotLocked(state *StoreState) (uint32, int, *StoreChunk) {
	if len(s.free.ids) == 0 {
		return state.Chunks.Count, 0, nil
	}
	id := s.free.ids[len(s.free.ids)-1]
	chunk := state.Chunks.Get(id)
	if chunk == nil {
		return id, 0, nil
	}
	limitMask := ^uint64(0)
	if state.StateOptions.ChunkDocuments < 64 {
		limitMask = uint64(1)<<uint(state.StateOptions.ChunkDocuments) - 1
	}
	free := ^chunk.Live & limitMask
	if free == 0 {
		panic("vibejson: full Store chunk in free set")
	}
	return id, bits.TrailingZeros64(free), chunk
}

func (s *Store) addFreeLocked(id uint32) {
	s.free.add(id)
}

func (s *Store) removeFreeLocked(id uint32) {
	s.free.remove(id)
}

func (s *Store) noteChunkPostingsLocked(id uint32, old, next *StoreChunk) {
	oldIndexed := old != nil && old.Docs.Postings
	nextIndexed := next != nil && next.Docs.Postings
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
// The error return always reports nil; it exists so Store satisfies the same
// [Table] shape as durable.Store, whose Snapshot can fail on I/O.
func (s *Store) Snapshot() (Snapshot, error) {
	return Snapshot{state: s.state.Load()}, nil
}

// Len returns the number of keys in the current snapshot.
func (s *Store) Len() uint64 {
	state := s.state.Load()
	if state == nil {
		return 0
	}
	return uint64(state.Count)
}

// Generation returns the monotonically increasing publication number. Zero is
// the empty initial state; every successful mutation publishes the next value.
func (s *Store) Generation() uint64 {
	state := s.state.Load()
	if state == nil {
		return 0
	}
	return state.Generation
}

// A Snapshot is a logically immutable Store view. Its zero value is an empty
// snapshot. It is safe for concurrent use and remains valid independently of
// later Store mutations. GetRaw takes no lock, clock call, TTL branch, or
// allocation; Get may populate an equivalent memoized shape-tape widening.
type Snapshot struct {
	state *StoreState
}

// Len returns the number of keys visible in s.
func (s Snapshot) Len() int {
	if s.state == nil {
		return 0
	}
	return s.state.Count
}

// Generation returns the publication generation captured by s.
func (s Snapshot) Generation() uint64 {
	if s.state == nil {
		return 0
	}
	return s.state.Generation
}

// GetRaw returns key's exact JSON bytes as a read-only borrowed RawValue.
func (s Snapshot) GetRaw(key string) (vibejson.RawValue, bool) {
	if s.state == nil {
		return vibejson.RawValue{}, false
	}
	hash := maphash.String(s.state.seed, key)
	// An untouched mapped directory has no heap overlay. Replacements keep a
	// base key at its stable slot and deletes clear the live bit; inserting any
	// non-base location creates the overlay. This dominant reopen/read path can
	// therefore avoid the general overlay router and a duplicate chunk walk.
	if s.state.baseKeys != nil && s.state.keys == nil {
		loc, ok := s.state.baseKeys.lookup(hash, key)
		if !ok {
			return vibejson.RawValue{}, false
		}
		chunk := s.state.Chunks.Get(loc.Chunk)
		if chunk == nil || chunk.Live&(uint64(1)<<loc.Slot) == 0 {
			return vibejson.RawValue{}, false
		}
		return vibejson.RawValue{Src: chunk.Docs.RawAt(int(chunk.Ord[loc.Slot]))}, true
	}
	chunk, loc, ok := storeStateKeyLookupChunk(s.state, hash, key)
	if !ok {
		return vibejson.RawValue{}, false
	}
	return vibejson.RawValue{Src: chunk.Docs.RawAt(int(chunk.Ord[loc.Slot]))}, true
}

// Get returns key's navigable Index. Shape-taped chunks may take their widening
// mutex and allocate once to memoize this document's equivalent classic tape,
// exactly like DocSet.Doc; GetRaw is the lock- and allocation-free path when
// exact JSON bytes are sufficient.
func (s Snapshot) Get(key string) (vibejson.Index, bool) {
	if s.state == nil {
		return vibejson.Index{}, false
	}
	hash := maphash.String(s.state.seed, key)
	if s.state.baseKeys != nil && s.state.keys == nil {
		loc, ok := s.state.baseKeys.lookup(hash, key)
		if !ok {
			return vibejson.Index{}, false
		}
		chunk := s.state.Chunks.Get(loc.Chunk)
		if chunk == nil || chunk.Live&(uint64(1)<<loc.Slot) == 0 {
			return vibejson.Index{}, false
		}
		return chunk.Docs.Doc(int(chunk.Ord[loc.Slot])), true
	}
	chunk, loc, ok := storeStateKeyLookupChunk(s.state, hash, key)
	if !ok {
		return vibejson.Index{}, false
	}
	return chunk.Docs.Doc(int(chunk.Ord[loc.Slot])), true
}

// Range visits live keys in stable chunk/slot order until fn returns false.
// Values borrow the Snapshot. Range itself allocates nothing.
func (s Snapshot) Range(fn func(key string, value vibejson.RawValue) bool) {
	if s.state == nil {
		return
	}
	s.state.Chunks.Each(func(_ uint32, chunk *StoreChunk) bool {
		for live := chunk.Live; live != 0; live &= live - 1 {
			slot := bits.TrailingZeros64(live)
			if !fn(chunk.Key(slot), vibejson.RawValue{Src: chunk.Docs.RawAt(int(chunk.Ord[slot]))}) {
				return false
			}
		}
		return true
	})
}

// GetRaw is the current-snapshot convenience form of Snapshot.GetRaw.
func (s *Store) GetRaw(key string) (vibejson.RawValue, bool) {
	snap, _ := s.Snapshot()
	return snap.GetRaw(key)
}

// Get is the current-snapshot convenience form of Snapshot.Get.
func (s *Store) Get(key string) (vibejson.Index, bool) {
	snap, _ := s.Snapshot()
	return snap.Get(key)
}

// postingsRequiredLocked includes online index builds in addition to the
// representation selected at construction. store_index.go supplies the
// dynamic half; this default keeps the core independent when no DDL exists.
func (s *Store) postingsRequiredLocked() bool {
	if s.options.Postings {
		return true
	}
	return s.hasPostingsIndexLocked()
}
