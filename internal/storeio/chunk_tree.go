package storeio

import (
	"fmt"
	"math/bits"
)

// ChunkTreeBounds are copied from the selected durable roots when existing
// radix pages are admitted.
type ChunkTreeBounds struct {
	FileEnd       uint64
	NextLogicalID uint64
}

// ChunkTreeMutation reports one fixed-depth radix path replacement.
type ChunkTreeMutation struct {
	Root         PageRef
	Retired      [8]PageRef
	RetiredCount uint8
	Found        bool
	Changed      bool
}

// WalkChunkTree visits live chunk mappings in ascending chunk ID without
// scanning holes in ChunkHighWater. The callback receives value-only refs;
// no directory lease remains pinned during the call.
func WalkChunkTree(cache *PageCache, root PageRef, bounds ChunkTreeBounds, fn func(uint32, PageRef) error) error {
	if root == (PageRef{}) {
		return nil
	}
	if cache == nil || fn == nil {
		return fmt.Errorf("%w: chunk-tree walk", ErrInvalidWrite)
	}
	return walkChunkTreePage(cache, root, bounds, 30, fn)
}

// WalkChunkTreeRuns coalesces consecutive chunk ids that name the same
// physical extent. Ordinary mutable document pages produce one-chunk runs;
// compact-generation groups produce bounded multi-chunk runs even when their
// mappings cross a radix-leaf boundary. Readers can therefore acquire and
// prefetch each physical extent once without materializing a chunk list.
func WalkChunkTreeRuns(
	cache *PageCache,
	root PageRef,
	bounds ChunkTreeBounds,
	fn func(first, count uint32, ref PageRef) error,
) error {
	if fn == nil {
		return fmt.Errorf("%w: chunk-tree run walk", ErrInvalidWrite)
	}
	var runRef PageRef
	var first, previous, count uint32
	flush := func() error {
		if count == 0 {
			return nil
		}
		err := fn(first, count, runRef)
		count = 0
		return err
	}
	err := WalkChunkTree(cache, root, bounds, func(chunk uint32, ref PageRef) error {
		if count != 0 && ref == runRef && uint64(chunk) == uint64(previous)+1 {
			if count == ^uint32(0) {
				return ErrChunkDirectoryCorrupt
			}
			previous = chunk
			count++
			return nil
		}
		if err := flush(); err != nil {
			return err
		}
		runRef, first, previous, count = ref, chunk, chunk, 1
		return nil
	})
	if err != nil {
		return err
	}
	return flush()
}

// WalkChunkTreeFloat64Runs extends ordinary physical run coalescing across
// adjacent document groups that derive the same detached typed extent. When
// detached is true ref names PageFloat64Group; otherwise it retains the
// ordinary document or document-group reference. This lets covering scans
// admit one shared typed page once without changing the general chunk tree.
func WalkChunkTreeFloat64Runs(
	cache *PageCache,
	root PageRef,
	bounds ChunkTreeBounds,
	allocationQuantum uint32,
	fn func(first, count uint32, ref PageRef, detached bool) error,
) error {
	if fn == nil || !validPhysicalPageSize(allocationQuantum) {
		return fmt.Errorf("%w: float64 chunk-tree run walk", ErrInvalidWrite)
	}
	var runRef PageRef
	var first, previous, count uint32
	runDetached := false
	flush := func() error {
		if count == 0 {
			return nil
		}
		err := fn(first, count, runRef, runDetached)
		count = 0
		return err
	}
	err := WalkChunkTreeRuns(cache, root, bounds, func(nextFirst, nextCount uint32, document PageRef) error {
		ref := document
		detached := false
		columns, found, deriveErr := DocumentGroupFloat64Sidecar(document, allocationQuantum)
		if deriveErr != nil {
			return deriveErr
		}
		if found {
			ref, detached = columns, true
		}
		if count != 0 && detached == runDetached && ref == runRef &&
			uint64(nextFirst) == uint64(previous)+1 {
			if uint64(count)+uint64(nextCount) > uint64(^uint32(0)) {
				return ErrChunkDirectoryCorrupt
			}
			count += nextCount
			previous = nextFirst + nextCount - 1
			return nil
		}
		if err := flush(); err != nil {
			return err
		}
		runRef, runDetached = ref, detached
		first, count = nextFirst, nextCount
		previous = nextFirst + nextCount - 1
		return nil
	})
	if err != nil {
		return err
	}
	return flush()
}

