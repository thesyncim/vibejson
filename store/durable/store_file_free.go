package durable

import (
	"fmt"
	"slices"

	"github.com/thesyncim/vibejson/internal/storeio"
)

// The durable free set, and why it is a log rather than a tree.
//
// Reclaimed space is only free once it is written down. The B+tree that used to
// hold it could persist exactly one edit per commit, because mutating a tree
// costs pages, allocating those pages changes the free set, and a changed free
// set changes the tree's shape. Everything a commit reclaimed past the first
// extent stayed in memory and was abandoned at the next restart, clean or not:
// the same write volume cost 6.3 MiB in one session and 23.9 MiB across eight.
//
// A flat log breaks that cycle because a record can describe its own
// allocation. Content is fixed once an extent is chosen, so d records need
// p = ceil(d/capacity) pages, allocating those adds at most p more records, and
// the recomputation converges. A commit therefore writes its complete diff, and
// the in-memory set and the durable set are the same set — which is the
// property the tests assert after every commit, because its violation is what
// corrupts.
//
// Two failure directions are not symmetric. Under-reporting free space leaks
// and is recoverable; over-reporting hands out space a live page occupies and
// is not. Every bound here therefore refuses to reclaim rather than refusing to
// be exact: a full arena stops taking extents, an oversized diff folds instead
// of truncating, and a replay that cannot prove its result is disjoint fails.
//
// What the log costs per commit is a second question, and it is the one that
// decides whether this scales. Folding used to rewrite the whole image, so the
// image had to be small enough to rewrite — sixteen pages, about 2,700 extents,
// roughly 11 MiB of trackable free space at the 4 KiB page size — and a store
// that fragmented past that could no longer write down space it had reclaimed.
// The image is now a set of independently addressed segments named by an index,
// a fold rewrites only the segments a commit changed, and what must fit inside
// one commit is a directory of the free set rather than the free set. See
// foldFreeLog for the mechanics and internal/storeio/free_index.go for the
// durable shape.

const (
	// freeReclaimBatch bounds how many retired extents one commit folds into the
	// free set. It exists so the merge scratch is a fixed allocation and so one
	// commit's diff cannot outgrow its delta pages; the reclaimer keeps whatever
	// does not fit and offers it again next commit, so nothing is lost by
	// draining slowly.
	freeReclaimBatch = 256
	// freeLogMinFoldChain is the shortest chain worth folding. The fold point is
	// otherwise the size of the image, and an image of one page would then force
	// a fold every commit, doubling metadata writes to save four pages.
	freeLogMinFoldChain = 4
	// freeLogAllocationRounds bounds the delta-page fixed point before the
	// writer stops reusing space and appends the remainder instead. Two rounds
	// suffice in every case the arithmetic admits; the third exists so that
	// termination does not depend on that argument being right.
	freeLogAllocationRounds = 3
)

// freeLogCommit is what one commit decided to do to the durable free log. It is
// applied to in-memory bookkeeping only after Publish succeeds: an aborted
// transaction leaves the previously published chain on disk, and the store must
// go on describing the free set relative to that chain rather than to pages
// that were never written.
type freeLogCommit struct {
	head     storeio.PageRef
	checksum uint32
	// changed distinguishes "wrote a new chain head" from "the free set did not
	// move, so the published head still describes it".
	changed bool
	folded  bool
}

