package storeio

import (
	"fmt"
)

// DeferredPage is one manual-checkpoint data write whose immutable destination
// is allocated and published now, while its complete page image is copied into
// the already-reserved staging buffer immediately before the checkpoint cut.
//
// It exists for buffered canonical frames: acknowledgement edits the sole
// resident frame, and repeated edits can coalesce before one checkpoint copy.
// A DeferredPage is value-only and may outlive its WriteTransaction, but it is
// valid only until the checkpoint that settles its batch.
type DeferredPage struct {
	batch *Batch
	index int
	ref   PageRef
	store [16]byte
}

// StageDeferred records the page's destination without reading p.Bytes. It is
// restricted to manual checkpoint committers because an automatic worker could
// otherwise observe the descriptor before its owner has captured the image.
func (p TransactionPage) StageDeferred() (DeferredPage, error) {
	if p.tx == nil || !p.tx.active || p.tx.batch == nil ||
		!p.tx.committer.options.ManualCheckpoint ||
		p.index < 0 || p.index >= p.tx.allocated ||
		len(p.bytes) != int(p.ref.Length) {
		return DeferredPage{}, ErrBatchState
	}
	writeIndex := p.index + int(p.tx.batch.materializationPatchCount)
	if writeIndex < 0 || writeIndex >= len(p.tx.batch.pages) {
		return DeferredPage{}, ErrBatchState
	}
	write := &p.tx.batch.pages[writeIndex]
	if write.Length != 0 {
		return DeferredPage{}, ErrBatchState
	}
	write.Offset = int64(p.ref.Offset)
	write.Length = p.ref.Length
	write.kind = p.ref.Kind
	write.pendingFlags |= pendingWriteDeferred
	return DeferredPage{
		batch: p.tx.batch,
		index: writeIndex,
		ref:   p.ref,
		store: p.tx.options.StoreID,
	}, nil
}

// NeedsCapture reports whether the deferred destination remains reachable from
// a pending checkpoint root. Buffered supersession may prove it unreachable
// and recycle its staging buffer before the checkpoint; in that case callers
// must not attempt to reacquire a canonical frame that may already be gone.
func (p DeferredPage) NeedsCapture() (bool, error) {
	if p.batch == nil || p.batch.committer == nil {
		return false, ErrBatchState
	}
	c := p.batch.committer
	if !c.options.ManualCheckpoint {
		return false, ErrBatchState
	}
	c.manualMu.Lock()
	defer c.manualMu.Unlock()
	write, err := p.pendingWriteLocked()
	if err != nil {
		return false, err
	}
	if write.pendingFlags&pendingWriteSuperseded != 0 ||
		write.pendingFlags&pendingWriteDeferredCaptured != 0 {
		return false, nil
	}
	return true, nil
}

// Capture verifies and copies one leased canonical page into its reserved
// staging buffer. The cache lease is the validation authority; the manual
// committer mutex linearizes the copy before checkpoint authorization and
// against producer-side supersession.
func (p DeferredPage) Capture(lease *PageLease) error {
	if p.batch == nil || p.batch.committer == nil {
		return ErrBatchState
	}
	if lease == nil {
		return ErrInvalidWrite
	}
	src := lease.Page()
	if len(src) < int(p.ref.Length) {
		return fmt.Errorf("%w: deferred page bytes", ErrInvalidWrite)
	}
	src = src[:int(p.ref.Length)]
	header := lease.Header()
	c := p.batch.committer
	if header.StoreID != p.store ||
		header.Generation != p.ref.Generation ||
		header.LogicalID != p.ref.LogicalID ||
		header.PageSize != p.ref.Length ||
		header.Kind != p.ref.Kind ||
		header.Flags != p.ref.Flags {
		return fmt.Errorf("%w: deferred page identity", ErrInvalidWrite)
	}

	c.manualMu.Lock()
	defer c.manualMu.Unlock()
	write, err := p.pendingWriteLocked()
	if err != nil {
		return err
	}
	if write.pendingFlags&pendingWriteSuperseded != 0 ||
		write.pendingFlags&pendingWriteDeferredCaptured != 0 {
		return nil
	}
	buffer := c.buffers[write.Buffer]
	if len(buffer) < len(src) {
		return ErrInvalidWrite
	}
	copy(buffer[:len(src)], src)
	write.pendingFlags |= pendingWriteDeferredCaptured
	return nil
}

func (p DeferredPage) pendingWriteLocked() (*Write, error) {
	if p.batch.state.Load() != batchPublished ||
		p.batch.generation != p.ref.Generation {
		return nil, ErrBatchState
	}
	if p.index >= 0 && p.index < len(p.batch.pages) {
		write := &p.batch.pages[p.index]
		if deferredWriteMatches(write, p.ref) {
			return write, nil
		}
	}
	// PublishInline sorts physical writes after StageDeferred returns. The
	// handle's index is therefore only a pre-publication hint; the immutable
	// destination identity is the post-publication authority.
	for index := range p.batch.pages {
		write := &p.batch.pages[index]
		if deferredWriteMatches(write, p.ref) {
			return write, nil
		}
	}
	return nil, ErrBatchState
}

func deferredWriteMatches(write *Write, ref PageRef) bool {
	return write != nil && write.Offset >= 0 &&
		uint64(write.Offset) == ref.Offset &&
		write.Length == ref.Length && write.kind == ref.Kind &&
		write.pendingFlags&pendingWriteDeferred != 0
}
