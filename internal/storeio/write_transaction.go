package storeio

import (
	"fmt"
	"slices"
)

// WriteTransactionOptions binds one copy-on-write publication to its current
// physical and logical allocation high-water marks.
type WriteTransactionOptions struct {
	StoreID       [16]byte
	Generation    uint64
	PageSize      uint32
	FileEnd       uint64
	NextLogicalID uint64
	// Reusable contains snapshot- and recovery-safe physical extents owned by
	// the serialized caller. Allocate shrinks entries in place; Publish keeps
	// those edits and Abort restores them from ReuseJournal.
	Reusable []FreeExtent
	// ReuseJournal is caller-owned scratch with capacity for maxPages edits.
	ReuseJournal []ReuseEdit
	// SingleReuseExtent bounds one transaction to one free-tree edit.
	SingleReuseExtent bool
}

// ReuseEdit is one allocator rollback record. Callers supply storage but must
// otherwise treat values as opaque.
type ReuseEdit struct {
	Index  uint32
	Before FreeExtent
}

// WriteTransaction reserves worst-case committer capacity, allocates immutable
// append extents, and publishes exactly one state root. It is single-owner and
// must be aborted or published.
type WriteTransaction struct {
	committer    *Committer
	cache        *PageCache
	batch        *Batch
	options      WriteTransactionOptions
	fileEnd      uint64
	nextID       uint64
	allocated    int
	reuseEdits   []ReuseEdit
	reuseEnabled bool
	reuseIndex   int
	reuseStart   int
	reuseEnd     int
	reuseExclude int
	active       bool
}

// TransactionPage is one staging buffer and its prospective durable reference.
// The value borrows its transaction and is invalid after Publish or Abort.
type TransactionPage struct {
	tx    *WriteTransaction
	index int
	ref   PageRef
	bytes []byte
}

// Ref returns the prospective immutable page reference.
func (p TransactionPage) Ref() PageRef { return p.ref }

// Bytes returns the exact capacity-clipped encoding buffer.
func (p TransactionPage) Bytes() []byte { return p.bytes }

// Stage verifies the complete common page, admits it as dirty when applicable,
// and records its positional write.
func (p TransactionPage) Stage() error {
	if p.tx == nil || !p.tx.active || p.index < 0 || p.index >= p.tx.allocated || len(p.bytes) != int(p.ref.Length) {
		return ErrBatchState
	}
	header, _, err := OpenPage(p.bytes)
	if err != nil {
		return err
	}
	if header.StoreID != p.tx.options.StoreID || header.Generation != p.tx.options.Generation ||
		header.LogicalID != p.ref.LogicalID || header.PageSize != p.ref.Length ||
		header.Kind != p.ref.Kind || header.Flags != p.ref.Flags {
		return fmt.Errorf("%w: staged page identity", ErrInvalidWrite)
	}
	write := &p.tx.batch.pages[p.index]
	if write.Length != 0 {
		return ErrBatchState
	}
	if p.tx.cache != nil && p.ref.LogicalID != StateRootLogicalID {
		if err := p.tx.cache.AdmitDirty(p.ref, p.bytes, p.tx.options.Generation); err != nil {
			return err
		}
	}
	return p.tx.batch.setStorePage(
		p.index, int64(p.ref.Offset), int(p.ref.Length), p.ref.Kind,
	)
}

// BeginWriteTransaction acquires bounded worst-case staging capacity.
func BeginWriteTransaction(committer *Committer, cache *PageCache, maxPages int, options WriteTransactionOptions) (*WriteTransaction, error) {
	if committer == nil || options.StoreID == ([16]byte{}) || options.Generation == 0 ||
		!validPhysicalPageSize(options.PageSize) || options.FileEnd < uint64(superblockCopies)*uint64(options.PageSize) ||
		options.FileEnd%uint64(options.PageSize) != 0 || options.FileEnd > maxSuperblockFileOffset ||
		options.NextLogicalID <= StateRootLogicalID {
		return nil, fmt.Errorf("%w: transaction identity or bounds", ErrInvalidWrite)
	}
	batch, err := committer.Begin(maxPages)
	if err != nil {
		return nil, err
	}
	return &WriteTransaction{
		committer: committer, cache: cache, batch: batch, options: options,
		fileEnd: options.FileEnd, nextID: options.NextLogicalID,
		reuseEdits: options.ReuseJournal[:0], reuseEnabled: true, reuseIndex: -1,
		reuseEnd: len(options.Reusable), reuseExclude: -1, active: true,
	}, nil
}