// refreshReusable brings the in-memory free set up to date before a commit
// begins: it replays the durable log once per open, then folds in whatever the
// reclaimer can now prove no reader and no recovery root can still reach.
func (c *Collection) refreshReusable(state *fileStoreState) error {
	if !c.freeLoaded {
		before := len(c.reusable)
		reusable, pages, err := storeio.ReplayFreeLog(
			c.cache, state.freeHead,
			storeio.FreeLogBounds{FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID},
			c.reusable, c.freeSetLimit,
		)
		if err != nil {
			clear(c.reusable[before:])
			c.reusable = c.reusable[:before]
			return err
		}
		c.reusable = reusable
		c.freeSegments = append(c.freeSegments[:0], pages.Segments...)
		c.freeIndexPages = append(c.freeIndexPages[:0], pages.Index...)
		c.freeDeltaPages = append(c.freeDeltaPages[:0], pages.Delta...)
		// Every segment the replayed chain had records for is already stale on
		// disk: the records won, so the page they overrode no longer describes
		// what the store now holds. Marking them here is what lets the first fold
		// after an open rewrite exactly those and carry the rest by reference.
		c.resetFreeDirty()
		for _, record := range c.freePending {
			c.markFreeDirty(record.Extent.Offset)
		}
		if len(pages.Delta) != 0 {
			c.freeDirtyAll = true
		}
		c.freeLoaded = true
	}
	durable := c.committer.DurableGeneration()
	c.cache.MarkDurable(durable)
	// Reclaim as many extents as the arena has room for, rather than declining
	// the batch whenever the pending set is larger than the room left. That
	// guard protected a real invariant — c.reusable is backed by a fixed
	// off-heap block and must never be reallocated onto the Go heap — but it
	// enforced it by doing nothing at all, which is self-defeating: this call
	// is the only drain of the pending set. Once it declined, the pending set
	// could only grow, so it never fell back under the bound, so the call
	// never resumed. It reached its own capacity a few commits later and
	// RetireBatch began failing every write, and releasing the snapshot that
	// started it did not recover the store — only restarting the process did,
	// abandoning every pending extent. The bound now limits how much moves
	// instead of whether anything moves.
	oldestRecovery := uint64(1)
	if durable > 1 {
		oldestRecovery = durable - 1
	}
	batch := c.reclaimer.AppendReusable(
		c.freeReclaimed[:0], state.root.Generation, oldestRecovery,
		min(c.freeSetLimit-len(c.reusable), freeReclaimBatch),
	)
	// Reclamation is the only thing that scatters changes across segments, and a
	// fold has to rewrite every segment one commit dirtied. Trimming the batch by
	// the segments it would dirty is therefore what keeps a fold bounded, and it
	// costs nothing: what does not fit stays pending and is offered again at the
	// next commit, exactly as an over-large batch already was.
	batch = c.trimBatchToFoldReserve(batch)
	if len(batch) == 0 {
		return nil
	}
	slices.SortFunc(batch, func(a, b storeio.FreeExtent) int {
		if a.Offset < b.Offset {
			return -1
		}
		if a.Offset > b.Offset {
			return 1
		}
		return 0
	})
	c.freeReclaimed = batch
	return c.mergeReusable(batch)
}

// trimBatchToFoldReserve drops the tail of an offset-sorted batch that would
// push the number of dirty segments past what one fold may rewrite.
//
// It counts segments rather than extents because the fold reserve is a page
// budget: a hundred reclaimed extents inside one segment cost one page, and
// two extents in two segments cost two. Dropping the tail rather than the whole
// batch matters for the same reason refreshReusable stopped declining batches
// outright — this call is the only drain of the pending set, and a drain that
// refuses to move anything is a drain that never resumes.
func (c *Collection) trimBatchToFoldReserve(batch []storeio.FreeExtent) []storeio.FreeExtent {
	if c.freeDirtyAll || len(c.freeSegments) == 0 {
		return batch
	}
	// Half the reserve is left for the segments this commit's own allocations
	// dirty and for the splits a grown segment causes.
	budget := storeio.FreeLogMaxFoldSegments/2 - c.freeDirtyCount
	if budget <= 0 {
		budget = 1
	}
	seen, last := 0, -1
	for i, extent := range batch {
		index := c.segmentOfFreeOffset(extent.Offset)
		if index == last || index >= 0 && index < len(c.freeDirty) && c.freeDirty[index] {
			continue
		}
		last = index
		seen++
		if seen > budget {
			return batch[:i]
		}
	}
	return batch
}

