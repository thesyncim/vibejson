package storeio

import (
	"errors"
	"fmt"
)

const freeTreeMaxLevel = uint8(10)

// ErrFreeTreeDepth reports a free tree beyond the bounded traversal or
// retirement scratch supported by this format generation.
var ErrFreeTreeDepth = errors.New("slopjson: Store free tree depth exhausted")

// FreeTreeBounds come from the selected superblock and state root.
type FreeTreeBounds struct {
	FileEnd       uint64
	NextLogicalID uint64
}

// FreeTreeMutation is one copy-on-write extent insertion, replacement, or
// deletion. Retired pages remain fenced by the old superblock generation.
type FreeTreeMutation struct {
	Root         PageRef
	Retired      [16]PageRef
	RetiredCount uint8
	Found        bool
	Changed      bool
}

func (m *FreeTreeMutation) retire(ref PageRef) error {
	if int(m.RetiredCount) == len(m.Retired) {
		return ErrFreeTreeDepth
	}
	m.Retired[m.RetiredCount] = ref
	m.RetiredCount++
	return nil
}

// AppendFreeTreeExtents walks leaves in physical-offset order. limit is a
// hard caller memory bound; exceeding it returns ErrRetiredExtentCapacity.
func AppendFreeTreeExtents(cache *PageCache, root PageRef, bounds FreeTreeBounds, dst []FreeExtent, limit int) ([]FreeExtent, error) {
	if root == (PageRef{}) {
		return dst, nil
	}
	if cache == nil || limit < len(dst) {
		return dst, fmt.Errorf("%w: free-tree traversal", ErrInvalidWrite)
	}
	return appendFreeTreePage(cache, root, bounds, dst, limit, 0)
}

func appendFreeTreePage(cache *PageCache, ref PageRef, bounds FreeTreeBounds, dst []FreeExtent, limit int, depth uint8) ([]FreeExtent, error) {
	if depth > freeTreeMaxLevel {
		return dst, ErrFreeTreeDepth
	}
	lease, err := cache.Acquire(ref)
	if err != nil {
		return dst, err
	}
	view, err := OpenFreeDirectoryPage(lease.Page(), bounds.FileEnd, bounds.NextLogicalID)
	if err != nil {
		lease.Release()
		return dst, err
	}
	if view.Header().Level == 0 {
		if view.Len() > limit-len(dst) {
			lease.Release()
			return dst, ErrRetiredExtentCapacity
		}
		for i := 0; i < view.Len(); i++ {
			extent, _ := view.ExtentAt(i)
			if len(dst) != 0 {
				previous := dst[len(dst)-1]
				if previous.Offset+previous.Length > extent.Offset {
					lease.Release()
					return dst, ErrFreeDirectoryCorrupt
				}
			}
			dst = append(dst, extent)
		}
		lease.Release()
		return dst, nil
	}
	children := make([]PageRef, view.Len())
	for i := range children {
		child, ok := view.ChildAt(i)
		if !ok {
			lease.Release()
			return dst, ErrFreeDirectoryCorrupt
		}
		children[i] = child.Ref
	}
	lease.Release()
	for _, child := range children {
		dst, err = appendFreeTreePage(cache, child, bounds, dst, limit, depth+1)
		if err != nil {
			return dst, err
		}
	}
	return dst, nil
}

// UpsertFreeTree inserts or replaces the extent at extent.Offset.
func UpsertFreeTree(cache *PageCache, tx *WriteTransaction, root PageRef, extent FreeExtent, bounds FreeTreeBounds) (FreeTreeMutation, error) {
	return mutateFreeTree(cache, tx, root, extent.Offset, extent, false, bounds)
}

// DeleteFreeTree removes the extent whose start equals offset.
func DeleteFreeTree(cache *PageCache, tx *WriteTransaction, root PageRef, offset uint64, bounds FreeTreeBounds) (FreeTreeMutation, error) {
	return mutateFreeTree(cache, tx, root, offset, FreeExtent{}, true, bounds)
}

type freeTreeRewrite struct {
	ref        PageRef
	lower      uint64
	rightRef   PageRef
	rightLower uint64
	found      bool
	changed    bool
	empty      bool
}