// Allocate reserves one append-only extent. logicalID zero allocates a new
// logical identity; non-zero rewrites that logical page at the new generation.
func (t *WriteTransaction) Allocate(kind PageKind, length uint32, logicalID uint64) (TransactionPage, error) {
	if t == nil || !t.active || t.allocated >= len(t.batch.pages) || !validPageKind(kind) ||
		!validPhysicalPageSize(length) || length < t.options.PageSize || length%t.options.PageSize != 0 {
		return TransactionPage{}, ErrTooManyPages
	}
	if !variableTransactionExtent(kind) && length != t.options.PageSize {
		return TransactionPage{}, fmt.Errorf("%w: variable metadata extent", ErrInvalidWrite)
	}
	if uint64(length) > uint64(t.committer.bufferSize) {
		return TransactionPage{}, ErrTooManyPages
	}
	if logicalID == 0 {
		logicalID = t.nextID
		if logicalID <= StateRootLogicalID || logicalID == ^uint64(0) {
			return TransactionPage{}, fmt.Errorf("%w: logical id exhausted", ErrInvalidWrite)
		}
		t.nextID++
	} else if logicalID != StateRootLogicalID && (logicalID <= StateRootLogicalID || logicalID >= t.nextID) {
		return TransactionPage{}, fmt.Errorf("%w: replacement logical id", ErrInvalidWrite)
	}
	if kind == PageStateRoot && logicalID != StateRootLogicalID || kind != PageStateRoot && logicalID == StateRootLogicalID {
		return TransactionPage{}, fmt.Errorf("%w: state-root logical id", ErrInvalidWrite)
	}
	offset, reused, err := t.allocatePhysical(length)
	if err != nil {
		return TransactionPage{}, err
	}
	index := t.allocated
	buffer, err := t.batch.PageBuffer(index)
	if err != nil {
		return TransactionPage{}, err
	}
	ref := PageRef{
		Offset: offset, LogicalID: logicalID, Generation: t.options.Generation,
		Length: length, Kind: kind,
	}
	if !reused {
		t.fileEnd += uint64(length)
	}
	t.allocated++
	return TransactionPage{tx: t, index: index, ref: ref, bytes: buffer[:int(length):int(length)]}, nil
}

// variableTransactionExtent reports immutable value/accelerator pages whose
// packed size is intentionally independent of the metadata allocation
// quantum. Tree nodes and publication roots remain exactly one PageSize so
// their bounded copy-on-write geometry cannot silently expand.
func variableTransactionExtent(kind PageKind) bool {
	switch kind {
	case PageDocument, PageOverflow, PageFloat64Stripe,
		PageIndexGroupCatalog:
		return true
	default:
		return false
	}
}

func (t *WriteTransaction) allocatePhysical(length uint32) (uint64, bool, error) {
	want := uint64(length)
	if !t.reuseEnabled {
		if want > maxSuperblockFileOffset-t.fileEnd {
			return 0, false, fmt.Errorf("%w: physical file exhausted", ErrInvalidWrite)
		}
		return t.fileEnd, false, nil
	}
	if t.options.SingleReuseExtent && t.reuseIndex >= 0 {
		extent := t.options.Reusable[t.reuseIndex]
		if extent.Length < want {
			return t.fileEnd, false, nil
		}
		return t.allocateFromReusable(t.reuseIndex, extent, want)
	}
	selected := -1
	for i := t.reuseStart; i < t.reuseEnd; i++ {
		if i == t.reuseExclude {
			continue
		}
		extent := t.options.Reusable[i]
		if extent.Length < want {
			continue
		}
		if selected < 0 || t.options.SingleReuseExtent && extent.Length > t.options.Reusable[selected].Length {
			selected = i
			if !t.options.SingleReuseExtent {
				break
			}
		}
	}
	if selected >= 0 {
		t.reuseIndex = selected
		return t.allocateFromReusable(selected, t.options.Reusable[selected], want)
	}
	if want > maxSuperblockFileOffset-t.fileEnd {
		return 0, false, fmt.Errorf("%w: physical file exhausted", ErrInvalidWrite)
	}
	return t.fileEnd, false, nil
}

func (t *WriteTransaction) allocateFromReusable(index int, extent FreeExtent, want uint64) (uint64, bool, error) {
	if extent.Offset < 2*uint64(t.options.PageSize) || extent.Offset%uint64(t.options.PageSize) != 0 ||
		extent.Length%uint64(t.options.PageSize) != 0 || extent.Length > t.options.FileEnd ||
		extent.Offset > t.options.FileEnd-extent.Length ||
		extent.RetiredGeneration == 0 || extent.RetiredGeneration >= t.options.Generation {
		return 0, false, fmt.Errorf("%w: reusable extent", ErrInvalidWrite)
	}
	if len(t.reuseEdits) == cap(t.reuseEdits) {
		return 0, false, ErrTooManyPages
	}
	t.reuseEdits = append(t.reuseEdits, ReuseEdit{Index: uint32(index), Before: extent})
	offset := extent.Offset + extent.Length - want
	extent.Length -= want
	if extent.Length == 0 {
		extent = FreeExtent{}
	}
	t.options.Reusable[index] = extent
	return offset, true, nil
}