// mergeReusable folds an offset-sorted batch of newly reclaimed extents into
// the offset-sorted free set and records the resulting durable diff.
//
// It coalesces across the whole set, not merely within the batch. The tree
// coalesced only within a batch because it could not afford to edit a neighbour
// it had not already touched; a free set that never re-merges fragments until
// no single extent can serve a document page, at which point a store with
// megabytes free grows the file anyway.
func (c *Collection) mergeReusable(batch []storeio.FreeExtent) error {
	head, count := len(c.reusable), len(batch)
	if count > cap(c.reusable)-head || head+count > c.freeSetLimit {
		return storeio.ErrRetiredExtentCapacity
	}
	// Everything strictly below, and not adjacent to, the batch's lowest extent
	// is untouched: the set is kept coalesced, so nothing before this point can
	// absorb or be absorbed by anything in the batch. Skipping that prefix makes
	// the cost proportional to the affected region instead of to the whole set.
	low, high := 0, head
	for low < high {
		middle := int(uint(low+high) >> 1)
		if c.reusable[middle].Offset+c.reusable[middle].Length < batch[0].Offset {
			low = middle + 1
		} else {
			high = middle
		}
	}
	base := low
	c.reusable = c.reusable[:head+count]
	// Merge right to left, in place. The write cursor stays strictly above the
	// unread set-side index — w = i + j + 2 (+1 while an extent is held) with
	// j >= -1 — so output can overwrite the region the merge has already
	// consumed. That is what the batch's separate scratch buys: two adjacent
	// runs inside one array cannot be merged in place in either direction.
	i, j, w := head-1, count-1, head+count
	var held storeio.FreeExtent
	holding, durable, changed := false, false, false
	flush := func() {
		w--
		c.reusable[w] = held
		// A record is needed when the offset is new to the durable set or when
		// what lives at that offset has changed length. An extent that passed
		// through untouched is already on disk.
		if !durable || changed {
			c.appendFreePending(storeio.FreeDelta{Op: storeio.FreeOpSet, Extent: held})
		}
	}
	for i >= base || j >= 0 {
		var extent storeio.FreeExtent
		fromSet := j < 0 || i >= base && c.reusable[i].Offset > batch[j].Offset
		if fromSet {
			extent = c.reusable[i]
			i--
		} else {
			extent = batch[j]
			j--
		}
		if holding {
			end := extent.Offset + extent.Length
			// Overlap means the same range was reclaimed twice or was never
			// retired. Publishing it would advertise live space; refusing the
			// commit only stalls reclamation.
			if end > held.Offset {
				return fmt.Errorf("%w: overlapping reclaimed extents", storeio.ErrFreeLogCorrupt)
			}
			if end == held.Offset {
				if durable {
					// The absorbed extent's offset stops naming anything. Its
					// space is not lost: it is now the tail of the extent below.
					c.appendFreePending(storeio.FreeDelta{
						Op: storeio.FreeOpDelete, Extent: storeio.FreeExtent{Offset: held.Offset},
					})
				}
				held = storeio.FreeExtent{
					Offset: extent.Offset, Length: extent.Length + held.Length,
					RetiredGeneration: max(extent.RetiredGeneration, held.RetiredGeneration),
				}
				durable, changed = fromSet, true
				continue
			}
			flush()
		}
		held, holding, durable, changed = extent, true, fromSet, false
	}
	if holding {
		flush()
	}
	moved := copy(c.reusable[base:], c.reusable[w:head+count])
	clear(c.reusable[base+moved:])
	c.reusable = c.reusable[:base+moved]
	return nil
}

// appendFreePending records a change the durable log does not yet carry. The
// list survives an aborted transaction, because refreshReusable's edits to the
// in-memory set are not rolled back and would otherwise never reach disk.
func (c *Collection) appendFreePending(delta storeio.FreeDelta) {
	// Once a fold is required the list is dead weight: the fold rebuilds every
	// segment straight from memory, so nothing accumulated here can still matter.
	// Overflowing into a fold is also what keeps the list bounded across a run
	// of consecutive aborts.
	//
	// Requiring a fold therefore has to dirty everything. The list is what told
	// the next fold which segments had moved, and a fold that dropped the list
	// without dirtying every segment would carry stale segment pages forward by
	// reference — publishing, as free, space the store had already taken back.
	if c.freeFoldRequired {
		return
	}
	c.markFreeDirty(delta.Extent.Offset)
	if len(c.freePending) == cap(c.freePending) {
		c.freeFoldRequired = true
		c.freeDirtyAll = true
		c.freePending = c.freePending[:0]
		return
	}
	c.freePending = append(c.freePending, delta)
}

// syncFreeLog records this commit's complete free-set diff and returns the new
// chain head. It must run after every other allocation in the transaction —
// including the state root, which is why the state page is reserved before this
// call and encoded after it — because a page allocated afterwards would consume
// free space that no record describes.
func (c *Collection) syncFreeLog(tx *storeio.WriteTransaction, state *fileStoreState) (freeLogCommit, error) {
	c.freeNewSegments = c.freeNewSegments[:0]
	c.freeNewIndex = c.freeNewIndex[:0]
	c.freeNewDelta = c.freeNewDelta[:0]
	deltas, err := c.appendFreeAllocationDeltas(
		append(c.freeDeltas[:0], c.freePending...), tx.ReuseEdits(), 0)
	if err != nil {
		return freeLogCommit{}, err
	}
	c.freeDeltas = deltas
	// A commit that neither consumed nor reclaimed free space leaves the durable
	// set exactly as the published chain already describes it. Keeping the old
	// head is not a micro-optimisation: writing an empty delta every commit
	// would drive the chain to its fold threshold, and rewrite the whole image,
	// for no change at all.
	if !c.freeFoldRequired && len(c.freeDeltas) == 0 {
		return freeLogCommit{head: state.freeHead, checksum: state.super.FreeChecksum}, nil
	}
	// Size the chain against the worst case before allocating anything. A commit
	// that discovered halfway through that it needed to fold instead would have
	// already allocated delta pages it will not encode, and an unstaged page
	// fails publication.
	live := c.liveReusable()
	room := storeio.FreeLogMaxChainPages - len(c.freeDeltaPages)
	need := freeLogPageCount(len(c.freeDeltas)+storeio.FreeLogMaxDeltaPages, c.freeDeltaPerPage)
	// A fold must rewrite every dirty segment in one commit, because the delta
	// chain it truncates is what described those segments' changes. So the
	// number of dirty segments is also a fold trigger: letting it drift past the
	// fold reserve would leave a commit that must fold and cannot.
	if c.freeFoldRequired || need > min(room, storeio.FreeLogMaxDeltaPages) ||
		c.freeDirtySegments() >= storeio.FreeLogMaxFoldSegments/2 ||
		len(c.freeDeltaPages)+need > c.freeFoldThreshold() {
		return c.foldFreeLog(tx, state, live)
	}
	var indexHead storeio.PageRef
	if len(c.freeIndexPages) != 0 {
		indexHead = c.freeIndexPages[len(c.freeIndexPages)-1]
	}
	return c.writeFreeDeltaChain(tx, state.freeHead, indexHead, 0, false)
}

