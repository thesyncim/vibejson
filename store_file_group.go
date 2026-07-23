package slopjson

import (
	"fmt"
	"math/bits"

	"github.com/thesyncim/slopjson/internal/storeio"
)

// FileIndexScalarGroup is one collision-certified scalar group read directly
// from a frozen single-column exact index. Value borrows the supplied
// FileIndexWorkspace until its next use or Release. Count includes only rows
// certified by the posting representative. First is an opaque token ordered
// like the snapshot's stable chunk/slot traversal.
type FileIndexScalarGroup struct {
	Value RawValue
	Count uint64
	First uint64
}

type fileIndexScalarGroupState struct {
	hash          uint64
	certificateAt uint32
	certificateN  uint32
	count         uint64
	first         uint64
}

// AppendIndexScalarGroupsInto appends exact grouped counts from one frozen
// single-column exact index. A compact generation may answer from bounded
// aggregate pages containing O(groups) representatives, counts, and first-row
// tokens. Otherwise the method streams certified postings and residual
// receives ordered stable-slot candidates for missing and container values,
// legacy postings without representatives, and hash collisions.
// Feeding residual to RangeMasksRawRowsBuffer and grouping the selected path
// completes the exact result without reading certified JSON.
//
// The posting lane retains only two pointer-free words per stable chunk plus
// one compact representative per distinct certified value. Neither lane
// duplicates a per-row index in memory. Reusing workspace and caller
// destinations makes a warmed call allocation-free once their observed
// high-water marks fit. Returned Value slices borrow workspace.
func (s *FileSnapshot) AppendIndexScalarGroupsInto(
	dst []FileIndexScalarGroup,
	residual []StoreMask,
	workspace *FileIndexWorkspace,
	name string,
) ([]FileIndexScalarGroup, []StoreMask, bool, error) {
	if s == nil || s.store == nil || s.state == nil {
		return dst, residual, false, ErrFileStoreClosed
	}
	if workspace == nil {
		workspace = &FileIndexWorkspace{}
	}
	indexID := -1
	for i, definition := range s.store.options.Indexes {
		if definition.Name == name {
			indexID = i
			break
		}
	}
	if indexID < 0 {
		return dst, residual, false, ErrStoreIndexNotFound
	}
	exact := s.store.options.indexes[indexID]
	if exact == nil || exact.n != 1 {
		return dst, residual, false, ErrStoreIndexArity
	}
	state := s.state
	workspace.groupArena = workspace.groupArena[:0]
	workspace.groupState = workspace.groupState[:0]
	workspace.lastProbe = FileIndexProbeStats{}
	if catalogGroups, covered, err := s.appendIndexCatalogScalarGroups(
		dst, workspace, uint32(indexID),
	); err != nil || covered {
		return catalogGroups, residual, covered, err
	}
	if uint64(state.root.ChunkHighWater) > uint64(maxInt()) {
		return dst, residual, false, ErrStoreTooLarge
	}
	chunks := int(state.root.ChunkHighWater)
	workspace.indexCoverage = resizeFileIndexWords(workspace.indexCoverage, chunks)
	workspace.certifiedCoverage = resizeFileIndexWords(workspace.certifiedCoverage, chunks)
	clear(workspace.indexCoverage)
	clear(workspace.certifiedCoverage)
	var (
		haveHash    bool
		currentHash uint64
		hashStart   int
	)
	bounds := storeio.IndexTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		IndexHighWater: state.root.IndexCount,
	}
	err := storeio.WalkIndexTreeIndex(
		s.store.cache, state.indexRoot, uint32(indexID), bounds,
		func(directory storeio.IndexDirectoryView) error {
			var (
				postingRef   storeio.PageRef
				postingLease storeio.PageLease
				postingPage  storeio.PostingPageView
				leased       bool
			)
			defer func() {
				if leased {
					postingLease.Release()
				}
			}()
			for rank := 0; rank < directory.Len(); rank++ {
				entry, ok := directory.EntryAt(rank)
				if !ok {
					return storeio.ErrIndexDirectoryCorrupt
				}
				if entry.Key.IndexID < uint32(indexID) {
					continue
				}
				if entry.Key.IndexID > uint32(indexID) {
					break
				}
				if !leased || entry.Posting.Page != postingRef {
					if leased {
						postingLease.Release()
					}
					var acquireErr error
					postingLease, acquireErr = s.store.cache.Acquire(entry.Posting.Page)
					if acquireErr != nil {
						leased = false
						return acquireErr
					}
					leased = true
					postingRef = entry.Posting.Page
					postingPage, acquireErr = storeio.OpenPostingPage(
						postingLease.Page(), state.root.NextLogicalID,
						state.root.IndexCount,
					)
					if acquireErr != nil {
						return acquireErr
					}
					if postingPage.Header().IndexID != uint32(indexID) {
						return storeio.ErrPostingPageCorrupt
					}
					workspace.lastProbe.PostingPages++
				}
				segment, ok := postingPage.SegmentAt(int(entry.Posting.Segment))
				if !ok || segment.Len() != 1 ||
					segment.Header().TupleHash != entry.Key.TupleHash {
					return storeio.ErrPostingPageCorrupt
				}
				iterator := segment.Iterator()
				posting, ok := iterator.Next()
				if !ok || posting.Chunk != entry.Key.Chunk ||
					posting.Chunk >= state.root.ChunkHighWater ||
					posting.Bits&^fileStoreLiveMask(state.root.ChunkDocuments) != 0 {
					return storeio.ErrPostingPageCorrupt
				}
				chunk := int(posting.Chunk)
				if workspace.indexCoverage[chunk]&posting.Bits != 0 {
					return fmt.Errorf(
						"%w: overlapping scalar index streams",
						storeio.ErrPostingPageCorrupt,
					)
				}
				workspace.indexCoverage[chunk] |= posting.Bits
				rows := uint64(bits.OnesCount64(posting.Bits))
				workspace.lastProbe.CandidateRows += rows
				workspace.lastProbe.CandidateChunks++

				certificate := segment.Certificate()
				if segment.Header().Flags&storeio.PostingSegmentCollision != 0 ||
					len(certificate) == 0 {
					continue
				}
				if !fileIndexCertificateValid(certificate, 1) {
					return storeio.ErrPostingPageCorrupt
				}
				if !haveHash || currentHash != entry.Key.TupleHash {
					haveHash = true
					currentHash = entry.Key.TupleHash
					hashStart = len(workspace.groupState)
				}
				group := -1
				for candidate := hashStart; candidate < len(workspace.groupState); candidate++ {
					existing := workspace.groupState[candidate]
					if existing.hash != currentHash {
						break
					}
					start := int(existing.certificateAt)
					end := start + int(existing.certificateN)
					if fileIndexCertificatesEqual(
						workspace.groupArena[start:end:end], certificate, 1,
					) {
						group = candidate
						break
					}
				}
				first := uint64(posting.Chunk)<<6 |
					uint64(bits.TrailingZeros64(posting.Bits))
				if group < 0 {
					if uint64(len(workspace.groupArena))+uint64(len(certificate)) >
						uint64(^uint32(0)) {
						return ErrStoreTooLarge
					}
					start := len(workspace.groupArena)
					workspace.groupArena = append(workspace.groupArena, certificate...)
					workspace.groupState = append(
						workspace.groupState,
						fileIndexScalarGroupState{
							hash: currentHash, certificateAt: uint32(start),
							certificateN: uint32(len(certificate)), first: first,
						},
					)
					group = len(workspace.groupState) - 1
				}
				groupState := &workspace.groupState[group]
				if groupState.count > ^uint64(0)-rows {
					return ErrStoreTooLarge
				}
				groupState.count += rows
				if first < groupState.first {
					groupState.first = first
				}
				workspace.certifiedCoverage[chunk] |= posting.Bits
				workspace.lastProbe.CertificateRows += rows
				workspace.lastProbe.MatchedRows += rows
			}
			return nil
		},
	)
	if err != nil {
		return dst, residual, true, err
	}

	limit := fileStoreLiveMask(state.root.ChunkDocuments)
	err = storeio.WalkChunkTree(
		s.store.cache, state.chunkRoot,
		storeio.ChunkTreeBounds{
			FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		},
		func(chunk uint32, _ storeio.PageRef) error {
			candidates := limit &^ workspace.certifiedCoverage[chunk]
			if candidates != 0 {
				residual = append(residual, StoreMask{Chunk: chunk, Bits: candidates})
			}
			return nil
		},
	)
	if err != nil {
		return dst, residual, true, err
	}
	for _, group := range workspace.groupState {
		start := int(group.certificateAt)
		end := start + int(group.certificateN)
		dst = append(dst, FileIndexScalarGroup{
			Value: RawValue{src: workspace.groupArena[start:end:end]},
			Count: group.count, First: group.first,
		})
	}
	return dst, residual, true, nil
}