// DisableReuse seals allocator edits for this transaction. Subsequent
// metadata pages append above FileEnd, allowing a free-tree root to describe
// the final reusable pool without recursively consuming from itself.
func (t *WriteTransaction) DisableReuse() {
	if t != nil {
		t.reuseEnabled = false
	}
}

// SetReuseRange starts a new bounded allocation phase over [start,end),
// optionally excluding one extent whose value is being encoded into metadata.
func (t *WriteTransaction) SetReuseRange(start, end, exclude int) error {
	if t == nil || !t.active || start < 0 || end < start || end > len(t.options.Reusable) || exclude >= end {
		return fmt.Errorf("%w: reusable range", ErrInvalidWrite)
	}
	t.reuseEnabled = true
	t.reuseIndex = -1
	t.reuseStart = start
	t.reuseEnd = end
	t.reuseExclude = exclude
	return nil
}

// ReuseEdits returns the caller-owned allocator journal accumulated so far.
func (t *WriteTransaction) ReuseEdits() []ReuseEdit {
	if t == nil {
		return nil
	}
	return t.reuseEdits
}

// FileEnd returns the prospective exclusive allocation high-water mark.
func (t *WriteTransaction) FileEnd() uint64 {
	if t == nil {
		return 0
	}
	return t.fileEnd
}

// NextLogicalID returns the prospective logical high-water mark.
func (t *WriteTransaction) NextLogicalID() uint64 {
	if t == nil {
		return 0
	}
	return t.nextID
}

// Generation returns the transaction publication generation.
func (t *WriteTransaction) Generation() uint64 {
	if t == nil {
		return 0
	}
	return t.options.Generation
}

// Publish selects stateRef through the alternate superblock. stateRef must be
// a staged state-root page from this transaction. free root fields may retain
// an older immutable tree or be zero when no free extents exist.
func (t *WriteTransaction) Publish(stateRef PageRef, stateChecksum uint32, freeOffset uint64, freeLength, freeChecksum uint32) error {
	if t == nil || !t.active || stateRef.Kind != PageStateRoot || stateRef.LogicalID != StateRootLogicalID ||
		stateRef.Generation != t.options.Generation || stateRef.Length != t.options.PageSize {
		return ErrBatchState
	}
	stagedState := false
	for i := 0; i < t.allocated; i++ {
		write := t.batch.pages[i]
		if write.Length == 0 {
			return fmt.Errorf("%w: unstaged transaction page", ErrInvalidWrite)
		}
		if uint64(write.Offset) == stateRef.Offset && write.Length == stateRef.Length {
			page := t.committer.buffers[write.Buffer][:write.Length]
			if PageChecksum(page) != stateChecksum {
				return fmt.Errorf("%w: state-root checksum", ErrInvalidWrite)
			}
			stagedState = true
		}
	}
	if !stagedState {
		return fmt.Errorf("%w: state root was not staged", ErrInvalidWrite)
	}
	if err := t.batch.ResizePages(t.allocated); err != nil {
		return err
	}
	// Reused best-fit extents need not be selected in physical order. Device
	// commits require a sorted, non-overlapping write vector for deterministic
	// validation and sequential submission.
	slices.SortFunc(t.batch.pages, func(a, b Write) int {
		if a.Offset < b.Offset {
			return -1
		}
		if a.Offset > b.Offset {
			return 1
		}
		return 0
	})
	root := Superblock{
		StoreID: t.options.StoreID, Generation: t.options.Generation,
		StateOffset: stateRef.Offset, StateLength: stateRef.Length, StateChecksum: stateChecksum,
		FileEnd: t.fileEnd, FreeOffset: freeOffset, FreeLength: freeLength,
		FreeChecksum: freeChecksum, PageSize: t.options.PageSize,
	}
	if err := t.batch.SetSuperblock(root); err != nil {
		return err
	}
	if err := t.batch.Publish(t.options.Generation); err != nil {
		return err
	}
	t.active = false
	t.batch = nil
	return nil
}

// Abort releases every reserved buffer. It is idempotent after a successful
// Publish only in the sense that it returns ErrBatchState and changes nothing.
func (t *WriteTransaction) Abort() error {
	if t == nil || !t.active || t.batch == nil {
		return ErrBatchState
	}
	err := t.batch.Abort()
	if err == nil {
		for i := len(t.reuseEdits) - 1; i >= 0; i-- {
			edit := t.reuseEdits[i]
			t.options.Reusable[edit.Index] = edit.Before
		}
		t.reuseEdits = t.reuseEdits[:0]
		t.active = false
		t.batch = nil
		if t.cache != nil {
			err = t.cache.DiscardDirty(t.options.Generation)
		}
	}
	return err
}
