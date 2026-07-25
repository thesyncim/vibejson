package store

import (
	"errors"
	"fmt"
	"hash/maphash"
	"math/bits"
	"strings"

	"github.com/thesyncim/vibejson/internal/byteview"
)

var (
	// ErrStoreDuplicateKey reports that StoreBuilder.Append received a key it
	// already owns. Bulk construction requires unique keys so every document is
	// written exactly once into its final micro-page.
	ErrStoreDuplicateKey = errors.New("vibejson: duplicate StoreBuilder key")
	// ErrStoreBuilderClosed reports use after Build transferred the builder's
	// immutable graph into a Store.
	ErrStoreBuilderClosed = errors.New("vibejson: StoreBuilder is closed")
)

// StoreBuilder constructs a keyed Store without publishing and path-copying
// persistent metadata for every input row. It is the bulk-load complement to
// Store.Put: Append validates and copies one unique key/document into its final
// bounded micro-page, mutating only builder-owned key and chunk radix nodes;
// Build freezes that graph and publishes it once.
//
// A builder belongs to one goroutine. Append errors leave all previously
// appended rows intact and do not consume a key or slot. Build may be called
// once; the returned Store has ordinary snapshot, mutation, TTL, and index
// semantics. CreateIndex can include ready nested or compound indexes in the
// same publication. StoreBuilder is intentionally not an update API: online
// changes belong to Store.Put.
type StoreBuilder struct {
	options  StoreOptions
	seed     maphash.Seed
	keyTable storeBuilderKeyTable
	chunks   storeChunkVector
	current  *StoreChunk
	count    int
	keyBytes int
	closed   bool
	exact    map[string]*StoreExactIndex
	shapes   []*ShapeRecord
	shapeSet map[*ShapeRecord]struct{}
	// sourceHint is the exact JSON bytes in the preceding full chunk. It lets
	// the next unpublished chunk reserve one bounded arena instead of retaining
	// every geometric growth generation through already-built Index slices.
	sourceHint      int
	currentDocBytes int
}

// NewBuilder returns an empty bulk builder. It validates StoreOptions up
// front so Append cannot discover a configuration error after consuming rows.
func NewBuilder(options StoreOptions) (*StoreBuilder, error) {
	normalized, err := options.Normalized()
	if err != nil {
		return nil, err
	}
	return &StoreBuilder{options: normalized, seed: maphash.MakeSeed()}, nil
}

// Len returns the number of documents successfully appended so far.
func (b *StoreBuilder) Len() int {
	if b == nil {
		return 0
	}
	return b.count
}

// CreateIndex declares a single-column or compound exact index to build inside
// the unpublished transaction. Paths have the same nested RFC 6901 semantics
// as [Store.CreateIndex]. Build returns the index Ready: it extracts and sorts
// page-local tuples, constructs immutable stable-slot postings in bulk, and
// publishes the documents, key directory, and index roots together.
//
// A declaration may be added before or after Append calls. Invalid or duplicate
// declarations leave the builder and all appended rows unchanged.
func (b *StoreBuilder) CreateIndex(def StoreIndexDefinition) error {
	if b == nil || b.closed {
		return ErrStoreBuilderClosed
	}
	exact, err := CompileStoreExactIndex(def)
	if err != nil {
		return err
	}
	if b.exact != nil {
		if _, exists := b.exact[def.Name]; exists {
			return ErrStoreIndexExists
		}
	} else {
		b.exact = make(map[string]*StoreExactIndex)
	}
	name := strings.Clone(def.Name)
	exact.seed = b.seed
	b.exact[name] = exact
	return nil
}

