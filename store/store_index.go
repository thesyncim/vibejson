package store

import (
	"errors"
	"math/bits"
	"slices"
	"strings"
)

// IndexKind names an online secondary-index family.
type IndexKind uint8

const (
	// IndexPostings builds the DocSet existence/scalar-containment posting
	// layer in every Store chunk. It accelerates query equality, existence, and
	// scalar containment while exact predicate rechecks preserve semantics.
	IndexPostings IndexKind = iota + 1
	// IndexExact is a declared single-column or compound scalar index.
	// Create it with [Store.CreateIndex]. Its physical directory maps one
	// order-sensitive typed fingerprint to persistent stable-slot bitmaps;
	// hashes only prune and every returned row is verified exactly.
	IndexExact
)

// IndexState is an online index's publication state.
type IndexState uint8

const (
	// IndexBuilding means exact scan fallback is still required for some
	// live chunks.
	IndexBuilding IndexState = iota + 1
	// IndexReady means every live chunk has physical coverage.
	IndexReady
)

// IndexInfo is immutable index metadata published with a Snapshot.
// During Building, covered chunks already carry the index and uncovered
// chunks remain exact through scan fallback. Ready means every current chunk
// was covered; later writes dual-maintain it before publication.
type IndexInfo struct {
	// Name is the caller-assigned logical index name.
	Name string
	// Kind identifies the shared physical index family.
	Kind IndexKind
	// Columns contains the indexed RFC 6901 paths in compound-key order.
	// ColumnCount is zero for the legacy wildcard posting family.
	Columns     [MaxIndexColumns]string
	ColumnCount uint8
	// State is Building until CoveredChunks equals TotalChunks.
	State IndexState
	// CoveredChunks is the count eligible for the physical accelerated path.
	CoveredChunks uint32
	// TotalChunks is the number of live chunks in this publication.
	TotalChunks uint32
}

var (
	// ErrIndexExists reports an AddIndex or CreateIndex name collision.
	ErrIndexExists = errors.New("vibejson: Store index already exists")
	// ErrIndexNotFound reports an unknown index name.
	ErrIndexNotFound = errors.New("vibejson: Store index not found")
	// ErrIndexKind reports an IndexKind this build does not implement.
	ErrIndexKind = errors.New("vibejson: unsupported Store index kind")
)

type storeIndexBuild struct {
	info     IndexInfo
	coverage storeCoverage
	scan     storeChunkVector
	cursor   uint64
	all      bool
	exact    *ExactIndex
	root     *storeIndexPostingNode
	base     *storePackedIndex
	dirty    storeIndexMaskVector
}

type storeIndexReclaim struct{}

