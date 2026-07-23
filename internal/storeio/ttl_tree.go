package storeio

import (
	"errors"
	"fmt"
)

const ttlTreeMaxLevel = uint8(10)

// ErrTTLTreeDepth reports a TTL tree beyond its fixed traversal bound.
var ErrTTLTreeDepth = errors.New("slopjson: Store TTL tree depth exhausted")

type TTLTreeBounds struct {
	FileEnd        uint64
	NextLogicalID  uint64
	ChunkHighWater uint32
	ChunkDocuments uint8
}

type TTLTreeMutation struct {
	Root         PageRef
	Retired      [16]PageRef
	RetiredCount uint8
	Found        bool
	Changed      bool
}

func (m *TTLTreeMutation) retire(ref PageRef) error {
	if int(m.RetiredCount) == len(m.Retired) {
		return ErrTTLTreeDepth
	}
	m.Retired[m.RetiredCount] = ref
	m.RetiredCount++
	return nil
}

// FirstTTLTree returns the earliest deadline through the leftmost path.
func FirstTTLTree(cache *PageCache, root PageRef, bounds TTLTreeBounds) (TTLKey, bool, error) {
	if root == (PageRef{}) {
		return TTLKey{}, false, nil
	}
	ref := root
	for depth := uint8(0); depth <= ttlTreeMaxLevel; depth++ {
		lease, err := cache.Acquire(ref)
		if err != nil {
			return TTLKey{}, false, err
		}
		view, err := OpenTTLDirectoryPage(lease.Page(), bounds.FileEnd, bounds.NextLogicalID, bounds.ChunkHighWater, bounds.ChunkDocuments)
		if err != nil {
			lease.Release()
			return TTLKey{}, false, err
		}
		if view.Header().Level == 0 {
			entry, ok := view.EntryAt(0)
			lease.Release()
			return entry, ok, nil
		}
		child, ok := view.ChildAt(0)
		lease.Release()
		if !ok {
			return TTLKey{}, false, ErrTTLDirectoryCorrupt
		}
		ref = child.Ref
	}
	return TTLKey{}, false, ErrTTLTreeDepth
}

func UpsertTTLTree(cache *PageCache, tx *WriteTransaction, root PageRef, key TTLKey, bounds TTLTreeBounds) (TTLTreeMutation, error) {
	return mutateTTLTree(cache, tx, root, key, false, bounds)
}

func DeleteTTLTree(cache *PageCache, tx *WriteTransaction, root PageRef, key TTLKey, bounds TTLTreeBounds) (TTLTreeMutation, error) {
	return mutateTTLTree(cache, tx, root, key, true, bounds)
}

type ttlTreeRewrite struct {
	ref        PageRef
	lower      TTLKey
	rightRef   PageRef
	rightLower TTLKey
	found      bool
	changed    bool
	empty      bool
}

func mutateTTLTree(cache *PageCache, tx *WriteTransaction, root PageRef, key TTLKey, deleting bool, bounds TTLTreeBounds) (TTLTreeMutation, error) {
	var mutation TTLTreeMutation
	if tx == nil || !tx.active || cache == nil && root != (PageRef{}) ||
		bounds.ChunkHighWater == 0 || bounds.ChunkDocuments == 0 || bounds.ChunkDocuments > 64 ||
		key.Chunk >= bounds.ChunkHighWater || key.Slot >= bounds.ChunkDocuments {
		return mutation, fmt.Errorf("%w: TTL-tree mutation", ErrInvalidWrite)
	}
	if root == (PageRef{}) {
		if deleting {
			return mutation, nil
		}
		page, err := encodeTTLTreeLeaf(tx, 0, []TTLKey{key}, bounds)
		if err != nil {
			return mutation, err
		}
		mutation.Root, mutation.Changed = page.Ref(), true
		return mutation, nil
	}
	result, err := rewriteTTLTreePage(cache, tx, root, key, deleting, bounds, true, &mutation)
	if err != nil {
		return mutation, err
	}
	mutation.Found, mutation.Changed = result.found, result.changed
	if !result.changed {
		mutation.Root = root
		return mutation, nil
	}
	if result.empty {
		return mutation, nil
	}
	mutation.Root = result.ref
	if result.rightRef == (PageRef{}) {
		return mutation, nil
	}
	level, err := ttlTreePageLevel(cache, result.ref, tx, bounds)
	if err != nil {
		return mutation, err
	}
	if level == ttlTreeMaxLevel {
		return mutation, ErrTTLTreeDepth
	}
	children := []TTLDirectoryChild{{Lower: result.lower, Ref: result.ref}, {Lower: result.rightLower, Ref: result.rightRef}}
	page, err := encodeTTLTreeBranch(tx, 0, level+1, children)
	if err != nil {
		return mutation, err
	}
	mutation.Root = page.Ref()
	return mutation, nil
}

