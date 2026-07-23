package storeio

import (
	"errors"
	"fmt"
)

const indexTreeMaxLevel = uint8(10)

var ErrIndexTreeDepth = errors.New("simdjson: Store index tree depth exhausted")

type IndexTreeBounds struct {
	FileEnd        uint64
	NextLogicalID  uint64
	IndexHighWater uint32
}

type IndexTreeMutation struct {
	Root         PageRef
	Retired      [16]PageRef
	RetiredCount uint8
	Found        bool
	Changed      bool
}

func (m *IndexTreeMutation) retire(ref PageRef) error {
	if int(m.RetiredCount) == len(m.Retired) {
		return ErrIndexTreeDepth
	}
	m.Retired[m.RetiredCount] = ref
	m.RetiredCount++
	return nil
}

func LookupIndexTree(cache *PageCache, root PageRef, key IndexDirectoryKey, bounds IndexTreeBounds) (IndexPostingRef, bool, error) {
	if root == (PageRef{}) {
		return IndexPostingRef{}, false, nil
	}
	ref := root
	for depth := uint8(0); depth <= indexTreeMaxLevel; depth++ {
		lease, err := cache.Acquire(ref)
		if err != nil {
			return IndexPostingRef{}, false, err
		}
		view, err := OpenIndexDirectoryPage(lease.Page(), bounds.FileEnd, bounds.NextLogicalID, bounds.IndexHighWater)
		if err != nil {
			lease.Release()
			return IndexPostingRef{}, false, err
		}
		if view.Header().Level == 0 {
			posting, ok := view.Lookup(key)
			lease.Release()
			return posting, ok, nil
		}
		next, ok := view.Child(key)
		lease.Release()
		if !ok {
			return IndexPostingRef{}, false, nil
		}
		ref = next
	}
	return IndexPostingRef{}, false, ErrIndexTreeDepth
}

// AppendIndexTreeHash appends every chunk entry for one (index, hash) prefix
// without visiting unrelated leaf subtrees. limit is a hard memory bound.
func AppendIndexTreeHash(cache *PageCache, root PageRef, indexID uint32, tupleHash uint64, bounds IndexTreeBounds, dst []IndexDirectoryEntry, limit int) ([]IndexDirectoryEntry, error) {
	if root == (PageRef{}) {
		return dst, nil
	}
	low := IndexDirectoryKey{IndexID: indexID, TupleHash: tupleHash}
	high := IndexDirectoryKey{IndexID: indexID, TupleHash: tupleHash, Chunk: ^uint32(0)}
	return appendIndexTreeRange(cache, root, low, high, bounds, dst, limit, 0)
}

// WalkIndexTreeIndex visits every leaf intersecting one index id in routing
// order. The borrowed view is valid only during fn. A boundary leaf may also
// contain adjacent index ids; callers must select entries by IndexID.
//
// Keeping the directory page leased across its complete callback lets
// analytical consumers stream a large index without retaining one Go object
// per posting. The traversal stack is bounded by indexTreeMaxLevel.
func WalkIndexTreeIndex(
	cache *PageCache,
	root PageRef,
	indexID uint32,
	bounds IndexTreeBounds,
	fn func(IndexDirectoryView) error,
) error {
	if root == (PageRef{}) || fn == nil {
		return nil
	}
	if indexID >= bounds.IndexHighWater {
		return fmt.Errorf("%w: index-tree walk id", ErrInvalidWrite)
	}
	low := IndexDirectoryKey{IndexID: indexID}
	high := IndexDirectoryKey{
		IndexID: indexID, TupleHash: ^uint64(0), Chunk: ^uint32(0),
	}
	return walkIndexTreeRange(cache, root, low, high, bounds, fn, 0)
}

func walkIndexTreeRange(
	cache *PageCache,
	ref PageRef,
	low, high IndexDirectoryKey,
	bounds IndexTreeBounds,
	fn func(IndexDirectoryView) error,
	depth uint8,
) error {
	if depth > indexTreeMaxLevel {
		return ErrIndexTreeDepth
	}
	lease, err := cache.Acquire(ref)
	if err != nil {
		return err
	}
	view, err := OpenIndexDirectoryPage(
		lease.Page(), bounds.FileEnd, bounds.NextLogicalID, bounds.IndexHighWater,
	)
	if err != nil {
		lease.Release()
		return err
	}
	if view.Header().Level == 0 {
		err = fn(view)
		lease.Release()
		return err
	}
	var childStorage [64]IndexDirectoryChild
	children := childStorage[:view.Len()]
	for i := range children {
		children[i], _ = view.ChildAt(i)
	}
	lease.Release()
	for i, child := range children {
		if i+1 < len(children) &&
			compareIndexDirectoryKey(children[i+1].Lower, low) <= 0 {
			continue
		}
		if compareIndexDirectoryKey(child.Lower, high) > 0 {
			break
		}
		if err := walkIndexTreeRange(
			cache, child.Ref, low, high, bounds, fn, depth+1,
		); err != nil {
			return err
		}
	}
	return nil
}

