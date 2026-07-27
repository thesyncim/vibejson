package durable

import (
	"errors"
	"fmt"

	"github.com/thesyncim/vibejson/internal/storeio"
)

// fileBufferedInplaceFrame ties one re-homed canonical frame to the staging
// descriptor that will receive its complete after-image immediately before the
// writer-held checkpoint cut.
type fileBufferedInplaceFrame struct {
	ref      storeio.PageRef
	deferred storeio.DeferredPage
}

// tryBufferedFileInplace replaces one same-size inline value in the current
// chunk primary only after the frame has already taken an ordinary COW update
// in this checkpoint window. The first touch does no in-place reservation and
// asks the caller to remember the newly published COW ref. The second touch
// re-homes that ref; later touches patch the re-homed canonical frame.
//
// handled=false is an honest eligibility or exclusive-ownership fallback. The
// caller proceeds through putLocked unchanged.
func (c *Collection) tryBufferedFileInplace(
	state *fileStoreState,
	src []byte,
	location storeio.KeyLocation,
	match *fileFingerprintMatch,
) (handled bool, trackFirstTouch bool, err error) {
	if c == nil || state == nil || match == nil || !c.buffered() {
		return false, false, nil
	}
	c.bufferedInplaceAttempts.Add(1)
	fallback := func() (bool, bool, error) {
		c.bufferedInplaceFallbacks.Add(1)
		return false, false, nil
	}
	if len(c.options.indexes) != 0 ||
		len(c.options.float64Columns) != 0 ||
		c.options.Collection.Schema != nil ||
		match.documentRef.Kind != storeio.PageDocument ||
		match.view.grouped ||
		match.value.value.Overflow != (storeio.PageRef{}) ||
		len(match.value.value.Inline) == 0 ||
		len(match.value.value.Inline) != len(src) {
		return fallback()
	}
	valueOffset, ok := match.document.OffsetOf(match.value.value.Inline)
	if !ok {
		return false, false, storeio.ErrDocumentPageCorrupt
	}

	registered := c.bufferedInplaceContains(match.documentRef)
	seen := c.bufferedFirstTouchContains(match.documentRef)
	if !registered && !seen {
		if !c.bufferedFirstTouchCapacityAvailable() {
			c.bufferedFirstTouchOverflows.Add(1)
			return fallback()
		}
		return false, true, nil
	}
	if err := c.ensureDirtyCapacityFor(
		c.options.singleDocumentTransactionPages,
		c.options.singleDocumentTransactionBytes,
	); err != nil {
		return false, false, err
	}
	// A capacity checkpoint clears the window. Re-evaluate after the fence so
	// this mutation becomes the first ordinary COW touch in the new window.
	if c.bufferedInplaceContains(match.documentRef) {
		handled, err = c.replaceBufferedFileInplace(
			state, src, valueOffset, match,
		)
		return handled, !handled && err == nil, err
	}
	if !c.takeBufferedFirstTouch(match.documentRef) {
		if !c.bufferedFirstTouchCapacityAvailable() {
			c.bufferedFirstTouchOverflows.Add(1)
			return fallback()
		}
		return false, true, nil
	}

	// Eligibility is entirely a collection-shape fact. With no exact indexes,
	// schema, or float64 scan columns configured, replacing an existing inline
	// value with the same number of bytes changes neither an auxiliary
	// projection nor the chunk's live mask. Chunk-zone bytes are deliberately
	// not inspected here: every durable chunk carries a summary, while zone
	// pruning is a process-wide query-planner switch rather than configured
	// collection maintenance. Consequently this replacement has no configured
	// zone projection whose summary could change.
	handled, err = c.rehomeBufferedFileInplace(
		state, src, location, valueOffset, match,
	)
	return handled, !handled && err == nil, err
}