func rewriteTTLTreePage(cache *PageCache, tx *WriteTransaction, ref PageRef, key TTLKey, deleting bool, bounds TTLTreeBounds, root bool, mutation *TTLTreeMutation) (ttlTreeRewrite, error) {
	lease, err := cache.Acquire(ref)
	if err != nil {
		return ttlTreeRewrite{}, err
	}
	defer lease.Release()
	view, err := OpenTTLDirectoryPage(lease.Page(), bounds.FileEnd, bounds.NextLogicalID, bounds.ChunkHighWater, bounds.ChunkDocuments)
	if err != nil {
		return ttlTreeRewrite{}, err
	}
	if view.Header().Level == 0 {
		return rewriteTTLTreeLeaf(tx, ref, view, key, deleting, bounds, mutation)
	}
	return rewriteTTLTreeBranch(cache, tx, ref, view, key, deleting, bounds, root, mutation)
}

func rewriteTTLTreeLeaf(tx *WriteTransaction, oldRef PageRef, view TTLDirectoryView, key TTLKey, deleting bool, bounds TTLTreeBounds, mutation *TTLTreeMutation) (ttlTreeRewrite, error) {
	entries := make([]TTLKey, view.Len()+1)
	position, found := 0, false
	for i := 0; i < view.Len(); i++ {
		entry, _ := view.EntryAt(i)
		comparison := compareTTLKey(entry, key)
		if comparison < 0 {
			entries[position] = entry
			position++
			continue
		}
		if comparison == 0 {
			found = true
			if !deleting {
				entries[position] = key
				position++
			}
			for j := i + 1; j < view.Len(); j++ {
				entries[position], _ = view.EntryAt(j)
				position++
			}
			break
		}
		if !deleting {
			entries[position] = key
			position++
		}
		for j := i; j < view.Len(); j++ {
			entries[position], _ = view.EntryAt(j)
			position++
		}
		break
	}
	if position == view.Len() && !found && !deleting {
		entries[position] = key
		position++
	}
	if deleting && !found {
		return ttlTreeRewrite{ref: oldRef}, nil
	}
	entries = entries[:position]
	if err := mutation.retire(oldRef); err != nil {
		return ttlTreeRewrite{}, err
	}
	if len(entries) == 0 {
		return ttlTreeRewrite{found: true, changed: true, empty: true}, nil
	}
	if ttlTreeLeafFits(tx.options.PageSize, len(entries)) {
		page, err := encodeTTLTreeLeaf(tx, oldRef.LogicalID, entries, bounds)
		if err != nil {
			return ttlTreeRewrite{}, err
		}
		return ttlTreeRewrite{ref: page.Ref(), lower: entries[0], found: found, changed: true}, nil
	}
	split := len(entries) / 2
	left, err := encodeTTLTreeLeaf(tx, oldRef.LogicalID, entries[:split], bounds)
	if err != nil {
		return ttlTreeRewrite{}, err
	}
	right, err := encodeTTLTreeLeaf(tx, 0, entries[split:], bounds)
	if err != nil {
		return ttlTreeRewrite{}, err
	}
	return ttlTreeRewrite{ref: left.Ref(), lower: entries[0], rightRef: right.Ref(), rightLower: entries[split], found: found, changed: true}, nil
}