func appendIndexTreeRange(cache *PageCache, ref PageRef, low, high IndexDirectoryKey, bounds IndexTreeBounds, dst []IndexDirectoryEntry, limit int, depth uint8) ([]IndexDirectoryEntry, error) {
	if depth > indexTreeMaxLevel {
		return dst, ErrIndexTreeDepth
	}
	lease, err := cache.Acquire(ref)
	if err != nil {
		return dst, err
	}
	view, err := OpenIndexDirectoryPage(lease.Page(), bounds.FileEnd, bounds.NextLogicalID, bounds.IndexHighWater)
	if err != nil {
		lease.Release()
		return dst, err
	}
	if view.Header().Level == 0 {
		for i := 0; i < view.Len(); i++ {
			entry, _ := view.EntryAt(i)
			if compareIndexDirectoryKey(entry.Key, low) < 0 {
				continue
			}
			if compareIndexDirectoryKey(entry.Key, high) > 0 {
				break
			}
			if len(dst) == limit {
				lease.Release()
				return dst, ErrRetiredExtentCapacity
			}
			dst = append(dst, entry)
		}
		lease.Release()
		return dst, nil
	}
	var childStorage [64]IndexDirectoryChild
	children := childStorage[:view.Len()]
	for i := range children {
		children[i], _ = view.ChildAt(i)
	}
	lease.Release()
	for i, child := range children {
		nextLowerBeyond := i+1 < len(children) && compareIndexDirectoryKey(children[i+1].Lower, low) <= 0
		if nextLowerBeyond {
			continue
		}
		if compareIndexDirectoryKey(child.Lower, high) > 0 {
			break
		}
		dst, err = appendIndexTreeRange(cache, child.Ref, low, high, bounds, dst, limit, depth+1)
		if err != nil {
			return dst, err
		}
	}
	return dst, nil
}

func UpsertIndexTree(cache *PageCache, tx *WriteTransaction, root PageRef, entry IndexDirectoryEntry, bounds IndexTreeBounds) (IndexTreeMutation, error) {
	return mutateIndexTree(cache, tx, root, entry.Key, entry, false, bounds)
}

func DeleteIndexTree(cache *PageCache, tx *WriteTransaction, root PageRef, key IndexDirectoryKey, bounds IndexTreeBounds) (IndexTreeMutation, error) {
	return mutateIndexTree(cache, tx, root, key, IndexDirectoryEntry{}, true, bounds)
}

type indexTreeRewrite struct {
	ref                   PageRef
	lower                 IndexDirectoryKey
	rightRef              PageRef
	rightLower            IndexDirectoryKey
	found, changed, empty bool
}

func mutateIndexTree(cache *PageCache, tx *WriteTransaction, root PageRef, key IndexDirectoryKey, entry IndexDirectoryEntry, deleting bool, bounds IndexTreeBounds) (IndexTreeMutation, error) {
	var mutation IndexTreeMutation
	if tx == nil || !tx.active || bounds.IndexHighWater == 0 || key.IndexID >= bounds.IndexHighWater || cache == nil && root != (PageRef{}) {
		return mutation, fmt.Errorf("%w: index-tree mutation", ErrInvalidWrite)
	}
	if root == (PageRef{}) {
		if deleting {
			return mutation, nil
		}
		page, err := encodeIndexTreeLeaf(tx, 0, []IndexDirectoryEntry{entry}, bounds)
		if err != nil {
			return mutation, err
		}
		mutation.Root, mutation.Changed = page.Ref(), true
		return mutation, nil
	}
	result, err := rewriteIndexTreePage(cache, tx, root, key, entry, deleting, bounds, true, &mutation)
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
	level, err := indexTreePageLevel(cache, result.ref, tx, bounds)
	if err != nil {
		return mutation, err
	}
	if level == indexTreeMaxLevel {
		return mutation, ErrIndexTreeDepth
	}
	page, err := encodeIndexTreeBranch(tx, 0, level+1, []IndexDirectoryChild{{Lower: result.lower, Ref: result.ref}, {Lower: result.rightLower, Ref: result.rightRef}})
	if err != nil {
		return mutation, err
	}
	mutation.Root = page.Ref()
	return mutation, nil
}

