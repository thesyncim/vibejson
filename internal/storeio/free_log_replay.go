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
	// FreeLogMaxImagePages bounds one base image, and with it the number of
	// extents the durable free set can hold. Bounding the image is what lets a
	// fold fit inside one ordinary transaction's page budget: the free log's
	// worst case is FreeLogMaxImagePages+FreeLogMaxDeltaPages pages, which is
	// less than the 21 pages the free B+tree could take when a mutation split
	// every level, so the transaction geometry it replaced still covers it.
	FreeLogMaxImagePages = 16
	// FreeLogMaxDeltaPages bounds one commit's diff. A commit that needs more
	// folds instead, which is always available because a fold's own diff
	// describes only the image and delta pages it just allocated.
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

// FreeLogPages are the pages that carry the current durable free set. The
// writer keeps them so a fold can retire exactly the pages it supersedes;
// without that list the old chain would be unreachable but never reclaimed,
// which is the same unbounded growth this representation exists to remove.
// Both slices run oldest first.
type FreeLogPages struct {
	Image []PageRef
	Delta []PageRef
}

// ReplayFreeLog rebuilds the durable free set from head, the newest delta page.
// It walks Prev to the end of the chain, takes the image the newest page names,
// loads that image, and applies the collected deltas oldest-to-newest.
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
	records, imageHead, err := collectFreeLogDeltas(cache, head, bounds, &pages)
	if err != nil {
		return dst, pages, err
	}
	image, err := collectFreeLogImage(cache, imageHead, bounds, &pages)
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
	var imageHead PageRef
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
		// Every page repeats the image it is relative to. Disagreement means two
		// chains have been spliced, and applying one chain's diff to another
		// chain's image would advertise space that is live.
		if len(pages.Delta) == 0 {
			imageHead = view.ImageHead()
		} else if view.ImageHead() != imageHead {
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
	return kept, imageHead, nil
}

func collectFreeLogImage(
	cache *PageCache, imageHead PageRef, bounds FreeLogBounds, pages *FreeLogPages,
) ([]FreeExtent, error) {
	if imageHead == (PageRef{}) {
		return nil, nil
	}
	extents := make([]FreeExtent, 0, FreeLogMaxImagePages*FreeImageRecordCapacity(imageHead.Length))
	for ref := imageHead; ref != (PageRef{}); {
		if len(pages.Image) == FreeLogMaxImagePages {
			return nil, fmt.Errorf(
				"%w: image exceeds %d pages", ErrFreeLogCorrupt, FreeLogMaxImagePages)
		}
		lease, err := cache.Acquire(ref)
		if err != nil {
			return nil, err
		}
		view, err := OpenFreeImagePage(lease.Page(), bounds.FileEnd, bounds.NextLogicalID)
		if err != nil {
			lease.Release()
			return nil, err
		}
		for i := range view.Len() {
			extent, ok := view.ExtentAt(i)
			// Page-local order is checked by the opener; the join between pages
			// is checked here, because a merge that assumes sorted input would
			// otherwise emit an unsorted free set from a plausible-looking chain.
			if !ok || len(extents) != 0 && extents[len(extents)-1].Offset+
				extents[len(extents)-1].Length > extent.Offset {
				lease.Release()
				return nil, fmt.Errorf("%w: image extent order", ErrFreeLogCorrupt)
			}
			extents = append(extents, extent)
		}
		pages.Image = append(pages.Image, ref)
		next := view.Next()
		lease.Release()
		ref = next
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