// Append validates and copies one unique keyed document. The caller may reuse
// key and src after return. A duplicate key, invalid JSON, closed builder, or
// exhausted chunk address space changes no committed row.
func (b *StoreBuilder) Append(key string, src []byte) error {
	if b == nil || b.closed {
		return ErrStoreBuilderClosed
	}
	if uint64(len(key)) > uint64(^uint32(0)) || len(key) > MaxInt()-b.keyBytes {
		return ErrStorePersistTooLarge
	}
	hash := maphash.String(b.seed, key)
	if b.keyTable.contains(b, hash, key) {
		return fmt.Errorf("%w %q", ErrStoreDuplicateKey, key)
	}
	if uint64(b.count) >= storeBuilderKeyOrdinalMask {
		return ErrStoreTooLarge
	}
	if b.current == nil {
		if b.chunks.Count == ^uint32(0) {
			return ErrStoreTooLarge
		}
		capacity := storeBuilderSourceCapacity(b.options.ChunkDocuments, len(src), b.sourceHint)
		b.current = newStoreBuilderChunk(b.options, b.shapes, capacity)
	}

	// DocSet.Append owns and validates src before any key or directory state is
	// committed. Its rollback contract leaves the page unchanged on error.
	var (
		ord int
		err error
	)
	if b.options.Schema == nil {
		ord, err = b.current.Docs.Append(src)
	} else {
		ord, err = b.current.Docs.appendStoreSchema(
			src, b.options.Schema,
		)
	}
	if err != nil {
		return err
	}
	// Grow before publishing the new key into the table. Rehashing sees only
	// preceding rows, and every subsequent operation is bounded and infallible.
	b.keyTable.reserve(b, b.count+1)
	b.currentDocBytes += len(src)
	keyStart := len(b.current.keyBytes)
	b.current.keyBytes = append(b.current.keyBytes, key...)
	storedKey := byteview.String(b.current.keyBytes[keyStart:])
	b.keyBytes += len(key)
	slot := int(b.current.Count)
	b.current.keys[slot] = storedKey
	b.current.Ord[slot] = uint8(ord)
	b.current.Live |= uint64(1) << uint(slot)
	b.current.Count++
	b.keyTable.insert(hash, uint64(b.count))
	b.count++

	if int(b.current.Count) == b.options.ChunkDocuments {
		b.flush()
	}
	return nil
}

func newStoreBuilderChunk(options StoreOptions, shapes []*ShapeRecord, sourceCapacity int) *StoreChunk {
	chunk := &StoreChunk{
		keys: make([]string, options.ChunkDocuments),
	}
	initChunkDocSet(
		&chunk.Docs, options.stateOptions(), options.Postings,
	)
	if sourceCapacity > 0 {
		chunk.Docs.srcChunk = make([]byte, 0, sourceCapacity)
	}
	for _, rec := range shapes {
		chunk.Docs.shapes.seedRecord(rec)
	}
	return chunk
}

func storeBuilderSourceCapacity(chunkDocuments, firstDocumentBytes, previousBytes int) int {
	if chunkDocuments <= 0 || firstDocumentBytes <= 0 {
		return 0
	}
	sample := docSetMaxSrcChunk
	if firstDocumentBytes <= docSetMaxSrcChunk/chunkDocuments {
		sample = firstDocumentBytes * chunkDocuments
	}
	if previousBytes <= 0 {
		return storeBuilderSourceHeadroom(sample, chunkDocuments)
	}
	previousBytes = min(previousBytes, docSetMaxSrcChunk)
	average := (previousBytes + chunkDocuments - 1) / chunkDocuments
	// Reuse the exact preceding-page size only while the new first row is a
	// plausible member of the same size distribution. A phase change switches
	// immediately to the current sample instead of pinning stale capacity.
	if firstDocumentBytes >= max(average/2, 1) && firstDocumentBytes <= average*2 {
		return storeBuilderSourceHeadroom(previousBytes, chunkDocuments)
	}
	return storeBuilderSourceHeadroom(sample, chunkDocuments)
}

func storeBuilderSourceHeadroom(size, chunkDocuments int) int {
	if size >= docSetMaxSrcChunk {
		return docSetMaxSrcChunk
	}
	if chunkDocuments <= 1 {
		return size
	}
	// One average row absorbs ordinary page-to-page variance without the
	// 2x retained cost of crossing an arena boundary by a few bytes.
	headroom := max(size/chunkDocuments, 256)
	if size > docSetMaxSrcChunk-headroom {
		return docSetMaxSrcChunk
	}
	return size + headroom
}