func mutateFreeTree(cache *PageCache, tx *WriteTransaction, root PageRef, offset uint64, extent FreeExtent, deleting bool, bounds FreeTreeBounds) (FreeTreeMutation, error) {
	var mutation FreeTreeMutation
	if tx == nil || !tx.active || cache == nil && root != (PageRef{}) || !deleting && extent.Offset != offset {
		return mutation, fmt.Errorf("%w: free-tree mutation", ErrInvalidWrite)
	}
	if root == (PageRef{}) {
		if deleting {
			return mutation, nil
		}
		page, err := encodeFreeTreeLeaf(tx, 0, []FreeExtent{extent}, bounds)
		if err != nil {
			return mutation, err
		}
		mutation.Root, mutation.Changed = page.Ref(), true
		return mutation, nil
	}
	result, err := rewriteFreeTreePage(cache, tx, root, offset, extent, deleting, bounds, true, &mutation)
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
	level, err := freeTreePageLevel(cache, result.ref, tx, bounds)
	if err != nil {
		return mutation, err
	}
	if level == freeTreeMaxLevel {
		return mutation, ErrFreeTreeDepth
	}
	children := []FreeDirectoryChild{{Lower: result.lower, Ref: result.ref}, {Lower: result.rightLower, Ref: result.rightRef}}
	page, err := encodeFreeTreeBranch(tx, 0, level+1, children)
	if err != nil {
		return mutation, err
	}
	mutation.Root = page.Ref()
	return mutation, nil
}

func rewriteFreeTreePage(cache *PageCache, tx *WriteTransaction, ref PageRef, offset uint64, extent FreeExtent, deleting bool, bounds FreeTreeBounds, root bool, mutation *FreeTreeMutation) (freeTreeRewrite, error) {
	lease, err := cache.Acquire(ref)
	if err != nil {
		return freeTreeRewrite{}, err
	}
	defer lease.Release()
	view, err := OpenFreeDirectoryPage(lease.Page(), bounds.FileEnd, bounds.NextLogicalID)
	if err != nil {
		return freeTreeRewrite{}, err
	}
	if view.Header().Level == 0 {
		return rewriteFreeTreeLeaf(tx, ref, view, offset, extent, deleting, bounds, mutation)
	}
	return rewriteFreeTreeBranch(cache, tx, ref, view, offset, extent, deleting, bounds, root, mutation)
}

func rewriteFreeTreeLeaf(tx *WriteTransaction, oldRef PageRef, view FreeDirectoryView, offset uint64, extent FreeExtent, deleting bool, bounds FreeTreeBounds, mutation *FreeTreeMutation) (freeTreeRewrite, error) {
	entries := make([]FreeExtent, view.Len()+1)
	position, found := 0, false
	for i := 0; i < view.Len(); i++ {
		entry, _ := view.ExtentAt(i)
		if entry.Offset < offset {
			entries[position] = entry
			position++
			continue
		}
		if entry.Offset == offset {
			found = true
			if !deleting {
				entries[position] = extent
				position++
			}
			for j := i + 1; j < view.Len(); j++ {
				entries[position], _ = view.ExtentAt(j)
				position++
			}
			break
		}
		if !deleting {
			entries[position] = extent
			position++
		}
		for j := i; j < view.Len(); j++ {
			entries[position], _ = view.ExtentAt(j)
			position++
		}
		break
	}
	if position == view.Len() && !found && !deleting {
		entries[position] = extent
		position++
	}
	if deleting && !found {
		return freeTreeRewrite{ref: oldRef}, nil
	}
	entries = entries[:position]
	if err := mutation.retire(oldRef); err != nil {
		return freeTreeRewrite{}, err
	}
	if len(entries) == 0 {
		return freeTreeRewrite{found: true, changed: true, empty: true}, nil
	}
	if freeTreeLeafFits(tx.options.PageSize, len(entries)) {
		page, err := encodeFreeTreeLeaf(tx, oldRef.LogicalID, entries, bounds)
		if err != nil {
			return freeTreeRewrite{}, err
		}
		return freeTreeRewrite{ref: page.Ref(), lower: entries[0].Offset, found: found, changed: true}, nil
	}
	split := len(entries) / 2
	left, err := encodeFreeTreeLeaf(tx, oldRef.LogicalID, entries[:split], bounds)
	if err != nil {
		return freeTreeRewrite{}, err
	}
	right, err := encodeFreeTreeLeaf(tx, 0, entries[split:], bounds)
	if err != nil {
		return freeTreeRewrite{}, err
	}
	return freeTreeRewrite{ref: left.Ref(), lower: entries[0].Offset, rightRef: right.Ref(), rightLower: entries[split].Offset, found: found, changed: true}, nil
}