func walkChunkTreePage(cache *PageCache, ref PageRef, bounds ChunkTreeBounds, expectedShift uint8, fn func(uint32, PageRef) error) error {
	lease, err := cache.Acquire(ref)
	if err != nil {
		return err
	}
	view, err := OpenChunkDirectoryPage(lease.Page(), bounds.FileEnd, bounds.NextLogicalID)
	if err != nil {
		lease.Release()
		return err
	}
	header := view.Header()
	if header.Shift != expectedShift {
		lease.Release()
		return ErrChunkDirectoryCorrupt
	}
	var refs [64]PageRef
	var lanes [64]uint8
	count := view.Len()
	bitmap := header.Bitmap
	for i := 0; i < count; i++ {
		refs[i], _ = view.RefAt(i)
		lanes[i] = uint8(bits.TrailingZeros64(bitmap))
		bitmap &= bitmap - 1
	}
	prefix := header.Prefix
	lease.Release()
	for i := 0; i < count; i++ {
		if expectedShift == 0 {
			if err := fn(prefix|uint32(lanes[i]), refs[i]); err != nil {
				return err
			}
			continue
		}
		if err := walkChunkTreePage(cache, refs[i], bounds, expectedShift-chunkDirectoryRadixBits, fn); err != nil {
			return err
		}
	}
	return nil
}

func (m *ChunkTreeMutation) retire(ref PageRef) error {
	if int(m.RetiredCount) == len(m.Retired) {
		return ErrKeyTreeDepth
	}
	m.Retired[m.RetiredCount] = ref
	m.RetiredCount++
	return nil
}

// LookupChunkTree resolves one logical chunk to its immutable document or
// multi-chunk group extent.
func LookupChunkTree(cache *PageCache, root PageRef, chunkID uint32, bounds ChunkTreeBounds) (PageRef, bool, error) {
	if root == (PageRef{}) {
		return PageRef{}, false, nil
	}
	if cache == nil {
		return PageRef{}, false, fmt.Errorf("%w: nil chunk-tree cache", ErrInvalidWrite)
	}
	ref := root
	for expectedShift := uint8(30); ; expectedShift -= chunkDirectoryRadixBits {
		lease, err := cache.Acquire(ref)
		if err != nil {
			return PageRef{}, false, err
		}
		view, err := OpenChunkDirectoryPage(lease.Page(), bounds.FileEnd, bounds.NextLogicalID)
		if err != nil {
			lease.Release()
			return PageRef{}, false, err
		}
		if view.Header().Shift != expectedShift || view.Header().Prefix != chunkDirectoryPrefix(chunkID, expectedShift) {
			lease.Release()
			return PageRef{}, false, ErrChunkDirectoryCorrupt
		}
		next, ok := view.Lookup(chunkID)
		lease.Release()
		if !ok {
			return PageRef{}, false, nil
		}
		if expectedShift == 0 {
			return next, true, nil
		}
		ref = next
	}
}

// ChunkTreeHasOtherReference reports whether any chunk in [first, first+count)
// except exclude still names want. It opens each covered 64-lane leaf once,
// rather than performing one full radix lookup per chunk. Compact document
// groups cover at most 128 rows, so their last-reference retirement touches at
// most three leaf paths regardless of database size.
func ChunkTreeHasOtherReference(
	cache *PageCache,
	root PageRef,
	first uint32,
	count uint16,
	exclude uint32,
	want PageRef,
	bounds ChunkTreeBounds,
) (bool, error) {
	return chunkTreeHasOtherReference(
		cache, root, first, count, exclude, want, 0, bounds,
	)
}

// ChunkTreeHasOtherFloat64Sidecar reports whether another document-group
// mapping in a typed extent's coverage derives want. Shared typed sidecars are
// retired only after both the touched document group and every other deriving
// group have disappeared from the old generation.
func ChunkTreeHasOtherFloat64Sidecar(
	cache *PageCache,
	root PageRef,
	first uint32,
	count uint16,
	exclude uint32,
	want PageRef,
	allocationQuantum uint32,
	bounds ChunkTreeBounds,
) (bool, error) {
	if want.Kind != PageFloat64Group || want.Flags != 0 || want.Aux != 0 ||
		!validPhysicalPageSize(allocationQuantum) {
		return false, fmt.Errorf("%w: float64 sidecar reference", ErrInvalidWrite)
	}
	return chunkTreeHasOtherReference(
		cache, root, first, count, exclude, want, allocationQuantum, bounds,
	)
}

func chunkTreeHasOtherReference(
	cache *PageCache,
	root PageRef,
	first uint32,
	count uint16,
	exclude uint32,
	want PageRef,
	float64Quantum uint32,
	bounds ChunkTreeBounds,
) (bool, error) {
	end := uint64(first) + uint64(count)
	if cache == nil || root == (PageRef{}) || count == 0 || end > uint64(^uint32(0))+1 {
		return false, fmt.Errorf("%w: chunk-tree reference range", ErrInvalidWrite)
	}
	for leaf := uint64(first) &^ uint64(63); leaf < end; leaf += 64 {
		found, err := chunkTreeLeafHasOtherReference(
			cache, root, uint32(leaf), first, end, exclude, want, float64Quantum, bounds,
		)
		if err != nil || found {
			return found, err
		}
	}
	return false, nil
}

