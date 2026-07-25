package storeio

import "fmt"

// TTLTreeEdit is one deadline-row mutation inside a sorted batch.
type TTLTreeEdit struct {
	Key    TTLKey
	Delete bool
}

// TTLTreeBatchMutation is the result of one batched TTL-directory descent.
type TTLTreeBatchMutation struct {
	Root    PageRef
	Retired []PageRef
	Changed bool
}

// MutateTTLTreeBatch applies edits, sorted strictly ascending by TTL key, in
// one copy-on-write descent that rewrites every visited page exactly once.
//
// The TTL directory is only reached by mutations that carry a deadline, so this
// exists for the page budget rather than for throughput: a batched publication
// must reserve for the worst case it can encounter, and one sequential removal
// per deleted document made that reservation — twenty-two pages per document —
// several times larger than everything else the batch could write combined.
func MutateTTLTreeBatch(
	cache *PageCache, tx *WriteTransaction, root PageRef,
	edits []TTLTreeEdit, bounds TTLTreeBounds, retired []PageRef,
) (TTLTreeBatchMutation, error) {
	mutation := TTLTreeBatchMutation{Root: root, Retired: retired}
	if tx == nil || !tx.active || (cache == nil && root != (PageRef{})) ||
		bounds.ChunkHighWater == 0 || bounds.ChunkDocuments == 0 || bounds.ChunkDocuments > 64 {
		return mutation, fmt.Errorf("%w: TTL-tree batch bounds", ErrInvalidWrite)
	}
	for i, edit := range edits {
		if edit.Key.Chunk >= bounds.ChunkHighWater || edit.Key.Slot >= bounds.ChunkDocuments {
			return mutation, fmt.Errorf("%w: TTL-tree batch key", ErrInvalidWrite)
		}
		if i != 0 && compareTTLKey(edits[i-1].Key, edit.Key) >= 0 {
			return mutation, fmt.Errorf("%w: unsorted TTL-tree batch", ErrInvalidWrite)
		}
	}
	if len(edits) == 0 {
		return mutation, nil
	}

	batch := ttlTreeBatch{cache: cache, tx: tx, bounds: bounds, retired: retired}
	if root == (PageRef{}) {
		batch.entries = batch.entries[:0]
		for _, edit := range edits {
			if edit.Delete {
				continue
			}
			batch.entries = append(batch.entries, edit.Key)
		}
		if len(batch.entries) == 0 {
			return mutation, nil
		}
		children, err := batch.encodeLeaves(nil, 0, batch.entries)
		if err != nil {
			return mutation, err
		}
		return batch.finish(children)
	}

	children, err := batch.rewritePage(nil, root, edits, 0)
	if err != nil {
		mutation.Retired = batch.retired
		return mutation, err
	}
	return batch.finish(children)
}

type ttlTreeBatch struct {
	cache    *PageCache
	tx       *WriteTransaction
	bounds   TTLTreeBounds
	retired  []PageRef
	entries  []TTLKey
	assembly [ttlTreeMaxLevel + 2][]TTLDirectoryChild
}

func (b *ttlTreeBatch) finish(children []TTLDirectoryChild) (TTLTreeBatchMutation, error) {
	mutation := TTLTreeBatchMutation{Retired: b.retired, Changed: true}
	level := uint8(0)
	if len(children) > 1 {
		var err error
		level, err = ttlTreePageLevel(b.cache, children[0].Ref, b.tx, b.bounds)
		if err != nil {
			return mutation, err
		}
	}
	for len(children) > 1 {
		if level >= ttlTreeMaxLevel {
			return mutation, ErrTTLTreeDepth
		}
		level++
		next, err := b.encodeBranches(nil, level, 0, children)
		if err != nil {
			return mutation, err
		}
		children = next
	}
	if len(children) == 1 {
		mutation.Root = children[0].Ref
	}
	return mutation, nil
}

func (b *ttlTreeBatch) rewritePage(
	dst []TTLDirectoryChild, ref PageRef, edits []TTLTreeEdit, depth int,
) ([]TTLDirectoryChild, error) {
	if depth > int(ttlTreeMaxLevel) {
		return nil, ErrTTLTreeDepth
	}
	lease, err := b.cache.Acquire(ref)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	view, err := OpenTTLDirectoryPage(
		lease.Page(), b.bounds.FileEnd, b.bounds.NextLogicalID,
		b.bounds.ChunkHighWater, b.bounds.ChunkDocuments,
	)
	if err != nil {
		return nil, err
	}
	if view.Header().Level == 0 {
		return b.rewriteLeaf(dst, ref, view, edits)
	}
	return b.rewriteBranch(dst, ref, view, edits, depth)
}