func (c *Collection) replaceBufferedFileInplace(
	state *fileStoreState,
	src []byte,
	valueOffset uint32,
	match *fileFingerprintMatch,
) (handled bool, err error) {
	generation := state.root.Generation + 1
	if generation == 0 {
		return false, storeio.ErrGenerationOrder
	}
	tx, err := c.beginWriteTransaction(
		0,
		storeio.WriteTransactionOptions{
			StoreID:       c.storeID,
			Generation:    generation,
			PageSize:      uint32(c.options.PageSize),
			FileEnd:       state.super.FileEnd,
			NextLogicalID: state.root.NextLogicalID,
		},
	)
	if err != nil {
		return false, fmt.Errorf("vibejson: begin buffered in-place token: %w", err)
	}
	abort := true
	defer func() {
		if abort {
			err = errors.Join(err, tx.Abort())
		}
	}()

	nextRoot := state.root
	nextRoot.Generation = generation
	nextSuper := state.super
	nextSuper.Generation = generation
	nextState := &fileStoreState{
		root: nextRoot, super: nextSuper,
		keyRoot: state.keyRoot, chunkRoot: state.chunkRoot,
		indexRoot: state.indexRoot, freeHead: state.freeHead,
	}
	c.bufferedValueBefore = append(
		c.bufferedValueBefore[:0], match.value.value.Inline...,
	)
	before := c.bufferedValueBefore

	// Put, automatic capacity checkpoints, Flush, and Close all hold c.writer
	// in buffered mode. Consequently no checkpoint seal can overlap this
	// admission. snapshotGate closes the remaining race: once AnyActive is
	// false, no old-generation reader can appear before the patched root is
	// visible.
	c.snapshotGate.Lock()
	if c.leases.AnyActive() {
		c.snapshotGate.Unlock()
		c.bufferedInplaceFallbacks.Add(1)
		return false, nil
	}
	previousDirty, replaceErr := c.cache.ReplaceLeasedCanonicalDirty(
		&match.document,
		match.documentRef, match.documentRef,
		valueOffset, before, src, generation,
	)
	if replaceErr != nil {
		c.snapshotGate.Unlock()
		if errors.Is(replaceErr, storeio.ErrCanonicalPageBusy) ||
			errors.Is(replaceErr, storeio.ErrCanonicalPageChanged) {
			c.bufferedInplaceFallbacks.Add(1)
			return false, nil
		}
		return false, replaceErr
	}
	if publishErr := tx.PublishInline(nextRoot, c.inlineFree); publishErr != nil {
		restoreErr := c.cache.RestoreLeasedCanonicalDirty(
			&match.document,
			match.documentRef, match.documentRef,
			valueOffset, before, src, generation, previousDirty,
		)
		c.snapshotGate.Unlock()
		return false, fmt.Errorf(
			"vibejson: publish buffered in-place token: %w",
			errors.Join(publishErr, restoreErr),
		)
	}
	abort = false
	c.pageValidator.update(nextState)
	c.publishFileState(nextState)
	c.snapshotGate.Unlock()
	c.bufferedInplaceUpdates.Add(1)
	return true, nil
}