func (b *StoreBuilder) flush() {
	if b.options.ShapeTapes {
		compactStoreBuilderShapes(&b.current.Docs)
		if b.shapeSet == nil {
			b.shapeSet = make(map[*ShapeRecord]struct{})
		}
		for _, rec := range b.current.Docs.shapes.shapes {
			if _, exists := b.shapeSet[rec]; exists {
				continue
			}
			b.shapeSet[rec] = struct{}{}
			b.shapes = append(b.shapes, rec)
		}
	}
	b.chunks.appendTransient(b.current)
	if int(b.current.Count) == b.options.ChunkDocuments {
		b.sourceHint = b.currentDocBytes
	}
	b.currentDocBytes = 0
	b.current = nil
}

// compactStoreBuilderShapes revisits the first sighting of every shape after
// its page-local repeat has compiled the immutable record. Ordinary DocSet
// append cannot rewrite an already returned Index, but an unpublished builder
// owns every row and can safely drop those redundant classic key tapes before
// publication. Value postings and dictionaries remain exact because the
// document bytes do not change. Key-existence postings also encode the
// document's physical storage class, so a successful transition moves its
// ordinal from the classic remainder to the compiled shape.
func compactStoreBuilderShapes(docs *DocSet) {
	if docs == nil || len(docs.shapes.shapes) == 0 || len(docs.tapeRefs) == 0 {
		return
	}
	allCompact := true
	for i := range docs.docs {
		if docs.ShapeTapeRefAt(i).Rec != nil {
			continue
		}
		index, ref, ok := copyStoreShapeTape(docs, docs.docs[i])
		if !ok {
			allCompact = false
			continue
		}
		docs.docs[i] = index
		docs.tapeRefs[i] = ref
		if docs.postings != nil {
			docs.postings.promoteShapeDoc(ref.Rec, int32(i))
		}
	}
	if allCompact {
		docs.entryChunk = nil
		docs.scratch = nil
	}
}

// Build freezes the accumulated graph, compacts immutable keys and document
// source/tapes into pointer-free owned blocks, and transfers them to a new
// Store. Empty input produces an initialized empty Store. The builder closes
// even when it is empty, preventing accidental aliasing through later Append
// calls. Once key compaction succeeds, Build is terminal: any later index or
// document-compaction failure releases all unpublished external blocks and a
// subsequent call returns ErrStoreBuilderClosed.
func (b *StoreBuilder) Build() (*Store, error) {
	return b.build(storeBuilderBuildSteps{
		buildExactIndexes: (*StoreBuilder).buildExactIndexes,
		compactDocuments:  (*StoreBuilder).compactDocuments,
	})
}

// storeBuilderBuildSteps keeps failure injection local to one unpublished
// builder. Tests can stop after a specific ownership transfer without a
// process-wide hook that could race an unrelated Build.
type storeBuilderBuildSteps struct {
	buildExactIndexes func(*StoreBuilder, *Store, *StoreState) error
	compactDocuments  func(*StoreBuilder, *StoreState) error
}

func (b *StoreBuilder) build(steps storeBuilderBuildSteps) (*Store, error) {
	if b == nil || b.closed {
		return nil, ErrStoreBuilderClosed
	}
	if b.current != nil {
		b.flush()
	}
	baseKeys, err := b.compactBaseKeys()
	if err != nil {
		return nil, err
	}
	b.closed = true

	state := &StoreState{
		Count:        b.count,
		ChunkCount:   b.chunks.Count,
		seed:         b.seed,
		StateOptions: b.options.stateOptions(),
		baseKeys:     baseKeys,
		Chunks:       b.chunks,
	}
	if b.count != 0 || len(b.exact) != 0 {
		state.Generation = 1
	}
	store := &Store{Options: b.options, options: b.options}
	store.free.pos = make(map[uint32]int)
	store.postingChunks.pos = make(map[uint32]int)
	for id := uint32(0); id < b.chunks.Count; id++ {
		chunk := b.chunks.Get(id)
		if chunk != nil && int(chunk.Count) < b.options.ChunkDocuments {
			store.free.add(id)
		}
		if chunk != nil && chunk.Docs.Postings {
			store.postingChunks.add(id)
		}
	}
	if err := steps.buildExactIndexes(b, store, state); err != nil {
		releaseStoreBuilderBuild(state, store)
		return nil, err
	}
	if err := steps.compactDocuments(b, state); err != nil {
		releaseStoreBuilderBuild(state, store)
		return nil, err
	}
	store.state.Store(state)
	b.keyTable = storeBuilderKeyTable{}
	b.chunks = storeChunkVector{}
	b.current = nil
	b.exact = nil
	b.shapes = nil
	b.shapeSet = nil
	b.sourceHint = 0
	b.currentDocBytes = 0
	return store, nil
}

