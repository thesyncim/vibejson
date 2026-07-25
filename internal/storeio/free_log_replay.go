package storeio

import (
	"cmp"
	"fmt"
	"slices"
)

// Geometry of the durable free log. These are policy rather than encoding: the
// writer never exceeds them and the reader rejects anything that does, so a
// corrupt prev or next link cannot turn recovery into an unbounded walk, and a
// reopen's free-set cost is a fixed number of page reads instead of a function
// of how long the store has been running.
const (
	// FreeLogMaxChainPages is the delta-chain length at which the writer folds
	// the chain back into a fresh image. It is a straight trade: every open
	// reads the whole chain, and every fold rewrites the whole image. At 32 the
	// worst reopen reads 32 delta pages plus at most 16 image pages, 192 KiB at
	// the 4 KiB page size, while a fold recurs at most once per 32 commits and
	// so amortises to well under one image page written per commit. Lowering it
	// makes opens cheaper and steady-state commits more expensive; raising it
	// does the reverse.
	FreeLogMaxChainPages = 32
	// FreeLogMaxFoldSegments bounds how many image segments one fold rewrites,
	// and so how many pages a fold writes.
	//
	// It replaced FreeLogMaxImagePages, which bounded the image itself at
	// sixteen pages and therefore bounded the entire free set at about 2,700
	// extents. Bounding the whole image was the only way to bound a fold while
	// the image was a linked list, because changing one page changed the
	// reference in the page before it and a fold rewrote everything. With the
	// segment index a fold rewrites only the segments whose extents changed, so
	// the bound moved off the image and onto the fold, which is where it belongs:
	// the free set may now be as large as the index can name, and it is the
	// per-commit write that stays fixed.
	//
	// Sixteen is chosen against the reclaim batch. Reclamation is what scatters
	// changes across segments, so the store caps one commit's batch by how many
	// segments it would dirty rather than by extents alone, and extents that do
	// not fit stay pending and are offered again next commit. Allocation cannot
	// scatter: it takes from the tail of the lowest-offset extent that fits, so a
	// transaction's page allocations land in a short run of consecutive extents
	// and therefore in one or two segments. Sixteen leaves room for both plus the
	// splits a grown segment causes.
	FreeLogMaxFoldSegments = 16
	// FreeLogMaxDeltaPages bounds one commit's diff. A commit that needs more
	// folds instead, which is always available because a fold's own diff
	// describes only the segment, index, and delta pages it just allocated.
	FreeLogMaxDeltaPages = 4
)

// FreeLogBounds are the physical and logical high-water marks the selected
// superblock and state root publish. Every page in the chain is validated
// against them, so a stale or grafted page cannot describe space past the end
// of the file the caller is actually recovering.
type FreeLogBounds struct {
	FileEnd       uint64
	NextLogicalID uint64
}

// FreeLogPages are the pages that carry the current durable free set, and the
// segment descriptors the index published.
//
// The writer keeps the page lists so a fold can retire exactly what it
// supersedes; without them the old chain would be unreachable but never
// reclaimed, which is the same unbounded growth this representation exists to
// remove. It keeps Segments because a fold now rewrites only the segments that
// changed and must carry the rest forward by reference: a fold that rebuilt the
// index from scratch would have to rewrite every segment to learn where it was,
// which is the whole-image rewrite this design removed.
//
// Index and Delta run oldest first. Segments run in ascending FirstOffset.
type FreeLogPages struct {
	Index    []PageRef
	Delta    []PageRef
	Segments []FreeSegment
}

// ReplayFreeLog rebuilds the durable free set from head, the newest delta page.
// It walks Prev to the end of the chain, takes the segment index the newest
// page names, loads every segment, and applies the collected deltas
// oldest-to-newest.
//
// dst is appended to and must have room for limit extents; exceeding limit is
// reported rather than grown, because the caller's arena is fixed and reusing a
// larger Go-heap copy would silently move the free set onto the heap.
func ReplayFreeLog(
	cache *PageCache, head PageRef, bounds FreeLogBounds, dst []FreeExtent, limit int,
) ([]FreeExtent, FreeLogPages, error) {
	var pages FreeLogPages
	if head == (PageRef{}) {
		return dst, pages, nil
	}
	if cache == nil || limit < len(dst) {
		return dst, pages, fmt.Errorf("%w: free log replay", ErrInvalidWrite)
	}
	records, indexHead, err := collectFreeLogDeltas(cache, head, bounds, &pages)
	if err != nil {
		return dst, pages, err
	}
	if err := collectFreeLogIndex(cache, indexHead, bounds, &pages); err != nil {
		return dst, pages, err
	}
	image, err := collectFreeLogSegments(cache, bounds, pages.Segments)
	if err != nil {
		return dst, pages, err
	}
	dst, err = applyFreeLogRecords(dst, image, records, limit)
	if err != nil {
		return dst, pages, err
	}
	return dst, pages, nil
}