func rewriteIndexTreePage(cache *PageCache, tx *WriteTransaction, ref PageRef, key IndexDirectoryKey, entry IndexDirectoryEntry, deleting bool, bounds IndexTreeBounds, root bool, mutation *IndexTreeMutation) (indexTreeRewrite, error) {
	lease, err := cache.Acquire(ref)
	if err != nil {
		return indexTreeRewrite{}, err
	}
	defer lease.Release()
	view, err := OpenIndexDirectoryPage(lease.Page(), bounds.FileEnd, bounds.NextLogicalID, bounds.IndexHighWater)
	if err != nil {
		return indexTreeRewrite{}, err
	}
	if view.Header().Level == 0 {
		return rewriteIndexTreeLeaf(tx, ref, view, key, entry, deleting, bounds, mutation)
	}
	return rewriteIndexTreeBranch(cache, tx, ref, view, key, entry, deleting, bounds, root, mutation)
}

func rewriteIndexTreeLeaf(tx *WriteTransaction, oldRef PageRef, view IndexDirectoryView, key IndexDirectoryKey, entry IndexDirectoryEntry, deleting bool, bounds IndexTreeBounds, mutation *IndexTreeMutation) (indexTreeRewrite, error) {
	entries := make([]IndexDirectoryEntry, view.Len()+1)
	position, found := 0, false
	for i := 0; i < view.Len(); i++ {
		current, _ := view.EntryAt(i)
		comparison := compareIndexDirectoryKey(current.Key, key)
		if comparison < 0 {
			entries[position] = current
			position++
			continue
		}
		if comparison == 0 {
			found = true
			if !deleting {
				entries[position] = entry
				position++
			}
			for j := i + 1; j < view.Len(); j++ {
				entries[position], _ = view.EntryAt(j)
				position++
			}
			break
		}
		if !deleting {
			entries[position] = entry
			position++
		}
		for j := i; j < view.Len(); j++ {
			entries[position], _ = view.EntryAt(j)
			position++
		}
		break
	}
	if position == view.Len() && !found && !deleting {
		entries[position], position = entry, position+1
	}
	if deleting && !found {
		return indexTreeRewrite{ref: oldRef}, nil
	}
	entries = entries[:position]
	if err := mutation.retire(oldRef); err != nil {
		return indexTreeRewrite{}, err
	}
	if len(entries) == 0 {
		return indexTreeRewrite{found: true, changed: true, empty: true}, nil
	}
	if indexTreeLeafFits(tx.options.PageSize, len(entries)) {
		page, err := encodeIndexTreeLeaf(tx, oldRef.LogicalID, entries, bounds)
		if err != nil {
			return indexTreeRewrite{}, err
		}
		return indexTreeRewrite{ref: page.Ref(), lower: entries[0].Key, found: found, changed: true}, nil
	}
	split := len(entries) / 2
	left, err := encodeIndexTreeLeaf(tx, oldRef.LogicalID, entries[:split], bounds)
	if err != nil {
		return indexTreeRewrite{}, err
	}
	right, err := encodeIndexTreeLeaf(tx, 0, entries[split:], bounds)
	if err != nil {
		return indexTreeRewrite{}, err
	}
	return indexTreeRewrite{ref: left.Ref(), lower: entries[0].Key, rightRef: right.Ref(), rightLower: entries[split].Key, found: found, changed: true}, nil
}