// releaseStoreBuilderBuild closes every external block that can become owned
// after key compaction but before the new Store is published. A failed Build
// is terminal once compactBaseKeys has rewritten the chunks, so no subsequent
// builder operation can observe these released unpublished representations.
func releaseStoreBuilderBuild(state *StoreState, store *Store) {
	if store != nil {
		for _, index := range store.indexes {
			if index != nil && index.base != nil {
				index.base.release()
				index.base = nil
			}
		}
	}
	if state == nil {
		return
	}
	if state.mappedDocs != nil {
		state.mappedDocs.release()
		state.mappedDocs = nil
	}
	if state.baseKeys != nil {
		state.baseKeys.release()
		state.baseKeys = nil
	}
}

// compactBaseKeys replaces the builder-only HAMT and its leaf objects with one
// immutable Swiss-style table plus packed key bytes. On common Unix systems
// both regions are outside the Go heap. The published Store therefore retains
// neither a string allocation nor a directory leaf per input row.
func (b *StoreBuilder) compactBaseKeys() (*storeMappedKeys, error) {
	if b.count == 0 {
		return nil, nil
	}
	base, err := newStoreOwnedKeys(b.count, b.keyBytes, b.chunks.Count >= storeMappedLocationMaxChunk, b.options.ChunkDocuments)
	if err != nil {
		return nil, fmt.Errorf("vibejson: compact StoreBuilder keys: %w", err)
	}
	position := 0
	refBase := uint64(0)
	valid := true
	b.chunks.Each(func(id uint32, chunk *StoreChunk) bool {
		for live := chunk.Live; live != 0; live &= live - 1 {
			slot := bits.TrailingZeros64(live)
			key := chunk.keys[slot]
			start := position
			position += copy(base.source[position:], key)
			ref := refBase + uint64(chunk.Ord[slot])
			if ref >= uint64(base.keyRefCount()) {
				valid = false
				return false
			}
			base.setKeySpan(ref, uint64(start), uint32(len(key)))
			base.setLocation(ref, StoreLocation{Chunk: id, Slot: uint8(slot)})
			if !base.insert(maphash.String(b.seed, key), ref) {
				valid = false
				return false
			}
		}
		refBase += uint64(chunk.Count)
		return true
	})
	if !valid || position != len(base.source) || refBase != uint64(b.count) {
		base.release()
		return nil, errors.New("vibejson: StoreBuilder compact key invariant")
	}
	refBase = 0
	b.chunks.Each(func(_ uint32, chunk *StoreChunk) bool {
		chunk.keys = nil
		chunk.keyBytes = nil
		chunk.mappedKeys = base
		chunk.mappedBase = refBase
		refBase += uint64(chunk.Count)
		return true
	})
	return base, nil
}

// buildExactIndexes constructs complete roots while store and state are still
// unreachable by readers. storeIndexCollectChunk coalesces equal tuples inside
// each page; radix traversal supplies ascending chunk ids, so every posting's
// masks are already in the order required by the packed-page builder.
func (b *StoreBuilder) buildExactIndexes(store *Store, state *StoreState) error {
	if len(b.exact) == 0 {
		return nil
	}
	if store.indexes == nil {
		store.indexes = make(map[string]*storeIndexBuild, len(b.exact))
	}
	for name, exact := range b.exact {
		pending := make(map[uint64][]storeIndexChunkMask)
		state.Chunks.Each(func(id uint32, chunk *StoreChunk) bool {
			var storage [StoreMaxChunkDocuments]storeIndexHashMask
			for _, entry := range storeIndexCollectChunk(storage[:0], exact, chunk) {
				pending[entry.hash] = append(pending[entry.hash], storeIndexChunkMask{
					chunk: id,
					mask:  entry.mask,
				})
			}
			return true
		})
		info := StoreIndexInfo{
			Name:          name,
			Kind:          StoreIndexExact,
			State:         StoreIndexReady,
			CoveredChunks: state.ChunkCount,
			TotalChunks:   state.ChunkCount,
			ColumnCount:   exact.N,
		}
		copy(info.Columns[:], exact.Specs[:exact.N])
		base, err := newStorePackedIndex(pending)
		if err != nil {
			return fmt.Errorf("vibejson: build packed exact index %q: %w", name, err)
		}
		store.indexes[name] = &storeIndexBuild{
			info:  info,
			exact: exact,
			base:  base,
			all:   true,
		}
	}
	state.Indexes = store.indexInfosLocked()
	state.secondary = store.indexSnapshotsLocked()
	return nil
}