// foldFreeLog rewrites the segments this store has changed since the last fold,
// republishes the segment index, and starts a fresh one-link delta chain.
//
// It is the whole point of the segment index. The flat image was a linked list,
// so changing one extent changed the page holding it, which changed that page's
// offset, which changed the reference in the page before it: a fold rewrote
// every image page no matter how little had moved. That is why the image had to
// stay inside sixteen pages, and why the free set could hold only about 2,700
// extents — roughly 11 MiB of trackable free space at the 4 KiB page size,
// against the millions of extents a multi-terabyte store fragments into.
//
// Here a fold writes only the segments whose extents changed plus the index,
// and the index is smaller than the set it describes by the segment fan-out. So
// the per-commit cost stopped being a function of how much free space exists
// and became a function of how much of it this commit touched, which is what
// "O(1)-ish per commit" has to mean for a structure that must also be complete.
//
// Segment contents are dumped straight from c.reusable rather than replayed
// forward from the old chain. c.reusable is already the complete authoritative
// set; an incremental rebuild would only add a second, less direct way to be
// wrong, and it is the disagreement between the two that corrupts.
func (c *Collection) foldFreeLog(
	tx *storeio.WriteTransaction, state *fileStoreState, liveCount int,
) (freeLogCommit, error) {
	if liveCount == 0 {
		// An empty free set is fully described by publishing no free reference
		// at all: a replay of nothing is the empty set, which is the right
		// answer rather than a missing one.
		if err := c.retireFreeLogPages(state, true); err != nil {
			return freeLogCommit{}, err
		}
		return freeLogCommit{changed: true, folded: true}, nil
	}
	// Compact the live set out of c.reusable before anything is planned. Entries
	// an in-flight transaction consumed whole are zeroed rather than removed —
	// finalizeReusable only compacts after Publish — and a zeroed entry has no
	// offset, so planning segment ranges over the raw array would both misplace
	// boundaries and encode an extent the image encoder rightly rejects.
	live := c.freeImageScratch[:0]
	for _, extent := range c.reusable {
		if extent.Length != 0 {
			live = append(live, extent)
		}
	}
	c.freeImageScratch = live
	plan, err := c.planFreeFold(live)
	if err != nil {
		return freeLogCommit{}, err
	}
	// The segment contents are the free set as it stood after every other
	// allocation this commit made and before any the fold makes. Everything the
	// fold itself consumes from here — segment pages, index pages, delta pages —
	// is therefore recorded in the fresh chain rather than being inside the
	// segments, which is the same self-describing trick the chain has always
	// used and the reason a fold needs no second commit to describe its own
	// allocation. Taking the mark before the first allocation rather than after
	// is what makes that true; taking it later would leave the segment pages'
	// own extents advertised as free by the very segments they were cut from.
	editStart := len(tx.ReuseEdits())
	if err := c.writeFreeSegments(tx, plan, live); err != nil {
		return freeLogCommit{}, err
	}
	indexHead, err := c.writeFreeIndex(tx)
	if err != nil {
		return freeLogCommit{}, err
	}
	if err := c.retireFreeLogPages(state, true); err != nil {
		return freeLogCommit{}, err
	}
	return c.writeFreeDeltaChain(tx, storeio.PageRef{}, indexHead, editStart, true)
}

