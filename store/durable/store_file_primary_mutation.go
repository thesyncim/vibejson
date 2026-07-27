package durable

import (
	"errors"
	"fmt"

	vibejson "github.com/thesyncim/vibejson"
	"github.com/thesyncim/vibejson/internal/storeio"
)

type filePrimaryMutationPath struct {
	rootLease    storeio.PageLease
	branchLease  storeio.PageLease
	catalogLease storeio.PageLease
	tabletLease  storeio.PageLease
	anchorLease  storeio.PageLease
	leafLease    storeio.PageLease

	root    storeio.GlobalTabletCatalogNodeView
	branch  storeio.GlobalTabletCatalogNodeView
	catalog storeio.GlobalTabletCatalogNodeView
	tablet  storeio.GlobalTabletCatalogTabletRootView
	anchor  storeio.GlobalTabletCatalogAnchorView
	leaf    storeio.CommonPrimaryLeafView

	rootRoute    storeio.GlobalTabletCatalogNodeRoute
	catalogRoute storeio.GlobalTabletCatalogNodeRoute
	tabletRoute  storeio.GlobalTabletCatalogNodeRoute
	anchorRoute  storeio.GlobalTabletCatalogAnchorRoute
	leafRoute    storeio.SegmentedTabletRouterRoute
	branchRef    storeio.PageRef
	catalogRef   storeio.PageRef
	hasBranch    bool
}

func (p *filePrimaryMutationPath) Release() {
	if p == nil {
		return
	}
	p.leafLease.Release()
	p.anchorLease.Release()
	p.tabletLease.Release()
	p.catalogLease.Release()
	p.branchLease.Release()
	p.rootLease.Release()
}

