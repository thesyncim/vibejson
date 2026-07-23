package slopjson

import (
	"math/bits"
	"reflect"
)

// StoreIndexStats describes the physical footprint of one declared exact
// index in a Snapshot. The counts describe only the currently reachable
// immutable root; older Snapshots deliberately retain their own path-copied
// nodes until callers release them.
type StoreIndexStats struct {
	Info StoreIndexInfo

	// Fingerprints is the number of distinct composite hash buckets. A rare
	// full-hash collision shares a bucket and is separated by exact recheck.
	Fingerprints uint64
	// ChunkWords counts materialized stable-slot uint64 words. Empty chunk
	// ranges consume no posting storage.
	ChunkWords uint64
	// CandidateRows is the sum of set bits before collision rechecks.
	CandidateRows uint64
	// DirectoryNodes and BitmapNodes expose the persistent radix overhead.
	DirectoryNodes uint64
	BitmapNodes    uint64
	// EstimatedBytes accounts for reachable index-owned nodes, leaves,
	// compiled paths, and the published snapshot descriptor. It excludes Go
	// allocator size classes and Store documents, which the index only borrows.
	EstimatedBytes uint64
	// PackedBytes is the immutable base's physical page and hash-directory
	// footprint. ExternalBytes is the subset outside Go HeapAlloc on the
	// current platform.
	PackedBytes   uint64
	ExternalBytes uint64
}

// IndexStats returns allocation-free physical statistics for a declared exact
// index in this immutable Snapshot.
func (s Snapshot) IndexStats(name string) (StoreIndexStats, error) {
	index, ok := s.exactIndex(name)
	if !ok {
		return StoreIndexStats{}, ErrStoreIndexNotFound
	}
	stats := StoreIndexStats{Info: index.info}
	stats.EstimatedBytes = uint64(reflect.TypeFor[storeIndexSnapshot]().Size()) + uint64(reflect.TypeFor[storeExactIndex]().Size())
	stats.EstimatedBytes += uint64(len(index.info.Name))
	for i := 0; i < int(index.exact.n); i++ {
		stats.EstimatedBytes += uint64(len(index.exact.specs[i]))
		stats.EstimatedBytes += uint64(len(index.exact.paths[i].tokens)) * uint64(reflect.TypeFor[compiledPointerToken]().Size())
	}
	if index.base != nil {
		stats.Fingerprints += index.base.fingerprints
		stats.ChunkWords += index.base.chunkWords
		stats.CandidateRows += index.base.candidateRows
		stats.PackedBytes = uint64(index.base.block.Len())
		stats.ExternalBytes = index.base.externalBytes()
		stats.EstimatedBytes += stats.PackedBytes
	}
	storeIndexAccumulatePostingStats(index.root, &stats)
	storeIndexAccumulateMaskStats(index.dirty.root, &stats)
	return stats, nil
}

// IndexStats returns statistics for the current Snapshot.
func (s *Store) IndexStats(name string) (StoreIndexStats, error) {
	return s.Snapshot().IndexStats(name)
}

func storeIndexAccumulatePostingStats(node *storeIndexPostingNode, stats *StoreIndexStats) {
	if node == nil {
		return
	}
	stats.DirectoryNodes++
	stats.EstimatedBytes += uint64(reflect.TypeFor[storeIndexPostingNode]().Size())
	for i := range node.slots {
		slot := node.slots[i]
		if slot.child != nil {
			storeIndexAccumulatePostingStats(slot.child, stats)
		}
		if slot.leaf == nil {
			continue
		}
		stats.Fingerprints++
		stats.EstimatedBytes += uint64(reflect.TypeFor[storeIndexPostingLeaf]().Size())
		stats.ChunkWords += uint64(slot.leaf.masks.n) + uint64(slot.leaf.masks.wide.words)
		slot.leaf.masks.each(func(_ uint32, mask uint64) bool {
			stats.CandidateRows += uint64(bits.OnesCount64(mask))
			return true
		})
		storeIndexAccumulateMaskStats(slot.leaf.masks.wide.root, stats)
	}
}

func storeIndexAccumulateMaskStats(node *storeIndexMaskNode, stats *StoreIndexStats) {
	if node == nil {
		return
	}
	stats.BitmapNodes++
	stats.EstimatedBytes += uint64(reflect.TypeFor[storeIndexMaskNode]().Size())
	for _, child := range node.children {
		storeIndexAccumulateMaskStats(child, stats)
	}
}