// appendIndexCatalogScalarGroups serves the compact-generation fast path.
// Representatives stream from bounded linked pages into workspace, so
// cardinality changes page count rather than one giant allocation. Results
// never borrow evictable page-cache storage.
func (s *FileSnapshot) appendIndexCatalogScalarGroups(
	dst []FileIndexScalarGroup,
	workspace *FileIndexWorkspace,
	indexID uint32,
) ([]FileIndexScalarGroup, bool, error) {
	state := s.state
	catalogRef := state.root.IndexGroupHead
	if catalogRef == (storeio.PageRef{}) {
		return dst, false, nil
	}
	total := uint64(0)
	var (
		coveredIndexes uint64
		previousIndex  uint32
		havePrevious   bool
		previousRef    storeio.PageRef
	)
	for catalogRef != (storeio.PageRef{}) {
		lease, err := s.store.cache.Acquire(catalogRef)
		if err != nil {
			return dst, true, err
		}
		catalog := storeio.AdmittedIndexGroupCatalog(lease.Page())
		header := catalog.Header()
		if previousRef == (storeio.PageRef{}) {
			coveredIndexes = header.CoveredIndexes
			if header.DocumentCount != state.root.DocumentCount {
				lease.Release()
				return dst, true, storeio.ErrIndexGroupCatalogCorrupt
			}
			if !catalog.Covered(indexID) {
				lease.Release()
				return dst, false, nil
			}
		} else if !catalog.Segmented() ||
			header.CoveredIndexes != coveredIndexes ||
			header.DocumentCount != state.root.DocumentCount ||
			header.Generation != previousRef.Generation {
			lease.Release()
			return dst, true, storeio.ErrIndexGroupCatalogCorrupt
		}
		iterator := catalog.Iterator()
		for {
			entry, ok := iterator.Next()
			if !ok {
				break
			}
			if havePrevious && entry.IndexID < previousIndex {
				lease.Release()
				return dst, true, storeio.ErrIndexGroupCatalogCorrupt
			}
			havePrevious = true
			previousIndex = entry.IndexID
			if entry.IndexID != indexID {
				continue
			}
			if !fileIndexCertificateValid(entry.Value, 1) ||
				uint64(len(workspace.groupArena))+uint64(len(entry.Value)) >
					uint64(^uint32(0)) ||
				total > ^uint64(0)-entry.Count {
				lease.Release()
				return dst, true, storeio.ErrIndexGroupCatalogCorrupt
			}
			start := len(workspace.groupArena)
			workspace.groupArena = append(
				workspace.groupArena, entry.Value...,
			)
			workspace.groupState = append(
				workspace.groupState,
				fileIndexScalarGroupState{
					certificateAt: uint32(start),
					certificateN:  uint32(len(entry.Value)),
					count:         entry.Count,
					first:         entry.First,
				},
			)
			total += entry.Count
		}
		next := header.Next
		if next != (storeio.PageRef{}) &&
			(!catalog.Segmented() ||
				next.LogicalID <= catalogRef.LogicalID ||
				next.Offset <= catalogRef.Offset) {
			lease.Release()
			return dst, true, storeio.ErrIndexGroupCatalogCorrupt
		}
		lease.Release()
		previousRef = catalogRef
		catalogRef = next
	}
	if len(workspace.groupState) == 0 || total != state.root.DocumentCount {
		return dst, true, storeio.ErrIndexGroupCatalogCorrupt
	}
	for _, group := range workspace.groupState {
		start := int(group.certificateAt)
		end := start + int(group.certificateN)
		dst = append(dst, FileIndexScalarGroup{
			Value: RawValue{src: workspace.groupArena[start:end:end]},
			Count: group.count, First: group.first,
		})
	}
	workspace.lastProbe.CertificateRows = total
	workspace.lastProbe.MatchedRows = total
	return dst, true, nil
}

func resizeFileIndexWords(words []uint64, length int) []uint64 {
	if cap(words) < length {
		return make([]uint64, length)
	}
	return words[:length]
}