func (c *Collection) acquirePrimaryMutationPath(
	path *filePrimaryMutationPath,
	state *fileStoreState,
	key []byte,
	resident storeio.ResidentPrimaryRoute,
) (err error) {
	if c == nil || path == nil || state == nil ||
		state.root.PrimaryRoot == (storeio.PageRef{}) {
		return fmt.Errorf(
			"%w: ordered primary mutation path",
			storeio.ErrGlobalTabletCatalogCorrupt,
		)
	}
	*path = filePrimaryMutationPath{}
	defer func() {
		if err != nil {
			path.Release()
		}
	}()
	bounds := storeio.GlobalTabletCatalogBounds{
		StoreID: c.storeID, SelectedRootGeneration: state.root.Generation,
		FileEnd:       state.super.FileEnd,
		NextLogicalID: state.root.NextLogicalID,
	}
	path.rootLease, err = c.cache.Acquire(state.root.PrimaryRoot)
	if err != nil {
		return err
	}
	path.root = storeio.AdmittedGlobalTabletCatalogNode(
		path.rootLease.Page(), bounds,
	)
	if path.root.Level() != storeio.GlobalTabletCatalogRoot {
		return storeio.ErrGlobalTabletCatalogCorrupt
	}
	path.rootRoute = path.root.Route(key)
	if path.rootRoute.Ref == (storeio.PageRef{}) {
		return storeio.ErrGlobalTabletCatalogCorrupt
	}

	childRoute := path.rootRoute
	if path.root.ChildLevel() == storeio.GlobalTabletCatalogBranch {
		path.hasBranch = true
		path.branchRef = path.rootRoute.Ref
		path.branchLease, err = c.cache.Acquire(path.rootRoute.Ref)
		if err != nil {
			return err
		}
		path.branch = storeio.AdmittedGlobalTabletCatalogNode(
			path.branchLease.Page(), bounds,
		)
		if path.branch.Level() != storeio.GlobalTabletCatalogBranch {
			return storeio.ErrGlobalTabletCatalogCorrupt
		}
		path.catalogRoute = path.branch.Route(key)
		childRoute = path.catalogRoute
	} else {
		path.catalogRoute = path.rootRoute
	}
	if childRoute.Ref == (storeio.PageRef{}) {
		return storeio.ErrGlobalTabletCatalogCorrupt
	}
	path.catalogRef = childRoute.Ref
	path.catalogLease, err = c.cache.Acquire(childRoute.Ref)
	if err != nil {
		return err
	}
	path.catalog = storeio.AdmittedGlobalTabletCatalogNode(
		path.catalogLease.Page(), bounds,
	)
	if path.catalog.Level() != storeio.GlobalTabletCatalogLeaf {
		return storeio.ErrGlobalTabletCatalogCorrupt
	}
	path.tabletRoute = path.catalog.Route(key)
	if path.tabletRoute.Ref == (storeio.PageRef{}) {
		return storeio.ErrGlobalTabletCatalogCorrupt
	}
	path.tabletLease, err = c.cache.Acquire(path.tabletRoute.Ref)
	if err != nil {
		return err
	}
	path.tablet = storeio.AdmittedGlobalTabletCatalogTabletRoot(
		path.tabletLease.Page(), bounds,
	)
	path.anchorRoute, _ = path.tablet.RouteAnchor(key)
	if path.anchorRoute.Ref == (storeio.PageRef{}) {
		return storeio.ErrGlobalTabletCatalogCorrupt
	}
	path.anchorLease, err = c.cache.Acquire(path.anchorRoute.Ref)
	if err != nil {
		return err
	}
	path.anchor = storeio.AdmittedGlobalTabletCatalogAnchor(
		path.anchorLease.Page(), &path.tablet, path.anchorRoute.PageID,
	)
	hash := storeio.KeyHashBytes(c.storeID, key)
	path.leafRoute, _ = path.anchor.RouteHashed(hash, key)
	if path.leafRoute.Ref == (storeio.PageRef{}) ||
		path.leafRoute.Ref != resident.Ref ||
		path.leafRoute.Bucket != resident.Bucket {
		return storeio.ErrSegmentedTabletRouterCorrupt
	}
	path.leafLease, err = c.cache.Acquire(path.leafRoute.Ref)
	if err != nil {
		return err
	}
	path.leaf = storeio.AdmittedCommonPrimaryLeaf(
		path.leafLease.Page(), c.storeID, path.leafRoute.Bucket,
		storeio.CommonPrimaryLeafBounds{
			FileEnd:           state.super.FileEnd,
			NextLogicalID:     state.root.NextLogicalID,
			AllocationQuantum: state.root.PageSize,
		},
	)
	return nil
}

func (c *Collection) currentPrimaryResidentRoute(
	state *fileStoreState,
	key []byte,
) (storeio.ResidentPrimaryRoute, error) {
	if c.primaryRouter == nil ||
		c.primaryRouter.Generation() != state.root.Generation {
		return storeio.ResidentPrimaryRoute{},
			storeio.ErrSegmentedTabletRouterCorrupt
	}
	route, ok := c.primaryRouter.Route(key)
	if !ok || c.primaryRouter.Generation() != state.root.Generation {
		return storeio.ResidentPrimaryRoute{},
			storeio.ErrSegmentedTabletRouterCorrupt
	}
	return route, nil
}