func rewriteFreeTreeBranch(cache *PageCache, tx *WriteTransaction, oldRef PageRef, view FreeDirectoryView, offset uint64, extent FreeExtent, deleting bool, bounds FreeTreeBounds, root bool, mutation *FreeTreeMutation) (freeTreeRewrite, error) {
	var children [65]FreeDirectoryChild
	count := view.Len()
	for i := 0; i < count; i++ {
		children[i], _ = view.ChildAt(i)
	}
	rank := freeTreeChildRank(children[:count], offset)
	if rank < 0 {
		if deleting {
			return freeTreeRewrite{ref: oldRef}, nil
		}
		rank = 0
	}
	child, err := rewriteFreeTreePage(cache, tx, children[rank].Ref, offset, extent, deleting, bounds, false, mutation)
	if err != nil {
		return freeTreeRewrite{}, err
	}
	if !child.changed {
		return freeTreeRewrite{ref: oldRef, found: child.found}, nil
	}
	if child.empty {
		copy(children[rank:], children[rank+1:count])
		count--
	} else {
		children[rank] = FreeDirectoryChild{Lower: child.lower, Ref: child.ref}
		if child.rightRef != (PageRef{}) {
			copy(children[rank+2:], children[rank+1:count])
			children[rank+1] = FreeDirectoryChild{Lower: child.rightLower, Ref: child.rightRef}
			count++
		}
	}
	if err := mutation.retire(oldRef); err != nil {
		return freeTreeRewrite{}, err
	}
	if count == 0 {
		return freeTreeRewrite{found: child.found, changed: true, empty: true}, nil
	}
	if root && count == 1 {
		return freeTreeRewrite{ref: children[0].Ref, lower: children[0].Lower, found: child.found, changed: true}, nil
	}
	level := view.Header().Level
	if count <= 64 {
		page, err := encodeFreeTreeBranch(tx, oldRef.LogicalID, level, children[:count])
		if err != nil {
			return freeTreeRewrite{}, err
		}
		return freeTreeRewrite{ref: page.Ref(), lower: children[0].Lower, found: child.found, changed: true}, nil
	}
	split := count / 2
	left, err := encodeFreeTreeBranch(tx, oldRef.LogicalID, level, children[:split])
	if err != nil {
		return freeTreeRewrite{}, err
	}
	right, err := encodeFreeTreeBranch(tx, 0, level, children[split:count])
	if err != nil {
		return freeTreeRewrite{}, err
	}
	return freeTreeRewrite{ref: left.Ref(), lower: children[0].Lower, rightRef: right.Ref(), rightLower: children[split].Lower, found: child.found, changed: true}, nil
}

func encodeFreeTreeLeaf(tx *WriteTransaction, logicalID uint64, extents []FreeExtent, bounds FreeTreeBounds) (TransactionPage, error) {
	page, err := tx.Allocate(PageFreeDirectory, tx.options.PageSize, logicalID)
	if err != nil {
		return TransactionPage{}, err
	}
	header := FreeDirectoryHeader{StoreID: tx.options.StoreID, Generation: tx.options.Generation, LogicalID: page.Ref().LogicalID, PageSize: page.Ref().Length}
	if _, err := EncodeFreeDirectoryLeaf(page.Bytes(), header, extents, tx.FileEnd(), tx.NextLogicalID()); err != nil {
		return TransactionPage{}, err
	}
	if err := page.Stage(); err != nil {
		return TransactionPage{}, err
	}
	return page, nil
}

func encodeFreeTreeBranch(tx *WriteTransaction, logicalID uint64, level uint8, children []FreeDirectoryChild) (TransactionPage, error) {
	page, err := tx.Allocate(PageFreeDirectory, tx.options.PageSize, logicalID)
	if err != nil {
		return TransactionPage{}, err
	}
	header := FreeDirectoryHeader{StoreID: tx.options.StoreID, Generation: tx.options.Generation, LogicalID: page.Ref().LogicalID, PageSize: page.Ref().Length, Level: level}
	if _, err := EncodeFreeDirectoryBranch(page.Bytes(), header, children, tx.FileEnd(), tx.NextLogicalID()); err != nil {
		return TransactionPage{}, err
	}
	if err := page.Stage(); err != nil {
		return TransactionPage{}, err
	}
	return page, nil
}

func freeTreePageLevel(cache *PageCache, ref PageRef, tx *WriteTransaction, bounds FreeTreeBounds) (uint8, error) {
	lease, err := cache.Acquire(ref)
	if err != nil {
		return 0, err
	}
	defer lease.Release()
	view, err := OpenFreeDirectoryPage(lease.Page(), tx.FileEnd(), tx.NextLogicalID())
	if err != nil {
		return 0, err
	}
	return view.Header().Level, nil
}

func freeTreeLeafFits(pageSize uint32, count int) bool {
	return uint64(PageHeaderSize+PageTrailerSize+FreeDirectoryPayloadHeaderSize+count*FreeDirectoryLeafRecordSize) <= uint64(pageSize)
}

func freeTreeChildRank(children []FreeDirectoryChild, offset uint64) int {
	low, high := 0, len(children)
	for low < high {
		middle := int(uint(low+high) >> 1)
		if children[middle].Lower <= offset {
			low = middle + 1
		} else {
			high = middle
		}
	}
	return low - 1
}