func chunkTreeLeafHasOtherReference(
	cache *PageCache,
	root PageRef,
	leaf uint32,
	first uint32,
	end uint64,
	exclude uint32,
	want PageRef,
	float64Quantum uint32,
	bounds ChunkTreeBounds,
) (bool, error) {
	ref := root
	for expectedShift := uint8(30); ; expectedShift -= chunkDirectoryRadixBits {
		lease, err := cache.Acquire(ref)
		if err != nil {
			return false, err
		}
		view, err := OpenChunkDirectoryPage(lease.Page(), bounds.FileEnd, bounds.NextLogicalID)
		if err != nil {
			lease.Release()
			return false, err
		}
		header := view.Header()
		if header.Shift != expectedShift || header.Prefix != chunkDirectoryPrefix(leaf, expectedShift) {
			lease.Release()
			return false, ErrChunkDirectoryCorrupt
		}
		if expectedShift == 0 {
			begin := max(uint64(first), uint64(leaf))
			limit := min(end, uint64(leaf)+64)
			for chunk := begin; chunk < limit; chunk++ {
				if uint32(chunk) == exclude {
					continue
				}
				candidate, ok := view.Lookup(uint32(chunk))
				if !ok {
					continue
				}
				if float64Quantum == 0 {
					if candidate == want {
						lease.Release()
						return true, nil
					}
					continue
				}
				if candidate.Kind == PageDocumentGroup {
					sidecar, detached, deriveErr := DocumentGroupFloat64Sidecar(
						candidate, float64Quantum,
					)
					if deriveErr != nil {
						lease.Release()
						return false, deriveErr
					}
					if detached && sidecar == want {
						lease.Release()
						return true, nil
					}
				}
			}
			lease.Release()
			return false, nil
		}
		next, ok := view.Lookup(leaf)
		lease.Release()
		if !ok {
			return false, nil
		}
		ref = next
	}
}

// UpsertChunkTree maps chunkID to one document extent.
func UpsertChunkTree(cache *PageCache, tx *WriteTransaction, root PageRef, chunkID uint32, document PageRef, bounds ChunkTreeBounds) (ChunkTreeMutation, error) {
	return mutateChunkTree(cache, tx, root, chunkID, document, false, bounds)
}

// DeleteChunkTree removes one logical chunk mapping.
func DeleteChunkTree(cache *PageCache, tx *WriteTransaction, root PageRef, chunkID uint32, bounds ChunkTreeBounds) (ChunkTreeMutation, error) {
	return mutateChunkTree(cache, tx, root, chunkID, PageRef{}, true, bounds)
}

type chunkTreeRewrite struct {
	ref     PageRef
	found   bool
	changed bool
	empty   bool
}

func mutateChunkTree(cache *PageCache, tx *WriteTransaction, root PageRef, chunkID uint32, document PageRef, deleting bool, bounds ChunkTreeBounds) (ChunkTreeMutation, error) {
	var mutation ChunkTreeMutation
	if tx == nil || !tx.active || cache == nil && root != (PageRef{}) {
		return mutation, fmt.Errorf("%w: chunk-tree mutation", ErrInvalidWrite)
	}
	if !deleting && !validChunkTreeDocumentRef(tx, document) {
		return mutation, fmt.Errorf("%w: chunk document reference", ErrInvalidWrite)
	}
	if root == (PageRef{}) {
		if deleting {
			return mutation, nil
		}
		ref, err := buildChunkTreePath(tx, chunkID, document, 30)
		if err != nil {
			return mutation, err
		}
		mutation.Root = ref
		mutation.Changed = true
		return mutation, nil
	}
	result, err := rewriteChunkTreePage(cache, tx, root, chunkID, document, deleting, bounds, 30, &mutation)
	if err != nil {
		return mutation, err
	}
	mutation.Root = result.ref
	mutation.Found = result.found
	mutation.Changed = result.changed
	return mutation, nil
}