// freeLogRecord pairs one delta with how recent it is. The chain is walked
// newest-first and each page's records are read back to front, so rank
// ascending is exactly reverse chronological order and the smallest rank for an
// offset is the record that survives. Ranking avoids a second traversal purely
// to reverse the chain, which would double the reads a reopen performs.
type freeLogRecord struct {
	rank  int
	delta FreeDelta
}

func collectFreeLogDeltas(
	cache *PageCache, head PageRef, bounds FreeLogBounds, pages *FreeLogPages,
) ([]freeLogRecord, PageRef, error) {
	records := make([]freeLogRecord, 0, FreeLogMaxChainPages*FreeDeltaRecordCapacity(head.Length))
	var indexHead PageRef
	for ref := head; ref != (PageRef{}); {
		if len(pages.Delta) == FreeLogMaxChainPages {
			return nil, PageRef{}, fmt.Errorf(
				"%w: delta chain exceeds %d pages", ErrFreeLogCorrupt, FreeLogMaxChainPages)
		}
		lease, err := cache.Acquire(ref)
		if err != nil {
			return nil, PageRef{}, err
		}
		view, err := OpenFreeDeltaPage(lease.Page(), bounds.FileEnd, bounds.NextLogicalID)
		if err != nil {
			lease.Release()
			return nil, PageRef{}, err
		}
		// Every page repeats the index it is relative to. Disagreement means two
		// chains have been spliced, and applying one chain's diff to another
		// chain's image would advertise space that is live.
		if len(pages.Delta) == 0 {
			indexHead = view.IndexHead()
		} else if view.IndexHead() != indexHead {
			lease.Release()
			return nil, PageRef{}, fmt.Errorf("%w: chain spans two images", ErrFreeLogCorrupt)
		}
		for i := view.Len() - 1; i >= 0; i-- {
			delta, ok := view.DeltaAt(i)
			if !ok {
				lease.Release()
				return nil, PageRef{}, ErrFreeLogCorrupt
			}
			records = append(records, freeLogRecord{rank: len(records), delta: delta})
		}
		pages.Delta = append(pages.Delta, ref)
		prev := view.Prev()
		lease.Release()
		ref = prev
	}
	slices.Reverse(pages.Delta)
	slices.SortFunc(records, func(a, b freeLogRecord) int {
		if c := cmp.Compare(a.delta.Extent.Offset, b.delta.Extent.Offset); c != 0 {
			return c
		}
		return cmp.Compare(a.rank, b.rank)
	})
	kept := records[:0]
	var previous uint64
	for i, record := range records {
		if i == 0 || record.delta.Extent.Offset != previous {
			kept = append(kept, record)
		}
		previous = record.delta.Extent.Offset
	}
	return kept, indexHead, nil
}

// collectFreeLogIndex walks the index chain from its newest page and leaves
// pages.Segments in ascending FirstOffset. The chain is walked high-to-low
// because that is the only direction a copy-on-write chain can be written in,
// and reversed once at the end rather than traversed twice.
func collectFreeLogIndex(
	cache *PageCache, indexHead PageRef, bounds FreeLogBounds, pages *FreeLogPages,
) error {
	if indexHead == (PageRef{}) {
		return nil
	}
	pages.Segments = make([]FreeSegment, 0,
		FreeLogMaxIndexPages*FreeIndexRecordCapacity(indexHead.Length))
	for ref := indexHead; ref != (PageRef{}); {
		if len(pages.Index) == FreeLogMaxIndexPages {
			return fmt.Errorf(
				"%w: index exceeds %d pages", ErrFreeLogCorrupt, FreeLogMaxIndexPages)
		}
		lease, err := cache.Acquire(ref)
		if err != nil {
			return err
		}
		view, err := OpenFreeIndexPage(lease.Page(), bounds.FileEnd, bounds.NextLogicalID)
		if err != nil {
			lease.Release()
			return err
		}
		// Read this page's segments back to front so that reversing the whole
		// accumulated slice once, below, restores ascending order across pages.
		for i := view.Len() - 1; i >= 0; i-- {
			segment, ok := view.SegmentAt(i)
			if !ok {
				lease.Release()
				return ErrFreeLogCorrupt
			}
			pages.Segments = append(pages.Segments, segment)
		}
		pages.Index = append(pages.Index, ref)
		prev := view.Prev()
		lease.Release()
		ref = prev
	}
	slices.Reverse(pages.Index)
	slices.Reverse(pages.Segments)
	// Page-local order is checked by the opener; the join between pages is
	// checked here, because a merge that assumes a partitioned offset space
	// would otherwise give one offset two owners from a plausible-looking chain.
	for i := 1; i < len(pages.Segments); i++ {
		if pages.Segments[i-1].FirstOffset >= pages.Segments[i].FirstOffset {
			return fmt.Errorf("%w: index segment order across pages", ErrFreeLogCorrupt)
		}
	}
	return nil
}