// freeFoldPlan is the segment layout one fold will publish, expressed as ranges
// of the in-memory free set. It is computed before anything is allocated
// because a fold that discovered halfway through that it needed one more page
// would already have staged pages it cannot now describe.
type freeFoldPlan struct {
	// rebuilt are half-open [lo,hi) ranges of c.reusable, one per new segment
	// page. carried names, for each entry, the published segment it replaces, or
	// -1 for a segment that is new.
	rebuilt [][2]int
	// slots maps each new segment page to its rank in the published order, so
	// clean segments can be spliced back in without being read.
	order []freeFoldSlot
}

// freeFoldSlot is one entry in the folded index: either a segment carried
// forward untouched, or one of the pages this fold is about to write.
type freeFoldSlot struct {
	carried storeio.FreeSegment
	rebuilt int
	fresh   bool
}

// planFreeFold decides which segments this fold rewrites and how the rewritten
// extents divide into pages.
//
// A dirty segment is rebuilt from whatever c.reusable now holds inside the
// offset range that segment owns, which may be more extents than one page holds
// — so it splits — or none at all — so it disappears. Neither case propagates:
// the index is an array that is rewritten whole at every fold, so adding or
// removing an entry costs nothing beyond the index rewrite already being paid.
// That is the property a B+tree could not have here, where an insert splits a
// node, the split allocates a page, the allocation changes the free set, and
// the changed free set may split again.
func (c *Collection) planFreeFold(live []storeio.FreeExtent) (freeFoldPlan, error) {
	plan := freeFoldPlan{rebuilt: c.freeFoldRanges[:0], order: c.freeFoldOrder[:0]}
	appendRebuilt := func(lo, hi int) error {
		// An empty range emits nothing: a dirty segment whose extents have all
		// been consumed simply stops existing, and allocating a page for it
		// would leave an unstaged page that fails publication.
		for start := lo; start < hi; start += c.freeImagePerPage {
			end := min(start+c.freeImagePerPage, hi)
			if len(plan.rebuilt) == storeio.FreeLogMaxFoldSegments {
				return storeio.ErrRetiredExtentCapacity
			}
			plan.order = append(plan.order, freeFoldSlot{rebuilt: len(plan.rebuilt), fresh: true})
			plan.rebuilt = append(plan.rebuilt, [2]int{start, end})
		}
		return nil
	}
	if c.freeDirtyAll || len(c.freeSegments) == 0 {
		if err := appendRebuilt(0, len(live)); err != nil {
			return freeFoldPlan{}, err
		}
		c.freeFoldRanges, c.freeFoldOrder = plan.rebuilt, plan.order
		return plan, nil
	}
	for i, segment := range c.freeSegments {
		if !c.freeDirty[i] {
			plan.order = append(plan.order, freeFoldSlot{carried: segment})
			continue
		}
		// Segment i owns [FirstOffset(i), FirstOffset(i+1)); the first segment
		// also owns everything below its own first offset, and the last owns
		// everything above, so the partition covers the whole file however far
		// the free set has moved since the index was written.
		lo := 0
		if i != 0 {
			lo = freeLowerBound(live, segment.FirstOffset)
		}
		hi := len(live)
		if i+1 < len(c.freeSegments) {
			hi = freeLowerBound(live, c.freeSegments[i+1].FirstOffset)
		}
		if err := appendRebuilt(lo, hi); err != nil {
			return freeFoldPlan{}, err
		}
	}
	if len(plan.order) > storeio.FreeLogMaxIndexPages*c.freeIndexPerPage {
		return freeFoldPlan{}, storeio.ErrRetiredExtentCapacity
	}
	c.freeFoldRanges, c.freeFoldOrder = plan.rebuilt, plan.order
	return plan, nil
}

// freeLowerBound returns the first index in an offset-sorted set whose extent
// starts at or after offset.
func freeLowerBound(set []storeio.FreeExtent, offset uint64) int {
	lo, hi := 0, len(set)
	for lo < hi {
		middle := int(uint(lo+hi) >> 1)
		if set[middle].Offset < offset {
			lo = middle + 1
		} else {
			hi = middle
		}
	}
	return lo
}