func (b *ttlTreeBatch) rewriteLeaf(
	dst []TTLDirectoryChild, ref PageRef, view TTLDirectoryView, edits []TTLTreeEdit,
) ([]TTLDirectoryChild, error) {
	entries := b.entries[:0]
	at := 0
	for i := 0; i < view.Len(); i++ {
		entry, _ := view.EntryAt(i)
		for at < len(edits) && compareTTLKey(edits[at].Key, entry) < 0 {
			if !edits[at].Delete {
				entries = append(entries, edits[at].Key)
			}
			at++
		}
		if at < len(edits) && compareTTLKey(edits[at].Key, entry) == 0 {
			if !edits[at].Delete {
				entries = append(entries, edits[at].Key)
			}
			at++
			continue
		}
		entries = append(entries, entry)
	}
	for ; at < len(edits); at++ {
		if edits[at].Delete {
			continue
		}
		entries = append(entries, edits[at].Key)
	}
	b.entries = entries
	b.retired = append(b.retired, ref)
	if len(entries) == 0 {
		return dst, nil
	}
	return b.encodeLeaves(dst, ref.LogicalID, entries)
}

func (b *ttlTreeBatch) rewriteBranch(
	dst []TTLDirectoryChild, ref PageRef, view TTLDirectoryView, edits []TTLTreeEdit, depth int,
) ([]TTLDirectoryChild, error) {
	count := view.Len()
	if count == 0 {
		return nil, ErrTTLDirectoryCorrupt
	}
	assembled := b.assembly[depth][:0]
	at := 0
	for rank := 0; rank < count; rank++ {
		child, _ := view.ChildAt(rank)
		end := len(edits)
		if rank+1 < count {
			next, _ := view.ChildAt(rank + 1)
			end = at
			for end < len(edits) && compareTTLKey(edits[end].Key, next.Lower) < 0 {
				end++
			}
		}
		if end == at {
			assembled = append(assembled, child)
			continue
		}
		var err error
		assembled, err = b.rewritePage(assembled, child.Ref, edits[at:end], depth+1)
		if err != nil {
			return nil, err
		}
		at = end
	}
	b.assembly[depth] = assembled
	b.retired = append(b.retired, ref)
	if len(assembled) == 0 {
		return dst, nil
	}
	return b.encodeBranches(dst, view.Header().Level, ref.LogicalID, assembled)
}

func (b *ttlTreeBatch) encodeLeaves(
	dst []TTLDirectoryChild, logicalID uint64, entries []TTLKey,
) ([]TTLDirectoryChild, error) {
	limit := ttlTreeLeafLimit(b.tx.options.PageSize)
	if limit == 0 {
		return nil, fmt.Errorf("%w: TTL entry does not fit a directory page", ErrInvalidWrite)
	}
	for len(entries) != 0 {
		take := treeBatchShare(len(entries), limit)
		page, err := encodeTTLTreeLeaf(b.tx, logicalID, entries[:take], b.bounds)
		if err != nil {
			return nil, err
		}
		dst = append(dst, TTLDirectoryChild{Lower: entries[0], Ref: page.Ref()})
		entries = entries[take:]
		logicalID = 0
	}
	return dst, nil
}

func (b *ttlTreeBatch) encodeBranches(
	dst []TTLDirectoryChild, level uint8, logicalID uint64, children []TTLDirectoryChild,
) ([]TTLDirectoryChild, error) {
	limit := ttlTreeBranchLimit(b.tx.options.PageSize)
	if limit == 0 {
		return nil, fmt.Errorf("%w: TTL branch does not fit a directory page", ErrInvalidWrite)
	}
	for len(children) != 0 {
		take := treeBatchShare(len(children), limit)
		page, err := encodeTTLTreeBranch(b.tx, logicalID, level, children[:take])
		if err != nil {
			return nil, err
		}
		dst = append(dst, TTLDirectoryChild{Lower: children[0].Lower, Ref: page.Ref()})
		children = children[take:]
		logicalID = 0
	}
	return dst, nil
}

func ttlTreeLeafLimit(pageSize uint32) int {
	room := int(pageSize) - PageHeaderSize - PageTrailerSize - TTLDirectoryPayloadHeaderSize
	if room < 0 {
		return 0
	}
	return room / TTLDirectoryLeafRecordSize
}

func ttlTreeBranchLimit(pageSize uint32) int {
	room := int(pageSize) - PageHeaderSize - PageTrailerSize - TTLDirectoryPayloadHeaderSize
	if room < 0 {
		return 0
	}
	return min(64, room/TTLDirectoryBranchRecordSize)
}

// TTLTreeBatchRemovalPages bounds the pages one MutateTTLTreeBatch descent can
// publish when every edit is a removal. Removals never split, so unlike
// KeyTreeBatchPages the bound is one page per visited node rather than two: a
// caller reserving for a batch of deletes would otherwise reserve twice the
// pages the descent can possibly write, and on the default geometry the TTL
// term alone was half the whole batch reservation.
func TTLTreeBatchRemovalPages(edits int) int {
	if edits <= 0 {
		return 0
	}
	return edits + 2*int(ttlTreeMaxLevel) + 2
}