func (c *Collection) putPrimary(
	key string,
	src []byte,
) (created bool, err error) {
	c.writer.Lock()
	var generation uint64
	defer func() {
		wait := generation != 0 && c.synchronous()
		if wait {
			c.durabilityWait.Add(1)
		}
		c.writer.Unlock()
		if wait {
			err = errors.Join(err, c.waitPublished(generation))
			c.durabilityWait.Done()
		}
	}()
	if c.closed {
		return false, ErrClosed
	}
	if failure := c.PersistenceError(); failure != nil {
		return false, failure
	}
	if len(key) == 0 ||
		len(key) > c.options.MaxKeyBytes ||
		len(key) > storeio.CommonPrimaryLeafMaxKeyBytes {
		return false, ErrKeyTooLarge
	}
	if len(src) == 0 ||
		len(src) > c.options.MaxDocumentBytes ||
		len(src) > c.options.InlineValueBytes {
		return false, ErrDocumentTooLarge
	}
	if err := vibejson.Validate(src); err != nil {
		return false, err
	}
	state := c.state.Load()
	if state == nil || state.root.PrimaryRoot == (storeio.PageRef{}) {
		return false, ErrClosed
	}
	c.pointKeyScratch = append(c.pointKeyScratch[:0], key...)
	keyBytes := c.pointKeyScratch
	resident, err := c.currentPrimaryResidentRoute(state, keyBytes)
	if err != nil {
		return false, err
	}
	leafLease, err := c.primaryRouter.AcquireLeaf(c.cache, resident)
	if err != nil {
		return false, err
	}
	leaf := storeio.AdmittedCommonPrimaryLeaf(
		leafLease.Page(), c.storeID, resident.Bucket,
		storeio.CommonPrimaryLeafBounds{
			FileEnd:           state.super.FileEnd,
			NextLogicalID:     state.root.NextLogicalID,
			AllocationQuantum: state.root.PageSize,
		},
	)
	_, raw, overflow, found := leaf.LookupRawHashed(
		resident.Hash, keyBytes,
	)
	if overflow {
		leafLease.Release()
		return false, fmt.Errorf(
			"%w: ordered primary overflow mutation",
			ErrPrimaryCutoverUnsupported,
		)
	}
	created = !found
	trackFirstTouch := false
	if found {
		handled, track, inplaceErr := c.tryBufferedPrimaryInplace(
			state, src, raw, resident.Ref, &leafLease,
		)
		if inplaceErr != nil {
			leafLease.Release()
			return false, inplaceErr
		}
		trackFirstTouch = track
		if handled {
			leafLease.Release()
			generation = state.root.Generation + 1
			return false, nil
		}
	}
	leafLease.Release()
	if err := c.ensureDirtyCapacityFor(
		c.options.singleDocumentTransactionPages,
		c.options.singleDocumentTransactionBytes,
	); err != nil {
		return false, err
	}
	var path filePrimaryMutationPath
	if err := c.acquirePrimaryMutationPath(
		&path, state, keyBytes, resident,
	); err != nil {
		return false, err
	}
	defer path.Release()
	slot, _, overflow, resolvedFound := path.leaf.LookupRawHashed(
		path.leafRoute.Hash, keyBytes,
	)
	if overflow {
		return false, fmt.Errorf(
			"%w: ordered primary overflow mutation",
			ErrPrimaryCutoverUnsupported,
		)
	}
	if resolvedFound != found {
		return false, storeio.ErrCommonPrimaryLeafCorrupt
	}
	nextLeaf, becameEmpty, filledEmpty, err :=
		c.cowPrimaryMutation(
			state, keyBytes, src, false, found, slot,
			resident, &path,
		)
	if err != nil {
		return false, err
	}
	if trackFirstTouch {
		c.rememberBufferedFirstTouch(nextLeaf)
	}
	if becameEmpty && c.primaryRouter.MarkEmpty(resident) {
		c.primaryEmptyLeaves.Add(1)
	}
	if filledEmpty && c.primaryRouter.ClearEmpty(resident) {
		c.removePrimaryEmptyLeaf()
	}
	generation = state.root.Generation + 1
	return created, nil
}