func rewriteTTLTreeBranch(cache *PageCache, tx *WriteTransaction, oldRef PageRef, view TTLDirectoryView, key TTLKey, deleting bool, bounds TTLTreeBounds, root bool, mutation *TTLTreeMutation) (ttlTreeRewrite, error) {
	var children [65]TTLDirectoryChild
	count := view.Len()
	for i := 0; i < count; i++ {
		children[i], _ = view.ChildAt(i)
	}
	rank := ttlTreeChildRank(children[:count], key)
	if rank < 0 {
		if deleting {
			return ttlTreeRewrite{ref: oldRef}, nil
		}
		rank = 0
	}
	child, err := rewriteTTLTreePage(cache, tx, children[rank].Ref, key, deleting, bounds, false, mutation)
	if err != nil {
		return ttlTreeRewrite{}, err
	}
	if !child.changed {
		return ttlTreeRewrite{ref: oldRef, found: child.found}, nil
	}
	if child.empty {
		copy(children[rank:], children[rank+1:count])
		count--
	} else {
		children[rank] = TTLDirectoryChild{Lower: child.lower, Ref: child.ref}
		if child.rightRef != (PageRef{}) {
			copy(children[rank+2:], children[rank+1:count])
			children[rank+1] = TTLDirectoryChild{Lower: child.rightLower, Ref: child.rightRef}
			count++
		}
	}
	if err := mutation.retire(oldRef); err != nil {
		return ttlTreeRewrite{}, err
	}
	if count == 0 {
		return ttlTreeRewrite{found: child.found, changed: true, empty: true}, nil
	}
	if root && count == 1 {
		return ttlTreeRewrite{ref: children[0].Ref, lower: children[0].Lower, found: child.found, changed: true}, nil
	}
	level := view.Header().Level
	if count <= 64 {
		page, err := encodeTTLTreeBranch(tx, oldRef.LogicalID, level, children[:count])
		if err != nil {
			return ttlTreeRewrite{}, err
		}
		return ttlTreeRewrite{ref: page.Ref(), lower: children[0].Lower, found: child.found, changed: true}, nil
	}
	split := count / 2
	left, err := encodeTTLTreeBranch(tx, oldRef.LogicalID, level, children[:split])
	if err != nil {
		return ttlTreeRewrite{}, err
	}
	right, err := encodeTTLTreeBranch(tx, 0, level, children[split:count])
	if err != nil {
		return ttlTreeRewrite{}, err
	}
	return ttlTreeRewrite{ref: left.Ref(), lower: children[0].Lower, rightRef: right.Ref(), rightLower: children[split].Lower, found: child.found, changed: true}, nil
}

func encodeTTLTreeLeaf(tx *WriteTransaction, logicalID uint64, entries []TTLKey, bounds TTLTreeBounds) (TransactionPage, error) {
	page, err := tx.Allocate(PageTTLDirectory, tx.options.PageSize, logicalID)
	if err != nil {
		return TransactionPage{}, err
	}
	header := TTLDirectoryHeader{StoreID: tx.options.StoreID, Generation: tx.options.Generation, LogicalID: page.Ref().LogicalID, PageSize: page.Ref().Length}
	if _, err := EncodeTTLDirectoryLeaf(page.Bytes(), header, entries, tx.NextLogicalID(), bounds.ChunkHighWater, bounds.ChunkDocuments); err != nil {
		return TransactionPage{}, err
	}
	if err := page.Stage(); err != nil {
		return TransactionPage{}, err
	}
	return page, nil
}

func encodeTTLTreeBranch(tx *WriteTransaction, logicalID uint64, level uint8, children []TTLDirectoryChild) (TransactionPage, error) {
	page, err := tx.Allocate(PageTTLDirectory, tx.options.PageSize, logicalID)
	if err != nil {
		return TransactionPage{}, err
	}
	header := TTLDirectoryHeader{StoreID: tx.options.StoreID, Generation: tx.options.Generation, LogicalID: page.Ref().LogicalID, PageSize: page.Ref().Length, Level: level}
	if _, err := EncodeTTLDirectoryBranch(page.Bytes(), header, children, tx.FileEnd(), tx.NextLogicalID()); err != nil {
		return TransactionPage{}, err
	}
	if err := page.Stage(); err != nil {
		return TransactionPage{}, err
	}
	return page, nil
}

func ttlTreePageLevel(cache *PageCache, ref PageRef, tx *WriteTransaction, bounds TTLTreeBounds) (uint8, error) {
	lease, err := cache.Acquire(ref)
	if err != nil {
		return 0, err
	}
	defer lease.Release()
	view, err := OpenTTLDirectoryPage(lease.Page(), tx.FileEnd(), tx.NextLogicalID(), bounds.ChunkHighWater, bounds.ChunkDocuments)
	if err != nil {
		return 0, err
	}
	return view.Header().Level, nil
}

func ttlTreeLeafFits(pageSize uint32, count int) bool {
	return uint64(PageHeaderSize+PageTrailerSize+TTLDirectoryPayloadHeaderSize+count*TTLDirectoryLeafRecordSize) <= uint64(pageSize)
}

func ttlTreeChildRank(children []TTLDirectoryChild, key TTLKey) int {
	low, high := 0, len(children)
	for low < high {
		middle := int(uint(low+high) >> 1)
		if compareTTLKey(children[middle].Lower, key) <= 0 {
			low = middle + 1
		} else {
			high = middle
		}
	}
	return low - 1
}
