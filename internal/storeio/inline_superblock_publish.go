package storeio

import (
	"fmt"
	"slices"
)

// SetInlineSuperblock encodes a checksummed StateRoot and cumulative free delta
// directly into the alternate fixed-root page. It has the same slot and
// full-page write discipline as SetSuperblock, but requires no separately
// allocated state or routine free-delta page.
func (b *Batch) SetInlineSuperblock(root InlineSuperblock) error {
	if b == nil || b.state.Load() != batchOwned {
		return ErrBatchState
	}
	buffer := b.committer.buffers[b.root.Buffer]
	if uint64(root.PageSize) > uint64(len(buffer)) {
		return ErrInvalidWrite
	}
	page := buffer[:root.PageSize]
	clear(page)
	if _, err := EncodeInlineSuperblock(page, root); err != nil {
		return err
	}
	offset, err := SuperblockOffset(root.Generation, root.PageSize)
	if err != nil {
		return err
	}
	b.root.Offset = offset
	b.root.Length = root.PageSize
	b.rootGeneration = root.Generation
	return nil
}

// PublishInline selects state through an inline alternate superblock. Unlike
// Publish, it accepts the decoded StateRoot and cumulative inline free delta
// and requires that the transaction did not allocate a PageStateRoot extent.
// All ordinary data pages must already be staged.
func (t *WriteTransaction) PublishInline(state StateRoot, free InlineFreeDelta) error {
	if t == nil || !t.active || state.StoreID != t.options.StoreID ||
		state.Generation != t.options.Generation || state.PageSize != t.options.PageSize ||
		state.NextLogicalID != t.nextID {
		return ErrBatchState
	}
	for i := 0; i < t.allocated; i++ {
		write := *t.writeAt(i)
		if write.Length == 0 {
			return fmt.Errorf("%w: unstaged transaction page", ErrInvalidWrite)
		}
		if write.kind == PageStateRoot {
			return fmt.Errorf("%w: inline publication allocated a state page", ErrInvalidWrite)
		}
	}
	if err := t.resizePages(t.allocated); err != nil {
		return err
	}
	if !t.batch.materialized {
		slices.SortFunc(t.fullWrites(), func(a, b Write) int {
			if a.Offset < b.Offset {
				return -1
			}
			if a.Offset > b.Offset {
				return 1
			}
			return 0
		})
	}
	root := InlineSuperblock{
		StoreID: t.options.StoreID, Generation: t.options.Generation,
		FileEnd: t.fileEnd, PageSize: t.options.PageSize, State: state,
		FreeDelta: free,
	}
	if err := t.batch.SetInlineSuperblock(root); err != nil {
		return err
	}
	if err := t.batch.Publish(t.options.Generation); err != nil {
		return err
	}
	t.active = false
	t.batch = nil
	return nil
}