func (c *Collection) deletePrimary(
	key string,
) (deleted bool, err error) {
	c.writer.Lock()
	var generation uint64
	defer func() {
		wait := generation != 0 && c.synchronous()
		if wait {
			c.durabilityWait.Add(1)
		}
		c.writer.Unlock()
		if wait {
			err = errors.Join(err, c.waitPublished(generation))
			c.durabilityWait.Done()
		}
	}()
	if c.closed {
		return false, ErrClosed
	}
	if failure := c.PersistenceError(); failure != nil {
		return false, failure
	}
	if len(key) == 0 ||
		len(key) > c.options.MaxKeyBytes ||
		len(key) > storeio.CommonPrimaryLeafMaxKeyBytes {
		return false, ErrKeyTooLarge
	}
	state := c.state.Load()
	if state == nil || state.root.PrimaryRoot == (storeio.PageRef{}) {
		return false, nil
	}
	c.pointKeyScratch = append(c.pointKeyScratch[:0], key...)
	keyBytes := c.pointKeyScratch
	resident, err := c.currentPrimaryResidentRoute(state, keyBytes)
	if err != nil {
		return false, err
	}
	var path filePrimaryMutationPath
	if err := c.acquirePrimaryMutationPath(
		&path, state, keyBytes, resident,
	); err != nil {
		return false, err
	}
	defer path.Release()
	slot, _, overflow, found := path.leaf.LookupRawHashed(
		path.leafRoute.Hash, keyBytes,
	)
	if !found {
		return false, nil
	}
	if overflow {
		return false, fmt.Errorf(
			"%w: ordered primary overflow mutation",
			ErrPrimaryCutoverUnsupported,
		)
	}
	if err := c.ensureDirtyCapacityFor(
		c.options.singleDocumentTransactionPages,
		c.options.singleDocumentTransactionBytes,
	); err != nil {
		return false, err
	}
	_, becameEmpty, _, err := c.cowPrimaryMutation(
		state, keyBytes, nil, true, true, slot,
		resident, &path,
	)
	if err != nil {
		return false, err
	}
	if becameEmpty && c.primaryRouter.MarkEmpty(resident) {
		c.primaryEmptyLeaves.Add(1)
	}
	generation = state.root.Generation + 1
	return true, nil
}