func (c *Collection) rehomeBufferedFileInplace(
	state *fileStoreState,
	src []byte,
	location storeio.KeyLocation,
	valueOffset uint32,
	match *fileFingerprintMatch,
) (handled bool, err error) {
	generation := state.root.Generation + 1
	if generation == 0 {
		return false, storeio.ErrGenerationOrder
	}
	if err := c.refreshReusableFor(
		state,
		c.options.singleDocumentTransactionPages,
		c.options.singleDocumentFreeFoldLimit,
	); err != nil {
		return false, fmt.Errorf("vibejson: refresh reusable extents: %w", err)
	}
	tx, err := c.beginWriteTransaction(
		c.options.singleDocumentTransactionPages,
		storeio.WriteTransactionOptions{
			StoreID: c.storeID, Generation: generation,
			PageSize: uint32(c.options.PageSize),
			FileEnd:  state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
			Reusable: c.reusable, ReuseJournal: c.reuseJournal,
			ReusableIndex:    &c.freeExtentIndex,
			ReusablePromoter: c.reusableExtentPromoter(),
		},
	)
	if err != nil {
		return false, fmt.Errorf("vibejson: begin buffered in-place rehome: %w", err)
	}
	abort := true
	retirementReserved := false
	defer func() {
		if abort {
			if retirementReserved {
				_ = c.reclaimer.CancelRetiredGeneration(state.root.Generation)
			}
			err = errors.Join(err, tx.Abort())
		}
	}()
	c.retireScratch = c.retireScratch[:0]
	c.retireRefScratch = c.retireRefScratch[:0]

	oldRef := match.documentRef
	documentPage, err := tx.Allocate(
		storeio.PageDocument, oldRef.Length, oldRef.LogicalID,
	)
	if err != nil {
		return false, fmt.Errorf("vibejson: allocate buffered canonical frame: %w", err)
	}
	deferred, err := documentPage.StageDeferred()
	if err != nil {
		return false, fmt.Errorf("vibejson: defer buffered canonical frame: %w", err)
	}
	chunkMutation, err := storeio.UpsertChunkTreeZone(
		c.cache, tx, state.chunkRoot, location.Chunk,
		documentPage.Ref(), nil,
		storeio.ChunkTreeBounds{
			FileEnd:       state.super.FileEnd,
			NextLogicalID: state.root.NextLogicalID,
		},
	)
	if err != nil {
		return false, err
	}
	if err := c.collectFileRetirements(
		state, oldRef, &match.view,
		storeio.PageKeyTreeMutation{}, chunkMutation, false, false,
	); err != nil {
		return false, fmt.Errorf("vibejson: collect retired extents: %w", err)
	}
	freeLog, err := c.syncFreeLogFor(
		tx, state, c.options.singleDocumentFreeFoldLimit,
	)
	if err != nil {
		return false, fmt.Errorf("vibejson: persist reusable extents: %w", err)
	}
	nextState, nextInline, err := c.stageFileState(
		tx, state, generation,
		state.root.ChunkHighWater, state.root.FreeChunkHint,
		state.root.DocumentCount, state.root.LiveChunks,
		chunkMutation.Root, state.keyRoot, state.indexRoot,
		state.root.Float64ScanHead, state.root.IndexGroupHead,
		freeLog.head, freeLog.checksum, freeLog.inline,
	)
	if err != nil {
		return false, err
	}
	if err := c.reserveFileRetirements(); err != nil {
		return false, fmt.Errorf("vibejson: reserve retired extents: %w", err)
	}
	retirementReserved = true
	if c.bufferedInplace == nil {
		c.bufferedInplace = make(
			[]fileBufferedInplaceFrame, 0,
			fileVisibilitySlots(c.options.QueueSlots),
		)
	}
	if len(c.bufferedInplace) == cap(c.bufferedInplace) {
		return false, storeio.ErrCheckpointRequired
	}

	c.bufferedValueBefore = append(
		c.bufferedValueBefore[:0], match.value.value.Inline...,
	)
	before := c.bufferedValueBefore
	newRef := documentPage.Ref()
	c.snapshotGate.Lock()
	if c.leases.AnyActive() {
		c.snapshotGate.Unlock()
		c.bufferedInplaceFallbacks.Add(1)
		return false, nil
	}
	previousDirty, replaceErr := c.cache.ReplaceLeasedCanonicalDirty(
		&match.document, oldRef, newRef,
		valueOffset, before, src, generation,
	)
	if replaceErr != nil {
		c.snapshotGate.Unlock()
		if errors.Is(replaceErr, storeio.ErrCanonicalPageBusy) ||
			errors.Is(replaceErr, storeio.ErrCanonicalPageChanged) {
			c.bufferedInplaceFallbacks.Add(1)
			return false, nil
		}
		return false, replaceErr
	}
	if publishErr := tx.PublishInlineRetiring(
		nextState.root, nextInline, c.retireRefScratch,
	); publishErr != nil {
		restoreErr := c.cache.RestoreLeasedCanonicalDirty(
			&match.document, oldRef, newRef,
			valueOffset, before, src, generation, previousDirty,
		)
		c.snapshotGate.Unlock()
		return false, fmt.Errorf(
			"vibejson: publish buffered canonical frame: %w",
			errors.Join(publishErr, restoreErr),
		)
	}
	abort = false
	c.bufferedInplace = append(c.bufferedInplace, fileBufferedInplaceFrame{
		ref: newRef, deferred: deferred,
	})
	c.pageValidator.update(nextState)
	c.publishFileState(nextState)
	c.cache.MarkUnreachable(c.retireRefScratch)
	c.snapshotGate.Unlock()

	c.finalizeReusable()
	c.commitFreeLog(freeLog)
	c.inlineFree = nextInline
	c.bufferedInplaceUpdates.Add(1)
	return true, nil
}