// writeFreeSegments allocates and encodes the segment pages the plan calls for,
// and leaves c.freeNewSegments holding the complete published order: carried
// descriptors unchanged, rewritten ones pointing at the pages just staged.
func (c *Collection) writeFreeSegments(
	tx *storeio.WriteTransaction, plan freeFoldPlan, live []storeio.FreeExtent,
) error {
	var pages [storeio.FreeLogMaxFoldSegments]storeio.TransactionPage
	for i := range plan.rebuilt {
		page, err := tx.Allocate(storeio.PageFreeImage, uint32(c.options.PageSize), 0)
		if err != nil {
			return err
		}
		pages[i] = page
	}
	c.freeNewSegments = c.freeNewSegments[:0]
	for _, slot := range plan.order {
		if !slot.fresh {
			c.freeNewSegments = append(c.freeNewSegments, slot.carried)
			continue
		}
		span := plan.rebuilt[slot.rebuilt]
		extents := live[span[0]:span[1]]
		page := pages[slot.rebuilt]
		if _, err := storeio.EncodeFreeImagePage(page.Bytes(), storeio.FreeLogHeader{
			StoreID: c.storeID, Generation: tx.Generation(),
			LogicalID: page.Ref().LogicalID, PageSize: page.Ref().Length,
		}, extents, tx.FileEnd(), tx.NextLogicalID()); err != nil {
			return err
		}
		if err := page.Stage(); err != nil {
			return err
		}
		largest := uint64(0)
		for _, extent := range extents {
			largest = max(largest, extent.Length)
		}
		c.freeNewSegments = append(c.freeNewSegments, storeio.FreeSegment{
			Ref: page.Ref(), FirstOffset: extents[0].Offset,
			LargestFree: largest, Count: uint32(len(extents)),
		})
	}
	return nil
}

// writeFreeIndex publishes the whole segment index and returns its newest page.
//
// The index is rewritten in full even though only some segments moved. That is
// the deliberate stopping point of this design: the index is smaller than the
// free set by the segment fan-out — 168 extents per segment, 70 segments per
// index page — so rewriting all of it costs a small fraction of rewriting the
// image, and stopping here avoids a third level whose only job would be to make
// this rewrite partial too.
func (c *Collection) writeFreeIndex(tx *storeio.WriteTransaction) (storeio.PageRef, error) {
	segments := c.freeNewSegments
	if len(segments) == 0 {
		return storeio.PageRef{}, nil
	}
	pageCount := freeLogPageCount(len(segments), c.freeIndexPerPage)
	if pageCount > storeio.FreeLogMaxIndexPages {
		return storeio.PageRef{}, storeio.ErrRetiredExtentCapacity
	}
	c.freeNewIndex = c.freeNewIndex[:0]
	var prev storeio.PageRef
	for i := range pageCount {
		lower := i * c.freeIndexPerPage
		upper := min(lower+c.freeIndexPerPage, len(segments))
		page, err := tx.Allocate(storeio.PageFreeIndex, uint32(c.options.PageSize), 0)
		if err != nil {
			return storeio.PageRef{}, err
		}
		if _, err := storeio.EncodeFreeIndexPage(page.Bytes(), storeio.FreeLogHeader{
			StoreID: c.storeID, Generation: tx.Generation(),
			LogicalID: page.Ref().LogicalID, PageSize: page.Ref().Length,
		}, segments[lower:upper], prev, tx.FileEnd(), tx.NextLogicalID()); err != nil {
			return storeio.PageRef{}, err
		}
		if err := page.Stage(); err != nil {
			return storeio.PageRef{}, err
		}
		c.freeNewIndex = append(c.freeNewIndex, page.Ref())
		prev = page.Ref()
	}
	return prev, nil
}

// writeFreeDeltaChain allocates the diff's pages and then encodes them, in that
// order. That is the whole trick: a delta page allocated before its content is
// fixed can record its own allocation, so the diff is complete without a second
// commit to describe the first one. p pages hold d records, allocating them
// adds at most p records, and recomputing converges — a shape a tree cannot
// have, because writing a tree may split it, splitting allocates, and
// allocating changes the shape again.
func (c *Collection) writeFreeDeltaChain(
	tx *storeio.WriteTransaction, prev, indexHead storeio.PageRef, editStart int, folded bool,
) (freeLogCommit, error) {
	var pages [storeio.FreeLogMaxDeltaPages]storeio.TransactionPage
	allocated, rounds := 0, 0
	for {
		deltas := c.freeDeltas[:0]
		// A fold's segments already carry everything the pending list described,
		// so replaying it on top would restate settled facts and could only
		// disagree with them.
		if !folded {
			deltas = append(deltas, c.freePending...)
		}
		deltas, err := c.appendFreeAllocationDeltas(deltas, tx.ReuseEdits(), editStart)
		if err != nil {
			return freeLogCommit{}, err
		}
		c.freeDeltas = deltas
		need := max(1, freeLogPageCount(len(deltas), c.freeDeltaPerPage))
		if need <= allocated {
			break
		}
		if need > storeio.FreeLogMaxDeltaPages {
			return freeLogCommit{}, storeio.ErrRetiredExtentCapacity
		}
		for allocated < need {
			page, allocErr := tx.Allocate(storeio.PageFreeDelta, uint32(c.options.PageSize), 0)
			if allocErr != nil {
				return freeLogCommit{}, allocErr
			}
			pages[allocated] = page
			allocated++
		}
		rounds++
		if rounds == freeLogAllocationRounds {
			// Appending consumes no free extent, so it adds no record and the
			// next round is guaranteed to be the last. Termination stops
			// depending on the convergence argument being right.
			tx.DisableReuse()
		}
	}
	for i := range allocated {
		lower := min(i*c.freeDeltaPerPage, len(c.freeDeltas))
		upper := min(lower+c.freeDeltaPerPage, len(c.freeDeltas))
		link := prev
		if i != 0 {
			link = pages[i-1].Ref()
		}
		if _, err := storeio.EncodeFreeDeltaPage(pages[i].Bytes(), storeio.FreeLogHeader{
			StoreID: c.storeID, Generation: tx.Generation(),
			LogicalID: pages[i].Ref().LogicalID, PageSize: pages[i].Ref().Length,
		}, c.freeDeltas[lower:upper], link, indexHead, tx.FileEnd(), tx.NextLogicalID()); err != nil {
			return freeLogCommit{}, err
		}
		if err := pages[i].Stage(); err != nil {
			return freeLogCommit{}, err
		}
		c.freeNewDelta = append(c.freeNewDelta, pages[i].Ref())
	}
	head := pages[allocated-1]
	return freeLogCommit{
		head: head.Ref(), checksum: storeio.PageChecksum(head.Bytes()),
		changed: true, folded: folded,
	}, nil
}