// collectFreeLogSegments reads every segment named by the index. Reading them
// all is what makes an open proportional to the free set rather than to the
// working set; the index exists so that a later change can read only the
// segments a commit needs, and nothing in the durable format has to change for
// that — a segment already stands alone.
func collectFreeLogSegments(
	cache *PageCache, bounds FreeLogBounds, segments []FreeSegment,
) ([]FreeExtent, error) {
	if len(segments) == 0 {
		return nil, nil
	}
	total := 0
	for _, segment := range segments {
		total += int(segment.Count)
	}
	extents := make([]FreeExtent, 0, total)
	for _, segment := range segments {
		lease, err := cache.Acquire(segment.Ref)
		if err != nil {
			return nil, err
		}
		view, err := OpenFreeImagePage(lease.Page(), bounds.FileEnd, bounds.NextLogicalID)
		if err != nil {
			lease.Release()
			return nil, err
		}
		// The index publishes each segment's extent count and first offset. A
		// segment whose page disagrees means the index and the image were
		// published from different states, and merging them would advertise
		// space some other segment still owns.
		if view.Len() != int(segment.Count) {
			lease.Release()
			return nil, fmt.Errorf("%w: segment count disagrees with index", ErrFreeLogCorrupt)
		}
		for i := range view.Len() {
			extent, ok := view.ExtentAt(i)
			if !ok || i == 0 && extent.Offset != segment.FirstOffset ||
				len(extents) != 0 && extents[len(extents)-1].Offset+
					extents[len(extents)-1].Length > extent.Offset {
				lease.Release()
				return nil, fmt.Errorf("%w: segment extent order", ErrFreeLogCorrupt)
			}
			extents = append(extents, extent)
		}
		lease.Release()
	}
	return extents, nil
}

// applyFreeLogRecords merges the offset-sorted surviving records into the
// offset-sorted image. Both inputs are keyed by offset and an extent's offset
// never moves once chosen — allocation always takes from an extent's tail —
// so one linear pass is the whole of "apply the deltas".
func applyFreeLogRecords(
	dst []FreeExtent, image []FreeExtent, records []freeLogRecord, limit int,
) ([]FreeExtent, error) {
	start := len(dst)
	appendExtent := func(extent FreeExtent) error {
		if len(dst) == limit {
			return ErrRetiredExtentCapacity
		}
		if len(dst) != start {
			previous := dst[len(dst)-1]
			// Overlap is the one failure that cannot be tolerated: an
			// over-reported free set hands out space a live page occupies.
			if previous.Offset+previous.Length > extent.Offset {
				return fmt.Errorf("%w: overlapping free extents", ErrFreeLogCorrupt)
			}
		}
		dst = append(dst, extent)
		return nil
	}
	i, j := 0, 0
	for i < len(image) || j < len(records) {
		var extent FreeExtent
		switch {
		case j == len(records) || i < len(image) && image[i].Offset < records[j].delta.Extent.Offset:
			extent = image[i]
			i++
		default:
			record := records[j]
			j++
			if i < len(image) && image[i].Offset == record.delta.Extent.Offset {
				i++
			}
			if record.delta.Op == FreeOpDelete {
				continue
			}
			extent = record.delta.Extent
		}
		if err := appendExtent(extent); err != nil {
			return dst[:start], err
		}
	}
	return dst, nil
}
