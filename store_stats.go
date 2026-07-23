package simdjson

import "time"

// StoreStats is an allocation-free operational snapshot. It is O(number of
// index definitions), never O(keys or chunks). The Chunks field counts
// materialized immutable chunks; ChunkHighWater is the persistent
// vector's address span and may be larger after deletes. Sparse vector
// traversal skips absent branches, so that difference is metadata, not scan or
// compaction debt. ReusableChunks includes both partially filled and empty
// chunk ids.
type StoreStats struct {
	// Generation is the latest atomic publication number.
	Generation uint64
	// Keys is the current document count.
	Keys int
	// Chunks is the number of materialized immutable chunks.
	Chunks uint32
	// ChunkHighWater is the persistent vector's address span.
	ChunkHighWater uint32
	// ChunkDocuments is the configured per-chunk document bound.
	ChunkDocuments int
	// ReusableChunks counts partially filled and empty writer-side ids.
	ReusableChunks int
	// ExpiringKeys is the exact TTL heap-node count.
	ExpiringKeys int
	// Indexes is the number of logical online index definitions.
	Indexes int
	// IndexedChunks counts chunks that physically retain postings.
	IndexedChunks int
	// IndexReclaiming reports detached physical postings still being removed.
	IndexReclaiming bool
	// MappedImageBytes is the caller-owned image retained by OpenStore.
	MappedImageBytes uint64
	// ExternalKeyBytes is pointer-free mapped key-directory metadata outside
	// Go HeapAlloc on supported Unix platforms. It remains process RSS.
	ExternalKeyBytes uint64
	// ExternalDocumentBytes is pointer-free document descriptors plus any
	// Store-owned packed source/tape blocks outside Go HeapAlloc on supported
	// Unix platforms. Caller-owned mapped image bytes remain separate.
	ExternalDocumentBytes uint64
	// ExternalIndexBytes is immutable exact-index page/directory storage
	// outside Go HeapAlloc on supported Unix platforms. Mutation deltas remain
	// ordinary snapshot-owned heap nodes until folded into a later base.
	ExternalIndexBytes uint64
}

// Stats returns current writer and publication counters without traversing
// documents or allocating. It briefly takes the writer mutex so TTL and
// reclamation counters describe the same instant as the published state.
func (s *Store) Stats() StoreStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state.Load()
	if state == nil {
		chunkDocuments := s.Options.ChunkDocuments
		if chunkDocuments == 0 {
			chunkDocuments = storeMaxChunkDocuments
		}
		return StoreStats{ChunkDocuments: chunkDocuments}
	}
	stats := StoreStats{
		Generation:      state.generation,
		Keys:            state.count,
		Chunks:          state.chunkCount,
		ChunkHighWater:  state.chunks.count,
		ChunkDocuments:  state.options.ChunkDocuments,
		ReusableChunks:  len(s.free.ids),
		ExpiringKeys:    len(s.ttl.heap),
		Indexes:         len(s.indexes),
		IndexedChunks:   len(s.postingChunks.ids),
		IndexReclaiming: s.reclaim != nil,
	}
	stats.MappedImageBytes = uint64(len(state.source))
	stats.ExternalKeyBytes = state.baseKeys.externalBytes()
	stats.ExternalDocumentBytes = state.mappedDocs.externalBytes()
	for _, index := range state.secondary {
		stats.ExternalIndexBytes += index.base.externalBytes()
	}
	return stats
}

// NextExpiration returns the earliest assigned deadline. Expired keys remain
// visible until ExpireDue or RunExpiry publishes their ordinary delete.
func (s *Store) NextExpiration() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ttl.heap) == 0 {
		return time.Time{}, false
	}
	return s.ttl.heap[0].deadline.time(), true
}