// appendFreeAllocationDeltas records what this transaction took from the free
// set. edits before start are already reflected in a base image written during
// this same commit and must not be restated.
func (c *Collection) appendFreeAllocationDeltas(
	dst []storeio.FreeDelta, edits []storeio.ReuseEdit, start int,
) ([]storeio.FreeDelta, error) {
	if start >= len(edits) {
		return dst, nil
	}
	c.freeAllocStamp++
	if c.freeAllocStamp == 0 {
		clear(c.freeAllocMark)
		c.freeAllocStamp = 1
	}
	// Walk the journal newest first and keep one record per extent. An extent
	// allocated from several times in one commit needs a single record saying
	// where it ended up, not one per allocation.
	for i := len(edits) - 1; i >= start; i-- {
		index := int(edits[i].Index)
		if index >= len(c.freeAllocMark) || index >= len(c.reusable) {
			return dst, fmt.Errorf("%w: reuse journal index", storeio.ErrInvalidWrite)
		}
		if c.freeAllocMark[index] == c.freeAllocStamp {
			continue
		}
		c.freeAllocMark[index] = c.freeAllocStamp
		if current := c.reusable[index]; current.Length != 0 {
			c.markFreeDirty(current.Offset)
			dst = append(dst, storeio.FreeDelta{Op: storeio.FreeOpSet, Extent: current})
			continue
		}
		c.markFreeDirty(edits[i].Before.Offset)
		// The extent was consumed whole, so the in-memory entry is zeroed and no
		// longer carries its offset. The journal still does, because allocation
		// takes from an extent's tail precisely so the offset never moves.
		dst = append(dst, storeio.FreeDelta{
			Op: storeio.FreeOpDelete, Extent: storeio.FreeExtent{Offset: edits[i].Before.Offset},
		})
	}
	return dst, nil
}

// retireFreeLogPages hands the superseded pages to the reclaimer.
//
// Only the segments this fold actually rewrote are retired. A segment carried
// forward by reference is still live — it is named by the new index — and
// retiring it would advertise as free the very bytes the next open reads the
// free set out of. That distinction is new with the segment index: when the
// image was a linked list every fold replaced every page, so retiring the whole
// image was correct by construction and there was nothing to get wrong.
//
// Retiring at the outgoing generation fences these pages exactly as state roots
// are fenced: they stay reserved until neither an active reader nor the
// alternate superblock can still name the generation that referenced them.
func (c *Collection) retireFreeLogPages(state *fileStoreState, folded bool) error {
	appendRef := func(ref storeio.PageRef) error {
		if len(c.retireScratch) == cap(c.retireScratch) {
			return storeio.ErrRetiredExtentCapacity
		}
		c.retireScratch = append(c.retireScratch, storeio.FreeExtent{
			Offset: ref.Offset, Length: uint64(ref.Length),
			RetiredGeneration: state.root.Generation,
		})
		return nil
	}
	if folded {
		for i, segment := range c.freeSegments {
			if !c.freeDirtyAll && !c.freeDirty[i] {
				continue
			}
			if err := appendRef(segment.Ref); err != nil {
				return err
			}
		}
		for _, ref := range c.freeIndexPages {
			if err := appendRef(ref); err != nil {
				return err
			}
		}
	}
	for _, ref := range c.freeDeltaPages {
		if err := appendRef(ref); err != nil {
			return err
		}
	}
	return nil
}