const (
	// A Store can address fewer than 2^38 rows: 2^32 chunk ids times at most
	// 64 rows. Forty ordinal bits therefore leave a 24-bit hash fingerprint in
	// one pointer-free word without narrowing the public address space.
	storeBuilderKeyOrdinalBits = 40
	storeBuilderKeyOrdinalMask = uint64(1)<<storeBuilderKeyOrdinalBits - 1
	storeBuilderKeyMinSlots    = 16
)

// storeBuilderKeyTable is the unpublished builder's duplicate-key guard.
//
// Each occupied slot packs a 24-bit hash fingerprint and row ordinal+1 into
// one uint64. The ordinal resolves the exact key from builder-owned chunk
// bytes, so even a full hash collision is compared byte-for-byte. Unlike the
// online persistent HAMT, this append-only table needs no node or leaf per key,
// contains no pointers for the garbage collector to scan, and grows only
// geometrically. Build discards it after publishing the compact mapped key
// directory.
type storeBuilderKeyTable struct {
	slots []uint64
}

func (t *storeBuilderKeyTable) contains(b *StoreBuilder, hash uint64, key string) bool {
	if len(t.slots) == 0 {
		return false
	}
	fingerprint := hash >> storeBuilderKeyOrdinalBits
	mask := uint64(len(t.slots) - 1)
	for slot := hash & mask; ; slot = (slot + 1) & mask {
		packed := t.slots[slot]
		if packed == 0 {
			return false
		}
		if packed>>storeBuilderKeyOrdinalBits != fingerprint {
			continue
		}
		stored, ok := b.keyAt((packed & storeBuilderKeyOrdinalMask) - 1)
		if ok && stored == key {
			return true
		}
	}
}

func (t *storeBuilderKeyTable) reserve(b *StoreBuilder, entries int) {
	capacity := len(t.slots)
	if capacity != 0 && entries <= capacity-capacity/8 {
		return
	}
	if capacity == 0 {
		capacity = storeBuilderKeyMinSlots
	}
	for entries > capacity-capacity/8 {
		capacity *= 2
	}
	previous := t.slots
	t.slots = make([]uint64, capacity)
	for _, packed := range previous {
		if packed == 0 {
			continue
		}
		row := (packed & storeBuilderKeyOrdinalMask) - 1
		key, ok := b.keyAt(row)
		if !ok {
			panic("vibejson: StoreBuilder key table ordinal invariant")
		}
		t.insert(maphash.String(b.seed, key), row)
	}
}

func (t *storeBuilderKeyTable) insert(hash, row uint64) {
	packed := hash&^storeBuilderKeyOrdinalMask | (row + 1)
	mask := uint64(len(t.slots) - 1)
	for slot := hash & mask; ; slot = (slot + 1) & mask {
		if t.slots[slot] == 0 {
			t.slots[slot] = packed
			return
		}
	}
}

func (b *StoreBuilder) keyAt(row uint64) (string, bool) {
	chunkDocuments := uint64(b.options.ChunkDocuments)
	chunkID := row / chunkDocuments
	if chunkID > uint64(^uint32(0)) {
		return "", false
	}
	var chunk *StoreChunk
	if uint32(chunkID) < b.chunks.Count {
		chunk = b.chunks.Get(uint32(chunkID))
	} else if uint32(chunkID) == b.chunks.Count {
		chunk = b.current
	}
	slot := int(row % chunkDocuments)
	if chunk == nil || slot >= int(chunk.Count) {
		return "", false
	}
	return chunk.keys[slot], true
}