func (c *Collection) bufferedInplaceContains(
	ref storeio.PageRef,
) bool {
	for index := range c.bufferedInplace {
		if c.bufferedInplace[index].ref == ref {
			return true
		}
	}
	return false
}

func (c *Collection) bufferedFirstTouchContains(ref storeio.PageRef) bool {
	for index := range c.bufferedFirstTouches {
		if c.bufferedFirstTouches[index] == ref {
			return true
		}
	}
	return false
}

func (c *Collection) takeBufferedFirstTouch(ref storeio.PageRef) bool {
	for index := range c.bufferedFirstTouches {
		if c.bufferedFirstTouches[index] != ref {
			continue
		}
		last := len(c.bufferedFirstTouches) - 1
		c.bufferedFirstTouches[index] = c.bufferedFirstTouches[last]
		c.bufferedFirstTouches[last] = storeio.PageRef{}
		c.bufferedFirstTouches = c.bufferedFirstTouches[:last]
		return true
	}
	return false
}

func (c *Collection) bufferedFirstTouchCapacityAvailable() bool {
	return len(c.bufferedFirstTouches) < cap(c.bufferedFirstTouches)
}

func (c *Collection) rememberBufferedFirstTouch(ref storeio.PageRef) {
	if ref == (storeio.PageRef{}) || c.bufferedFirstTouchContains(ref) {
		return
	}
	if !c.bufferedFirstTouchCapacityAvailable() {
		c.bufferedFirstTouchOverflows.Add(1)
		c.bufferedInplaceFallbacks.Add(1)
		return
	}
	c.bufferedFirstTouches = append(c.bufferedFirstTouches, ref)
}

// captureBufferedInplaceLocked copies each still-reachable canonical frame
// once into its pre-reserved new-extent write. The caller holds c.writer, so
// no mutation can change bytes between capture and checkpoint authorization.
func (c *Collection) captureBufferedInplaceLocked() error {
	for index := range c.bufferedInplace {
		frame := c.bufferedInplace[index]
		needed, err := frame.deferred.NeedsCapture()
		if err != nil {
			return fmt.Errorf("vibejson: inspect buffered canonical frame: %w", err)
		}
		if !needed {
			continue
		}
		lease, err := c.cache.Acquire(frame.ref)
		if err != nil {
			return fmt.Errorf("vibejson: acquire buffered canonical frame: %w", err)
		}
		captureErr := frame.deferred.Capture(&lease)
		lease.Release()
		if captureErr != nil {
			return fmt.Errorf("vibejson: capture buffered canonical frame: %w", captureErr)
		}
	}
	return nil
}

func (c *Collection) clearBufferedInplaceLocked() {
	clear(c.bufferedInplace)
	c.bufferedInplace = c.bufferedInplace[:0]
	clear(c.bufferedFirstTouches)
	c.bufferedFirstTouches = c.bufferedFirstTouches[:0]
}

func (c *Collection) checkpointBufferedLocked() error {
	if err := c.captureBufferedInplaceLocked(); err != nil {
		return err
	}
	if err := c.committer.Flush(); err != nil {
		return err
	}
	c.cache.MarkDurable(c.committer.DurableGeneration())
	c.clearBufferedInplaceLocked()
	return nil
}