// commitFreeLog adopts a published commit's chain and drops the diff it carried
// to disk. Nothing here may run before Publish returns.
func (c *Collection) commitFreeLog(commit freeLogCommit) {
	if !commit.changed {
		return
	}
	if commit.folded {
		c.freeSegments = append(c.freeSegments[:0], c.freeNewSegments...)
		c.freeIndexPages = append(c.freeIndexPages[:0], c.freeNewIndex...)
		c.freeDeltaPages = append(c.freeDeltaPages[:0], c.freeNewDelta...)
		c.resetFreeDirty()
		c.freeFoldRequired = false
	} else {
		c.freeDeltaPages = append(c.freeDeltaPages, c.freeNewDelta...)
	}
	c.freePending = c.freePending[:0]
}

// resetFreeDirty declares every published segment to match the in-memory set,
// which is true exactly after a fold has written the ones that did not.
func (c *Collection) resetFreeDirty() {
	c.freeDirty = append(c.freeDirty[:0], make([]bool, len(c.freeSegments))...)
	c.freeDirtyCount = 0
	c.freeDirtyAll = len(c.freeSegments) == 0
}

// segmentOfFreeOffset returns the published segment that owns offset, or -1 when
// no index has been published yet.
//
// The first segment owns everything below its own first offset and the last
// owns everything above, so every offset in the file has exactly one owner
// however far the free set has drifted from the index. Routing a change to one
// owner is what lets a fold rewrite one segment instead of all of them.
func (c *Collection) segmentOfFreeOffset(offset uint64) int {
	if len(c.freeSegments) == 0 {
		return -1
	}
	lo, hi := 0, len(c.freeSegments)
	for lo < hi {
		middle := int(uint(lo+hi) >> 1)
		if c.freeSegments[middle].FirstOffset <= offset {
			lo = middle + 1
		} else {
			hi = middle
		}
	}
	if lo == 0 {
		return 0
	}
	return lo - 1
}

// markFreeDirty records that the segment owning offset no longer matches its
// durable page. Marking is deliberately conservative: a segment marked without
// having changed costs one page rewrite at the next fold, while a segment that
// changed without being marked is published stale, and a stale segment
// advertises space a live page occupies.
func (c *Collection) markFreeDirty(offset uint64) {
	if c.freeDirtyAll {
		return
	}
	index := c.segmentOfFreeOffset(offset)
	if index < 0 {
		c.freeDirtyAll = true
		return
	}
	if index < len(c.freeDirty) && !c.freeDirty[index] {
		c.freeDirty[index] = true
		c.freeDirtyCount++
	}
}

// freeDirtySegments reports how many segments the next fold would rewrite.
func (c *Collection) freeDirtySegments() int {
	if c.freeDirtyAll {
		return max(1, freeLogPageCount(len(c.reusable), c.freeImagePerPage))
	}
	return c.freeDirtyCount
}

// finalizeReusable drops the entries this commit consumed whole. Their removal
// is already on disk as a delete record, so the in-memory set and the durable
// set agree again the moment this returns.
func (c *Collection) finalizeReusable() {
	out := c.reusable[:0]
	for _, extent := range c.reusable {
		if extent.Length != 0 {
			out = append(out, extent)
		}
	}
	clear(c.reusable[len(out):])
	c.reusable = out
}

func (c *Collection) liveReusable() int {
	live := 0
	for _, extent := range c.reusable {
		if extent.Length != 0 {
			live++
		}
	}
	return live
}

// freeFoldThreshold is the chain length past which folding is worth its cost.
//
// A delta chain is live space and is read in full at every open, so its cost
// grows with every commit; a fold's cost does not. Break-even is therefore the
// size of the write the fold would make: once the chain is longer than the
// dirty segments plus the index that would replace it, the chain is pure
// overhead.
//
// This is where the segment index changes the arithmetic rather than merely the
// cost. The old threshold was the size of the whole image, because the whole
// image was what a fold rewrote, so a store with a large free set folded rarely
// and paid a large write when it did. The fold write is now bounded by the fold
// reserve however large the free set is, so the threshold no longer grows with
// the store and a terabyte store folds as cheaply as a megabyte one.
func (c *Collection) freeFoldThreshold() int {
	return min(storeio.FreeLogMaxChainPages,
		max(freeLogMinFoldChain, c.freeDirtySegments()+len(c.freeIndexPages)))
}

func freeLogPageCount(records, perPage int) int {
	if records <= 0 || perPage <= 0 {
		return 0
	}
	return (records + perPage - 1) / perPage
}