func rewriteChunkTreePage(cache *PageCache, tx *WriteTransaction, oldRef PageRef, chunkID uint32, document PageRef, deleting bool, bounds ChunkTreeBounds, expectedShift uint8, mutation *ChunkTreeMutation) (chunkTreeRewrite, error) {
	lease, err := cache.Acquire(oldRef)
	if err != nil {
		return chunkTreeRewrite{}, err
	}
	defer lease.Release()
	view, err := OpenChunkDirectoryPage(lease.Page(), bounds.FileEnd, bounds.NextLogicalID)
	if err != nil {
		return chunkTreeRewrite{}, err
	}
	header := view.Header()
	if header.Shift != expectedShift || header.Prefix != chunkDirectoryPrefix(chunkID, expectedShift) {
		return chunkTreeRewrite{}, ErrChunkDirectoryCorrupt
	}
	lane := uint8(chunkID >> expectedShift & 63)
	bit := uint64(1) << lane
	rank := bits.OnesCount64(header.Bitmap & (bit - 1))
	var refs [65]PageRef
	count := view.Len()
	for i := 0; i < count; i++ {
		refs[i], _ = view.RefAt(i)
	}
	found := header.Bitmap&bit != 0
	if expectedShift == 0 {
		if deleting {
			if !found {
				return chunkTreeRewrite{ref: oldRef}, nil
			}
			copy(refs[rank:], refs[rank+1:count])
			count--
			header.Bitmap &^= bit
		} else if found {
			refs[rank] = document
		} else {
			copy(refs[rank+1:], refs[rank:count])
			refs[rank] = document
			count++
			header.Bitmap |= bit
		}
	} else {
		if !found {
			if deleting {
				return chunkTreeRewrite{ref: oldRef}, nil
			}
			child, err := buildChunkTreePath(tx, chunkID, document, expectedShift-chunkDirectoryRadixBits)
			if err != nil {
				return chunkTreeRewrite{}, err
			}
			copy(refs[rank+1:], refs[rank:count])
			refs[rank] = child
			count++
			header.Bitmap |= bit
		} else {
			child, err := rewriteChunkTreePage(cache, tx, refs[rank], chunkID, document, deleting, bounds, expectedShift-chunkDirectoryRadixBits, mutation)
			if err != nil {
				return chunkTreeRewrite{}, err
			}
			if !child.changed {
				return chunkTreeRewrite{ref: oldRef, found: child.found}, nil
			}
			if child.empty {
				copy(refs[rank:], refs[rank+1:count])
				count--
				header.Bitmap &^= bit
			} else {
				refs[rank] = child.ref
			}
			found = child.found
		}
	}
	if err := mutation.retire(oldRef); err != nil {
		return chunkTreeRewrite{}, err
	}
	if count == 0 {
		return chunkTreeRewrite{found: found, changed: true, empty: true}, nil
	}
	page, err := encodeChunkTreeNode(tx, oldRef.LogicalID, header.Prefix, header.Bitmap, expectedShift, refs[:count])
	if err != nil {
		return chunkTreeRewrite{}, err
	}
	return chunkTreeRewrite{ref: page.Ref(), found: found, changed: true}, nil
}

func buildChunkTreePath(tx *WriteTransaction, chunkID uint32, document PageRef, maxShift uint8) (PageRef, error) {
	shift := uint8(0)
	lane := uint8(chunkID >> shift & 63)
	child := document
	for {
		refs := [1]PageRef{child}
		page, err := encodeChunkTreeNode(tx, 0, chunkDirectoryPrefix(chunkID, shift), uint64(1)<<lane, shift, refs[:])
		if err != nil {
			return PageRef{}, err
		}
		child = page.Ref()
		if shift == maxShift {
			return child, nil
		}
		shift += chunkDirectoryRadixBits
		lane = uint8(chunkID >> shift & 63)
	}
}

func encodeChunkTreeNode(tx *WriteTransaction, logicalID uint64, prefix uint32, bitmap uint64, shift uint8, refs []PageRef) (TransactionPage, error) {
	page, err := tx.Allocate(PageChunkDirectory, tx.options.PageSize, logicalID)
	if err != nil {
		return TransactionPage{}, err
	}
	header := ChunkDirectoryHeader{
		StoreID: tx.options.StoreID, Generation: tx.options.Generation,
		LogicalID: page.Ref().LogicalID, PageSize: page.Ref().Length,
		Prefix: prefix, Bitmap: bitmap, Shift: shift,
	}
	if _, err := EncodeChunkDirectoryPage(page.Bytes(), header, refs, tx.FileEnd(), tx.NextLogicalID()); err != nil {
		return TransactionPage{}, err
	}
	if err := page.Stage(); err != nil {
		return TransactionPage{}, err
	}
	return page, nil
}

func validChunkTreeDocumentRef(tx *WriteTransaction, ref PageRef) bool {
	quantum := uint64(tx.options.PageSize)
	length := uint64(ref.Length)
	return ref.Kind == PageDocument && ref.Flags == 0 && ref.Aux == 0 && validPhysicalPageSize(ref.Length) &&
		ref.Length >= tx.options.PageSize && ref.Length%tx.options.PageSize == 0 &&
		ref.Generation != 0 && ref.Generation <= tx.options.Generation &&
		ref.LogicalID > StateRootLogicalID && ref.LogicalID < tx.NextLogicalID() &&
		ref.Offset >= uint64(superblockCopies)*quantum && ref.Offset%quantum == 0 &&
		length <= tx.FileEnd() && ref.Offset <= tx.FileEnd()-length
}
