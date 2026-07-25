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
		c.freeImagePages = append(c.freeImagePages[:0], pages.Image...)
		c.freeDeltaPages = append(c.freeDeltaPages[:0], pages.Delta...)
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
	// Once a fold is required the list is dead weight: a fold writes the whole
	// set straight from memory, so nothing accumulated here can still matter.
	// Overflowing into a fold is also what keeps the list bounded across a run
	// of consecutive aborts.
	if c.freeFoldRequired {
		return
	}
	if len(c.freePending) == cap(c.freePending) {
		c.freeFoldRequired = true
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
	c.freeNewImage = c.freeNewImage[:0]
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
	if c.freeFoldRequired || need > min(room, storeio.FreeLogMaxDeltaPages) ||
		len(c.freeDeltaPages)+need > c.freeFoldThreshold(live) {
		return c.foldFreeLog(tx, state, live)
	}
	var imageHead storeio.PageRef
	if len(c.freeImagePages) != 0 {
		imageHead = c.freeImagePages[0]
	}
	return c.writeFreeDeltaChain(tx, state.freeHead, imageHead, 0, false)
}

// foldFreeLog replaces the chain with a fresh image plus a one-link chain that
// names it, and retires everything the old chain occupied.
//
// The image is dumped straight from c.reusable rather than replayed forward
// from the old chain. c.reusable is already the complete authoritative set; an
// incremental rebuild would only add a second, less direct way to be wrong, and
// it is the disagreement between the two that corrupts.
func (c *Collection) foldFreeLog(
	tx *storeio.WriteTransaction, state *fileStoreState, live int,
) (freeLogCommit, error) {
	if live == 0 {
		// An empty free set is fully described by publishing no free reference
		// at all: a replay of nothing is the empty set, which is the right
		// answer rather than a missing one.
		if err := c.retireFreeLogPages(state); err != nil {
			return freeLogCommit{}, err
		}
		return freeLogCommit{changed: true, folded: true}, nil
	}
	imagePages := freeLogPageCount(live, c.freeImagePerPage)
	if imagePages > storeio.FreeLogMaxImagePages {
		return freeLogCommit{}, storeio.ErrRetiredExtentCapacity
	}
	var pages [storeio.FreeLogMaxImagePages]storeio.TransactionPage
	for i := range imagePages {
		page, err := tx.Allocate(storeio.PageFreeImage, uint32(c.options.PageSize), 0)
		if err != nil {
			return freeLogCommit{}, err
		}
		pages[i] = page
	}
	// Everything the image pages themselves consumed is inside the image, since
	// the content is taken after they are allocated. Only what the delta pages
	// consume from here on needs a record, which is why the fold's own diff is a
	// handful of entries and always fits one page.
	editStart := len(tx.ReuseEdits())
	extents := c.freeImageScratch[:0]
	for _, extent := range c.reusable {
		if extent.Length != 0 {
			extents = append(extents, extent)
		}
	}
	c.freeImageScratch = extents
	for i := range imagePages {
		lower := min(i*c.freeImagePerPage, len(extents))
		upper := min(lower+c.freeImagePerPage, len(extents))
		var next storeio.PageRef
		if i+1 < imagePages {
			next = pages[i+1].Ref()
		}
		if _, err := storeio.EncodeFreeImagePage(pages[i].Bytes(), storeio.FreeLogHeader{
			StoreID: c.storeID, Generation: tx.Generation(),
			LogicalID: pages[i].Ref().LogicalID, PageSize: pages[i].Ref().Length,
		}, extents[lower:upper], next, tx.FileEnd(), tx.NextLogicalID()); err != nil {
			return freeLogCommit{}, err
		}
		if err := pages[i].Stage(); err != nil {
			return freeLogCommit{}, err
		}
		c.freeNewImage = append(c.freeNewImage, pages[i].Ref())
	}
	if err := c.retireFreeLogPages(state); err != nil {
		return freeLogCommit{}, err
	}
	return c.writeFreeDeltaChain(tx, storeio.PageRef{}, pages[0].Ref(), editStart, true)
}

// writeFreeDeltaChain allocates the diff's pages and then encodes them, in that
// order. That is the whole trick: a delta page allocated before its content is
// fixed can record its own allocation, so the diff is complete without a second
// commit to describe the first one. p pages hold d records, allocating them
// adds at most p records, and recomputing converges — a shape a tree cannot
// have, because writing a tree may split it, splitting allocates, and
// allocating changes the shape again.
func (c *Collection) writeFreeDeltaChain(
	tx *storeio.WriteTransaction, prev, imageHead storeio.PageRef, editStart int, folded bool,
) (freeLogCommit, error) {
	var pages [storeio.FreeLogMaxDeltaPages]storeio.TransactionPage
	allocated, rounds := 0, 0
	for {
		deltas := c.freeDeltas[:0]
		// A fold's image already carries everything the pending list described,
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
		}, c.freeDeltas[lower:upper], link, imageHead, tx.FileEnd(), tx.NextLogicalID()); err != nil {
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
			dst = append(dst, storeio.FreeDelta{Op: storeio.FreeOpSet, Extent: current})
			continue
		}
		// The extent was consumed whole, so the in-memory entry is zeroed and no
		// longer carries its offset. The journal still does, because allocation
		// takes from an extent's tail precisely so the offset never moves.
		dst = append(dst, storeio.FreeDelta{
			Op: storeio.FreeOpDelete, Extent: storeio.FreeExtent{Offset: edits[i].Before.Offset},
		})
	}
	return dst, nil
}

// retireFreeLogPages hands the superseded chain to the reclaimer. Retiring at
// the outgoing generation fences them exactly as state roots are fenced: the
// pages stay reserved until neither an active reader nor the alternate
// superblock can still name the generation that referenced them.
func (c *Collection) retireFreeLogPages(state *fileStoreState) error {
	for _, group := range [2][]storeio.PageRef{c.freeImagePages, c.freeDeltaPages} {
		for _, ref := range group {
			if len(c.retireScratch) == cap(c.retireScratch) {
				return storeio.ErrRetiredExtentCapacity
			}
			c.retireScratch = append(c.retireScratch, storeio.FreeExtent{
				Offset: ref.Offset, Length: uint64(ref.Length),
				RetiredGeneration: state.root.Generation,
			})
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
		c.freeImagePages = append(c.freeImagePages[:0], c.freeNewImage...)
		c.freeDeltaPages = append(c.freeDeltaPages[:0], c.freeNewDelta...)
		c.freeFoldRequired = false
	} else {
		c.freeDeltaPages = append(c.freeDeltaPages, c.freeNewDelta...)
	}
	c.freePending = c.freePending[:0]
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
// grows with every commit; a fold's cost is one rewrite of the image and does
// not. Break-even is therefore the size of the image: once the chain is longer
// than the image that would replace it, the chain is pure overhead. Anchoring
// the threshold to a constant instead cost a small store thirty-four pages of
// permanent chain — more than the store itself held — while a store with a
// large free set folded far too eagerly.
func (c *Collection) freeFoldThreshold(live int) int {
	return min(storeio.FreeLogMaxChainPages,
		max(freeLogMinFoldChain, freeLogPageCount(live, c.freeImagePerPage)))
}

func freeLogPageCount(records, perPage int) int {
	if records <= 0 || perPage <= 0 {
		return 0
	}
	return (records + perPage - 1) / perPage
}