func rewriteIndexTreeBranch(cache *PageCache, tx *WriteTransaction, oldRef PageRef, view IndexDirectoryView, key IndexDirectoryKey, entry IndexDirectoryEntry, deleting bool, bounds IndexTreeBounds, root bool, mutation *IndexTreeMutation) (indexTreeRewrite, error) {
	var children [65]IndexDirectoryChild
	count := view.Len()
	for i := 0; i < count; i++ {
		children[i], _ = view.ChildAt(i)
	}
	rank := indexTreeChildRank(children[:count], key)
	if rank < 0 {
		if deleting {
			return indexTreeRewrite{ref: oldRef}, nil
		}
		rank = 0
	}
	child, err := rewriteIndexTreePage(cache, tx, children[rank].Ref, key, entry, deleting, bounds, false, mutation)
	if err != nil {
		return indexTreeRewrite{}, err
	}
	if !child.changed {
		return indexTreeRewrite{ref: oldRef, found: child.found}, nil
	}
	if child.empty {
		copy(children[rank:], children[rank+1:count])
		count--
	} else {
		children[rank] = IndexDirectoryChild{Lower: child.lower, Ref: child.ref}
		if child.rightRef != (PageRef{}) {
			copy(children[rank+2:], children[rank+1:count])
			children[rank+1] = IndexDirectoryChild{Lower: child.rightLower, Ref: child.rightRef}
			count++
		}
	}
	if err := mutation.retire(oldRef); err != nil {
		return indexTreeRewrite{}, err
	}
	if count == 0 {
		return indexTreeRewrite{found: child.found, changed: true, empty: true}, nil
	}
	if root && count == 1 {
		return indexTreeRewrite{ref: children[0].Ref, lower: children[0].Lower, found: child.found, changed: true}, nil
	}
	level := view.Header().Level
	if count <= 64 {
		page, err := encodeIndexTreeBranch(tx, oldRef.LogicalID, level, children[:count])
		if err != nil {
			return indexTreeRewrite{}, err
		}
		return indexTreeRewrite{ref: page.Ref(), lower: children[0].Lower, found: child.found, changed: true}, nil
	}
	split := count / 2
	left, err := encodeIndexTreeBranch(tx, oldRef.LogicalID, level, children[:split])
	if err != nil {
		return indexTreeRewrite{}, err
	}
	right, err := encodeIndexTreeBranch(tx, 0, level, children[split:count])
	if err != nil {
		return indexTreeRewrite{}, err
	}
	return indexTreeRewrite{ref: left.Ref(), lower: children[0].Lower, rightRef: right.Ref(), rightLower: children[split].Lower, found: child.found, changed: true}, nil
}

func encodeIndexTreeLeaf(tx *WriteTransaction, logicalID uint64, entries []IndexDirectoryEntry, bounds IndexTreeBounds) (TransactionPage, error) {
	page, err := tx.Allocate(PageIndexDirectory, tx.options.PageSize, logicalID)
	if err != nil {
		return TransactionPage{}, err
	}
	header := IndexDirectoryHeader{StoreID: tx.options.StoreID, Generation: tx.options.Generation, LogicalID: page.Ref().LogicalID, PageSize: page.Ref().Length}
	if _, err := EncodeIndexDirectoryLeaf(page.Bytes(), header, entries, tx.FileEnd(), tx.NextLogicalID(), bounds.IndexHighWater); err != nil {
		return TransactionPage{}, err
	}
	if err := page.Stage(); err != nil {
		return TransactionPage{}, err
	}
	return page, nil
}

func encodeIndexTreeBranch(tx *WriteTransaction, logicalID uint64, level uint8, children []IndexDirectoryChild) (TransactionPage, error) {
	page, err := tx.Allocate(PageIndexDirectory, tx.options.PageSize, logicalID)
	if err != nil {
		return TransactionPage{}, err
	}
	header := IndexDirectoryHeader{StoreID: tx.options.StoreID, Generation: tx.options.Generation, LogicalID: page.Ref().LogicalID, PageSize: page.Ref().Length, Level: level}
	if _, err := EncodeIndexDirectoryBranch(page.Bytes(), header, children, tx.FileEnd(), tx.NextLogicalID()); err != nil {
		return TransactionPage{}, err
	}
	if err := page.Stage(); err != nil {
		return TransactionPage{}, err
	}
	return page, nil
}

func indexTreePageLevel(cache *PageCache, ref PageRef, tx *WriteTransaction, bounds IndexTreeBounds) (uint8, error) {
	lease, err := cache.Acquire(ref)
	if err != nil {
		return 0, err
	}
	defer lease.Release()
	view, err := OpenIndexDirectoryPage(lease.Page(), tx.FileEnd(), tx.NextLogicalID(), bounds.IndexHighWater)
	if err != nil {
		return 0, err
	}
	return view.Header().Level, nil
}
func indexTreeLeafFits(pageSize uint32, count int) bool {
	return uint64(PageHeaderSize+PageTrailerSize+IndexDirectoryPayloadHeaderSize+count*IndexDirectoryLeafRecordSize) <= uint64(pageSize)
}
func indexTreeChildRank(children []IndexDirectoryChild, key IndexDirectoryKey) int {
	low, high := 0, len(children)
	for low < high {
		middle := int(uint(low+high) >> 1)
		if compareIndexDirectoryKey(children[middle].Lower, key) <= 0 {
			low = middle + 1
		} else {
			high = middle
		}
	}
	return low - 1
}