// AddIndex atomically publishes an online index definition. Existing chunks
// are backfilled by BackfillIndex; all writes from this call onward build the
// index before their new snapshot is visible. Query consumers can inspect the
// published coverage and use exact scan fallback until Ready.
func (s *Store) AddIndex(name string, kind IndexKind) (IndexInfo, error) {
	if kind != IndexPostings {
		return IndexInfo{}, ErrIndexKind
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.initLocked()
	if err != nil {
		return IndexInfo{}, err
	}
	if s.indexes == nil {
		s.indexes = make(map[string]*storeIndexBuild)
	}
	if _, exists := s.indexes[name]; exists {
		return IndexInfo{}, ErrIndexExists
	}
	name = strings.Clone(name)
	s.reclaim = nil // a new consumer cancels removal of the same physical layer
	b := &storeIndexBuild{info: IndexInfo{
		Name:        name,
		Kind:        kind,
		State:       IndexBuilding,
		TotalChunks: state.ChunkCount,
	}, scan: state.Chunks}
	// Logical postings indexes share one physical layer. Copying an existing
	// build's coverage is O(coverage words), not a document pass; a Store born
	// with Postings already covers the complete vector. If reclamation was in
	// flight, start conservatively uncovered and let bounded backfill discover
	// the still-indexed chunks without trusting stale logical metadata.
	for _, existing := range s.indexes {
		if existing.info.Kind == kind {
			b.coverage = existing.coverage.clone()
			b.info.CoveredChunks = existing.info.CoveredChunks
			b.info.State = existing.info.State
			b.all = existing.all
			break
		}
	}
	if state.StateOptions.Postings {
		b.info.CoveredChunks = b.info.TotalChunks
	}
	b.updateState()
	s.indexes[name] = b
	next := *state
	next.Generation++
	next.Indexes = s.indexInfosLocked()
	next.secondary = s.indexSnapshotsLocked()
	s.state.Store(&next)
	return b.info, nil
}

// CreateIndex atomically publishes a path-specific exact scalar index. A
// single path indexes one column; multiple paths form an order-sensitive
// compound key. Existing chunks are processed by BackfillIndex while every
// later write dual-maintains the definition before publication. Until Ready,
// probes use an exact scan fallback.
func (s *Store) CreateIndex(def IndexDefinition) (IndexInfo, error) {
	exact, err := CompileExactIndex(def)
	if err != nil {
		return IndexInfo{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.initLocked()
	if err != nil {
		return IndexInfo{}, err
	}
	if s.indexes == nil {
		s.indexes = make(map[string]*storeIndexBuild)
	}
	if _, exists := s.indexes[def.Name]; exists {
		return IndexInfo{}, ErrIndexExists
	}
	name := strings.Clone(def.Name)
	exact.seed = state.seed
	b := &storeIndexBuild{exact: exact, info: IndexInfo{
		Name:        name,
		Kind:        IndexExact,
		State:       IndexBuilding,
		TotalChunks: state.ChunkCount,
	}, scan: state.Chunks}
	b.info.ColumnCount = exact.N
	copy(b.info.Columns[:], exact.Specs[:exact.N])
	b.updateState()
	s.indexes[name] = b
	next := *state
	next.Generation++
	next.Indexes = s.indexInfosLocked()
	next.secondary = s.indexSnapshotsLocked()
	s.state.Store(&next)
	return b.info, nil
}

// BackfillIndex examines and rebuilds at most maxChunks chunks from the
// definition's immutable start snapshot, then publishes changed coverage
// atomically. maxChunks <= 0 means all remaining chunks. The resumable radix
// cursor skips deleted subtrees without scanning integer ids. Writes that
// touched a chunk after AddIndex already marked it covered and need no rebuild.
func (s *Store) BackfillIndex(name string, maxChunks int) (IndexInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.indexes[name]
	if b == nil {
		return IndexInfo{}, ErrIndexNotFound
	}
	state := s.state.Load()
	if b.info.State == IndexReady || state == nil {
		return b.info, nil
	}
	nextChunks := state.Chunks
	examined := 0
	detachedMapped := uint32(0)
	changed := false
	bulkRoot := b.root
	var pending map[uint64][]storeIndexChunkMask
	for maxChunks <= 0 || examined < maxChunks {
		id, _, ok := b.scan.next(b.cursor)
		if !ok {
			break
		}
		b.cursor = uint64(id) + 1
		examined++
		if b.has(id) {
			continue
		}
		chunk := state.Chunks.Get(id)
		if chunk == nil {
			continue
		}
		if b.exact != nil {
			var storage [MaxChunkDocuments]storeIndexHashMask
			for _, entry := range storeIndexCollectChunk(storage[:0], b.exact, chunk) {
				if pending == nil {
					pending = make(map[uint64][]storeIndexChunkMask)
				}
				pending[entry.hash] = append(pending[entry.hash], storeIndexChunkMask{chunk: id, mask: entry.mask})
			}
		} else if !chunk.Docs.Postings {
			rebuilt, err := cloneStoreChunk(state.StateOptions, true, chunk)
			if err != nil {
				return b.info, err
			}
			nextChunks = nextChunks.set(id, rebuilt)
			if state.mappedDocs != nil && chunk.Docs.mappedDocs == state.mappedDocs {
				detachedMapped++
			}
			s.noteChunkPostingsLocked(id, chunk, rebuilt)
		}
		if b.exact != nil {
			b.mark(id)
		} else {
			s.markIndexesCoveredLocked(id)
		}
		changed = true
	}
	if len(pending) != 0 {
		if bulkRoot == nil {
			b.root = storeIndexBuildBulk(pending)
		} else {
			b.root = storeIndexApplyBulk(bulkRoot, pending)
		}
	}
	if b.exact != nil && b.base == nil && b.info.CoveredChunks == b.info.TotalChunks {
		base, err := newStorePackedIndex(storeIndexPending(b.root))
		if err != nil {
			// Keep the complete heap root and Building state retryable. A later
			// BackfillIndex call can retry only the fold without rescanning rows.
			return b.info, err
		}
		b.base = base
		b.root = nil
		b.dirty = storeIndexMaskVector{}
		changed = true
	}
	b.updateState()
	if changed {
		next := *state
		next.Generation++
		next.detachMappedDocumentChunks(detachedMapped)
		next.Chunks = nextChunks
		next.Indexes = s.indexInfosLocked()
		next.secondary = s.indexSnapshotsLocked()
		s.state.Store(&next)
	}
	return b.info, nil
}

// DropIndex removes the logical definition from the next snapshot immediately.
// If it was the last postings consumer, future writes omit postings and
// ReclaimIndexes can rebuild old chunks in bounded batches. Old Snapshots keep
// their indexed chunks until ordinary garbage collection releases them.
func (s *Store) DropIndex(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.indexes == nil || s.indexes[name] == nil {
		return ErrIndexNotFound
	}
	delete(s.indexes, name)
	state := s.state.Load()
	if state == nil {
		return nil
	}
	if !s.options.Postings && !s.hasPostingsIndexLocked() {
		if len(s.postingChunks.ids) != 0 {
			s.reclaim = &storeIndexReclaim{}
		}
	}
	next := *state
	next.Generation++
	next.Indexes = s.indexInfosLocked()
	next.secondary = s.indexSnapshotsLocked()
	s.state.Store(&next)
	return nil
}

// ReclaimIndexes removes physically detached postings from at most maxChunks
// chunks, atomically publishing the batch. It returns the number rebuilt and
// whether reclamation is complete. maxChunks <= 0 processes all remaining
// chunks. No live read is blocked and old snapshots retain their old chunks.
func (s *Store) ReclaimIndexes(maxChunks int) (rebuilt int, done bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reclaim == nil || s.options.Postings || s.hasPostingsIndexLocked() {
		return 0, true
	}
	state := s.state.Load()
	if state == nil || len(s.postingChunks.ids) == 0 {
		s.reclaim = nil
		return 0, true
	}
	limit := maxChunks
	if limit <= 0 {
		limit = int(state.Chunks.Count)
	}
	nextChunks := state.Chunks
	detachedMapped := uint32(0)
	for len(s.postingChunks.ids) != 0 && rebuilt < limit {
		id := s.postingChunks.ids[len(s.postingChunks.ids)-1]
		chunk := state.Chunks.Get(id)
		if chunk == nil || !chunk.Docs.Postings {
			s.postingChunks.remove(id)
			continue
		}
		plain, err := cloneStoreChunk(state.StateOptions, false, chunk)
		if err != nil {
			panic("vibejson: rebuilding validated Store chunk: " + err.Error())
		}
		nextChunks = nextChunks.set(id, plain)
		if state.mappedDocs != nil && chunk.Docs.mappedDocs == state.mappedDocs {
			detachedMapped++
		}
		s.noteChunkPostingsLocked(id, chunk, plain)
		rebuilt++
	}
	done = len(s.postingChunks.ids) == 0
	if rebuilt != 0 {
		next := *state
		next.Generation++
		next.detachMappedDocumentChunks(detachedMapped)
		next.Chunks = nextChunks
		s.state.Store(&next)
	}
	if done {
		s.reclaim = nil
	}
	return rebuilt, done
}

func (b *storeIndexBuild) has(id uint32) bool {
	if b.all {
		return true
	}
	return b.coverage.has(id)
}

func (b *storeIndexBuild) mark(id uint32) {
	if b.all {
		b.info.CoveredChunks = b.info.TotalChunks
		return
	}
	if b.coverage.mark(id) {
		b.info.CoveredChunks++
	}
}

func (b *storeIndexBuild) unmark(id uint32) {
	if b.all {
		b.info.CoveredChunks = b.info.TotalChunks
		return
	}
	if b.coverage.unmark(id) {
		b.info.CoveredChunks--
	}
}

// updateState collapses complete coverage to an implicit all-live-chunks
// representation. Readiness is monotonic because every later write builds the
// active physical family before publication. Discarding both the bitmap and
// AddIndex snapshot prevents a completed logical index from pinning historical
// chunks or retaining one bit per chunk indefinitely.
func (b *storeIndexBuild) updateState() {
	if b.info.CoveredChunks == b.info.TotalChunks {
		b.info.State = IndexReady
		b.coverage = storeCoverage{}
		b.all = true
		b.scan = storeChunkVector{}
		b.cursor = 0
		return
	}
	b.info.State = IndexBuilding
}

func (s *Store) hasPostingsIndexLocked() bool {
	for _, b := range s.indexes {
		if b.info.Kind == IndexPostings {
			return true
		}
	}
	return false
}

func (s *Store) markIndexesCoveredLocked(id uint32) {
	for _, b := range s.indexes {
		if b.info.Kind == IndexPostings {
			b.mark(id)
			b.updateState()
		}
	}
}

// noteIndexesForChunkLocked updates logical coverage for a Store write. A
// newly materialized chunk joins every logical index already fully built;
// removing the last document removes that chunk from both total and covered
// counts. Rewrites dual-maintain before publication and therefore mark the
// chunk covered even while background backfill is still running elsewhere.
func (s *Store) noteIndexesForChunkLocked(id uint32, old, next *Chunk, changed uint64) (catalogChanged, secondaryChanged bool) {
	oldLive, nextLive := old != nil, next != nil
	for _, b := range s.indexes {
		oldInfo, oldRoot, oldDirty := b.info, b.root, b.dirty.root
		covered := b.has(id)
		if b.exact != nil && b.base != nil && b.dirty.get(id) == 0 {
			// Shadow the whole chunk before changing one slot. Base postings for
			// every tuple in a dirty chunk are skipped by readers, so seed the
			// delta with the complete old image exactly once.
			b.dirty = b.dirty.set(id, 1)
			if oldLive {
				b.root = storeIndexSetChunk(b.root, b.exact, id, old, true)
			}
		}
		switch {
		case !oldLive && nextLive:
			b.info.TotalChunks++
			if b.exact != nil {
				b.root = storeIndexSetChunk(b.root, b.exact, id, next, true)
			}
			b.mark(id)
		case oldLive && !nextLive:
			if b.exact != nil && covered {
				b.root = storeIndexSetChunk(b.root, b.exact, id, old, false)
			}
			b.info.TotalChunks--
			b.unmark(id)
		case nextLive:
			if b.exact != nil {
				if covered {
					for slots := changed; slots != 0; slots &= slots - 1 {
						slot := bits.TrailingZeros64(slots)
						b.root = storeIndexUpdateSlot(b.root, b.exact, id, old, next, slot)
					}
				} else {
					// A mutation of an uncovered chunk indexes its complete next
					// image, so the chunk can join coverage immediately.
					b.root = storeIndexSetChunk(b.root, b.exact, id, next, true)
				}
			}
			b.mark(id)
		}
		b.updateState()
		if b.info != oldInfo {
			catalogChanged = true
		}
		if b.exact != nil && (b.root != oldRoot || b.dirty.root != oldDirty || b.info != oldInfo) {
			secondaryChanged = true
		}
	}
	return catalogChanged, secondaryChanged
}

func (s *Store) indexSnapshotsLocked() []storeIndexSnapshot {
	if len(s.indexes) == 0 {
		return nil
	}
	out := make([]storeIndexSnapshot, 0, len(s.indexes))
	for _, b := range s.indexes {
		if b.exact != nil {
			out = append(out, storeIndexSnapshot{
				info: b.info, exact: b.exact, root: b.root, base: b.base, dirty: b.dirty,
			})
		}
	}
	slices.SortFunc(out, func(a, b storeIndexSnapshot) int {
		if a.info.Name < b.info.Name {
			return -1
		}
		if a.info.Name > b.info.Name {
			return 1
		}
		return 0
	})
	return out
}

func (s *Store) indexInfosLocked() []IndexInfo {
	if len(s.indexes) == 0 {
		return nil
	}
	out := make([]IndexInfo, 0, len(s.indexes))
	for _, b := range s.indexes {
		out = append(out, b.info)
	}
	slices.SortFunc(out, func(a, b IndexInfo) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	return out
}

// AppendIndexes appends immutable index metadata to dst.
func (s Snapshot) AppendIndexes(dst []IndexInfo) []IndexInfo {
	if s.state == nil {
		return dst
	}
	return append(dst, s.state.Indexes...)
}