func (c *Collection) tryBufferedPrimaryInplace(
	state *fileStoreState,
	src, before []byte,
	ref storeio.PageRef,
	leafLease *storeio.PageLease,
) (handled bool, trackFirstTouch bool, err error) {
	if c == nil || state == nil || leafLease == nil || !c.buffered() {
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
		len(before) == 0 || len(before) != len(src) {
		return fallback()
	}
	valueOffset, ok := leafLease.OffsetOf(before)
	if !ok {
		return false, false, storeio.ErrCommonPrimaryLeafCorrupt
	}
	if !c.bufferedFirstTouchContains(ref) {
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
	if !c.bufferedFirstTouchContains(ref) {
		if !c.bufferedFirstTouchCapacityAvailable() {
			c.bufferedFirstTouchOverflows.Add(1)
			return fallback()
		}
		return false, true, nil
	}
	handled, err = c.replaceBufferedPrimaryInplace(
		state, src, before, valueOffset, ref, leafLease,
	)
	if handled || err != nil {
		return handled, false, err
	}
	c.takeBufferedFirstTouch(ref)
	return false, true, nil
}

func (c *Collection) replaceBufferedPrimaryInplace(
	state *fileStoreState,
	src, old []byte,
	valueOffset uint32,
	ref storeio.PageRef,
	leafLease *storeio.PageLease,
) (handled bool, err error) {
	generation := state.root.Generation + 1
	if generation == 0 {
		return false, storeio.ErrGenerationOrder
	}
	tx, err := c.beginWriteTransaction(
		0,
		storeio.WriteTransactionOptions{
			StoreID: c.storeID, Generation: generation,
			PageSize:      uint32(c.options.PageSize),
			FileEnd:       state.super.FileEnd,
			NextLogicalID: state.root.NextLogicalID,
		},
	)
	if err != nil {
		return false, fmt.Errorf(
			"vibejson: begin buffered primary in-place token: %w", err,
		)
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
	c.bufferedValueBefore = append(c.bufferedValueBefore[:0], old...)
	before := c.bufferedValueBefore

	c.snapshotGate.Lock()
	if c.leases.AnyActive() {
		c.snapshotGate.Unlock()
		c.bufferedInplaceFallbacks.Add(1)
		return false, nil
	}
	previousDirty, replaceErr := c.cache.ReplaceLeasedCanonicalDirty(
		leafLease,
		ref, ref,
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
			leafLease,
			ref, ref,
			valueOffset, before, src, generation, previousDirty,
		)
		c.snapshotGate.Unlock()
		return false, fmt.Errorf(
			"vibejson: publish buffered primary in-place token: %w",
			errors.Join(publishErr, restoreErr),
		)
	}
	abort = false
	c.primaryRouter.AdvanceGeneration(generation)
	c.pageValidator.update(nextState)
	c.publishFileState(nextState)
	c.snapshotGate.Unlock()
	c.bufferedInplaceUpdates.Add(1)
	return true, nil
}

func (c *Collection) cowPrimaryMutation(
	state *fileStoreState,
	key, src []byte,
	deleting, found bool,
	slot uint8,
	resident storeio.ResidentPrimaryRoute,
	path *filePrimaryMutationPath,
) (
	nextLeaf storeio.PageRef,
	becameEmpty bool,
	filledEmpty bool,
	err error,
) {
	generation := state.root.Generation + 1
	if generation == 0 {
		return storeio.PageRef{}, false, false,
			storeio.ErrGenerationOrder
	}
	leafImage, leafBytes, prepareErr := c.preparePrimaryLeafMutation(
		path, generation, key, src, deleting, found, slot,
	)
	if prepareErr != nil {
		return storeio.PageRef{}, false, false, prepareErr
	}
	becameEmpty = deleting && path.leaf.Len() == 1
	filledEmpty = !deleting && !found && path.leaf.Len() == 0
	if err := c.refreshReusableFor(
		state,
		c.options.singleDocumentTransactionPages,
		c.options.singleDocumentFreeFoldLimit,
	); err != nil {
		return storeio.PageRef{}, false, false, err
	}
	tx, err := c.beginWriteTransaction(
		c.options.singleDocumentTransactionPages,
		storeio.WriteTransactionOptions{
			StoreID: c.storeID, Generation: generation,
			PageSize:         uint32(c.options.PageSize),
			FileEnd:          state.super.FileEnd,
			NextLogicalID:    state.root.NextLogicalID,
			Reusable:         c.reusable,
			ReuseJournal:     c.reuseJournal,
			ReusableIndex:    &c.freeExtentIndex,
			ReusablePromoter: c.reusableExtentPromoter(),
		},
	)
	if err != nil {
		return storeio.PageRef{}, false, false, err
	}
	abort := true
	retirementReserved := false
	defer func() {
		if abort {
			if retirementReserved {
				_ = c.reclaimer.CancelRetiredGeneration(
					state.root.Generation,
				)
			}
			err = errors.Join(err, tx.Abort())
		}
	}()
	c.retireScratch = c.retireScratch[:0]
	c.retireRefScratch = c.retireRefScratch[:0]

	leafPage, err := tx.Allocate(
		storeio.PagePrimaryLeaf, uint32(leafBytes),
		path.leafRoute.Ref.LogicalID,
	)
	if err != nil {
		return storeio.PageRef{}, false, false, err
	}
	copy(leafPage.Bytes(), leafImage)
	if err := leafPage.Stage(); err != nil {
		return storeio.PageRef{}, false, false, err
	}
	nextLeaf = leafPage.Ref()

	anchorPage, err := tx.Allocate(
		storeio.PagePrimaryAnchor,
		storeio.SegmentedTabletRouterAnchorPageBytes,
		path.anchorRoute.Ref.LogicalID,
	)
	if err != nil {
		return storeio.PageRef{}, false, false, err
	}
	cow, err := path.tablet.RewriteHandle(
		c.primaryRootScratch,
		anchorPage.Bytes(),
		generation, path.leafRoute, nextLeaf,
		path.leafRoute.Zone, anchorPage.Ref(), &path.anchor,
	)
	if err != nil {
		return storeio.PageRef{}, false, false, err
	}
	if err := anchorPage.Stage(); err != nil {
		return storeio.PageRef{}, false, false, err
	}

	tabletPage, err := tx.Allocate(
		storeio.PageTabletRoute,
		storeio.GlobalTabletCatalogTabletBytes,
		path.tabletRoute.Ref.LogicalID,
	)
	if err != nil {
		return storeio.PageRef{}, false, false, err
	}
	locator, ok := path.tablet.LocatorRef()
	if !ok {
		return storeio.PageRef{}, false, false,
			storeio.ErrGlobalTabletCatalogCorrupt
	}
	bounds := c.primaryMutationBounds(tx)
	if _, err := storeio.EncodeGlobalTabletCatalogTabletRoot(
		tabletPage.Bytes(),
		storeio.PageHeader{
			StoreID: c.storeID, Generation: generation,
			LogicalID: tabletPage.Ref().LogicalID,
			PageSize:  storeio.GlobalTabletCatalogTabletBytes,
			PayloadLength: storeio.GlobalTabletCatalogRootHeader +
				storeio.SegmentedTabletRouterRootBytes,
			Kind: storeio.PageTabletRoute,
		},
		bounds, locator, cow.Root,
	); err != nil {
		return storeio.PageRef{}, false, false, err
	}
	if err := tabletPage.Stage(); err != nil {
		return storeio.PageRef{}, false, false, err
	}

	catalogPage, err := tx.Allocate(
		storeio.PagePrimaryCatalog,
		storeio.GlobalTabletCatalogNodeBytes,
		path.catalogLease.Header().LogicalID,
	)
	if err != nil {
		return storeio.PageRef{}, false, false, err
	}
	bounds = c.primaryMutationBounds(tx)
	if _, err := path.catalog.RewriteHandle(
		catalogPage.Bytes(), generation, bounds,
		path.tabletRoute.ID, tabletPage.Ref(),
	); err != nil {
		return storeio.PageRef{}, false, false, err
	}
	if err := catalogPage.Stage(); err != nil {
		return storeio.PageRef{}, false, false, err
	}

	childID := path.catalogRoute.ID
	childRef := catalogPage.Ref()
	if path.hasBranch {
		branchPage, allocateErr := tx.Allocate(
			storeio.PagePrimaryCatalog,
			storeio.GlobalTabletCatalogNodeBytes,
			path.branchLease.Header().LogicalID,
		)
		if allocateErr != nil {
			return storeio.PageRef{}, false, false, allocateErr
		}
		bounds = c.primaryMutationBounds(tx)
		if _, rewriteErr := path.branch.RewriteHandle(
			branchPage.Bytes(), generation, bounds,
			path.catalogRoute.ID, catalogPage.Ref(),
		); rewriteErr != nil {
			return storeio.PageRef{}, false, false, rewriteErr
		}
		if stageErr := branchPage.Stage(); stageErr != nil {
			return storeio.PageRef{}, false, false, stageErr
		}
		childID = path.rootRoute.ID
		childRef = branchPage.Ref()
	}

	rootPage, err := tx.Allocate(
		storeio.PagePrimaryCatalog,
		storeio.GlobalTabletCatalogRootBytes,
		state.root.PrimaryRoot.LogicalID,
	)
	if err != nil {
		return storeio.PageRef{}, false, false, err
	}
	bounds = c.primaryMutationBounds(tx)
	if _, err := path.root.RewriteHandle(
		rootPage.Bytes(), generation, bounds, childID, childRef,
	); err != nil {
		return storeio.PageRef{}, false, false, err
	}
	if err := rootPage.Stage(); err != nil {
		return storeio.PageRef{}, false, false, err
	}
	if !c.primaryRouter.CanUpdateLeaf(
		resident, nextLeaf, generation,
	) {
		return storeio.PageRef{}, false, false,
			storeio.ErrSegmentedTabletRouterCorrupt
	}
	for _, ref := range [...]storeio.PageRef{
		path.leafRoute.Ref,
		path.anchorRoute.Ref,
		path.tabletRoute.Ref,
		path.catalogRef,
	} {
		if err := c.appendPrimaryRetirement(state, ref); err != nil {
			return storeio.PageRef{}, false, false, err
		}
	}
	if path.hasBranch {
		if err := c.appendPrimaryRetirement(
			state, path.branchRef,
		); err != nil {
			return storeio.PageRef{}, false, false, err
		}
	}
	if err := c.appendPrimaryRetirement(
		state, state.root.PrimaryRoot,
	); err != nil {
		return storeio.PageRef{}, false, false, err
	}
	freeLog, err := c.syncFreeLogFor(
		tx, state, c.options.singleDocumentFreeFoldLimit,
	)
	if err != nil {
		return storeio.PageRef{}, false, false,
			fmt.Errorf("vibejson: persist reusable extents: %w", err)
	}
	nextState, nextInline, err := c.stagePrimaryState(
		tx, state, generation, rootPage.Ref(),
		freeLog.head, freeLog.checksum, freeLog.inline,
		func() uint64 {
			if deleting {
				return state.root.DocumentCount - 1
			}
			if !found {
				return state.root.DocumentCount + 1
			}
			return state.root.DocumentCount
		}(),
	)
	if err != nil {
		return storeio.PageRef{}, false, false, err
	}
	if err := c.reserveFileRetirements(); err != nil {
		return storeio.PageRef{}, false, false,
			fmt.Errorf("vibejson: reserve retired extents: %w", err)
	}
	retirementReserved = true
	if err := c.publishStagedPrimaryMutation(
		tx, nextState, nextInline, freeLog,
		resident, nextLeaf,
	); err != nil {
		return storeio.PageRef{}, false, false, err
	}
	abort = false
	return nextLeaf, becameEmpty, filledEmpty, nil
}

func (c *Collection) preparePrimaryLeafMutation(
	path *filePrimaryMutationPath,
	generation uint64,
	key, src []byte,
	deleting, found bool,
	slot uint8,
) ([]byte, int, error) {
	size := len(path.leaf.PersistentBytes())
	dst := c.primaryLeafScratch[:size]
	var (
		page []byte
		err  error
	)
	switch {
	case deleting:
		page, err = path.leaf.DeleteTo(dst, generation, slot, key)
	case found:
		page, err = path.leaf.UpdateTo(
			dst, generation, slot, key,
			storeio.CommonPrimaryLeafValue{Inline: src},
		)
		if errors.Is(err, storeio.ErrCommonPrimaryLeafNeedsWide) {
			page, err = storeio.PromoteCommonPrimaryLeafUpdateTo(
				c.primaryLeafScratch, generation, &path.leaf,
				slot, key,
				storeio.CommonPrimaryLeafValue{Inline: src},
			)
		}
	default:
		page, _, err = path.leaf.InsertTo(
			dst, generation, key,
			storeio.CommonPrimaryLeafValue{Inline: src},
		)
		if errors.Is(err, storeio.ErrCommonPrimaryLeafNeedsWide) {
			page, _, err = storeio.PromoteCommonPrimaryLeafInsertTo(
				c.primaryLeafScratch, generation, &path.leaf,
				key, storeio.CommonPrimaryLeafValue{Inline: src},
			)
		}
	}
	if errors.Is(err, storeio.ErrCommonPrimaryLeafFull) {
		c.primaryLeafSplitRequired.Add(1)
		return nil, 0, errors.Join(
			ErrPrimaryLeafSplitRequired,
			storeio.ErrCommonPrimaryLeafFull,
		)
	}
	if err != nil {
		return nil, 0, err
	}
	return page, len(page), nil
}

func (c *Collection) primaryMutationBounds(
	tx *storeio.WriteTransaction,
) storeio.GlobalTabletCatalogBounds {
	return storeio.GlobalTabletCatalogBounds{
		StoreID: c.storeID, SelectedRootGeneration: tx.Generation(),
		FileEnd: tx.FileEnd(), NextLogicalID: tx.NextLogicalID(),
	}
}

func (c *Collection) appendPrimaryRetirement(
	state *fileStoreState,
	ref storeio.PageRef,
) error {
	if ref == (storeio.PageRef{}) {
		return nil
	}
	if len(c.retireScratch) == cap(c.retireScratch) {
		return storeio.ErrRetiredExtentCapacity
	}
	c.retireScratch = append(c.retireScratch, storeio.FreeExtent{
		Offset: ref.Offset, Length: uint64(ref.Length),
		RetiredGeneration: state.root.Generation,
	})
	c.rememberRetiredRef(ref)
	return nil
}

func (c *Collection) removePrimaryEmptyLeaf() {
	for {
		current := c.primaryEmptyLeaves.Load()
		if current == 0 ||
			c.primaryEmptyLeaves.CompareAndSwap(current, current-1) {
			return
		}
	}
}

func (c *Collection) stagePrimaryState(
	tx *storeio.WriteTransaction,
	old *fileStoreState,
	generation uint64,
	primaryRoot, freeHead storeio.PageRef,
	freeChecksum uint32,
	inlineFree *storeio.InlineFreeDelta,
	documentCount uint64,
) (*fileStoreState, storeio.InlineFreeDelta, error) {
	if inlineFree == nil || inlineFree.ExternalPrev() != freeHead {
		return nil, storeio.InlineFreeDelta{},
			storeio.ErrFreeLogCorrupt
	}
	root := old.root
	root.Generation = generation
	root.DocumentCount = documentCount
	root.NextLogicalID = tx.NextLogicalID()
	root.PrimaryRoot = primaryRoot
	super := old.super
	super.Generation = generation
	super.FileEnd = tx.FileEnd()
	super.FreeOffset = 0
	super.FreeLength = 0
	super.FreeChecksum = 0
	if freeHead != (storeio.PageRef{}) {
		super.FreeOffset = freeHead.Offset
		super.FreeLength = freeHead.Length
		super.FreeChecksum = freeChecksum
	}
	return &fileStoreState{
		root: root, super: super,
		keyRoot: old.keyRoot, chunkRoot: old.chunkRoot,
		indexRoot: old.indexRoot, freeHead: freeHead,
	}, *inlineFree, nil
}

func (c *Collection) publishStagedPrimaryMutation(
	tx *storeio.WriteTransaction,
	nextState *fileStoreState,
	nextInline storeio.InlineFreeDelta,
	freeLog freeLogCommit,
	route storeio.ResidentPrimaryRoute,
	nextLeaf storeio.PageRef,
) error {
	publish := func(retiring bool) error {
		if retiring {
			return tx.PublishInlineRetiring(
				nextState.root, nextInline, c.retireRefScratch,
			)
		}
		return tx.PublishInline(nextState.root, nextInline)
	}
	if !c.buffered() {
		if err := publish(false); err != nil {
			return err
		}
		c.finalizeReusable()
		c.commitFreeLog(freeLog)
		c.inlineFree = nextInline
		c.snapshotGate.Lock()
		c.primaryRouter.UpdateLeaf(
			route, nextLeaf, nextState.root.Generation,
		)
		c.pageValidator.update(nextState)
		c.publishFileState(nextState)
		c.snapshotGate.Unlock()
		return nil
	}

	c.snapshotGate.Lock()
	retiring := !c.leases.AnyActive()
	if err := publish(retiring); err != nil {
		c.snapshotGate.Unlock()
		return err
	}
	c.primaryRouter.UpdateLeaf(
		route, nextLeaf, nextState.root.Generation,
	)
	c.pageValidator.update(nextState)
	c.publishFileState(nextState)
	if retiring {
		c.cache.MarkUnreachable(c.retireRefScratch)
	}
	c.snapshotGate.Unlock()
	c.finalizeReusable()
	c.commitFreeLog(freeLog)
	c.inlineFree = nextInline
	return nil
}
