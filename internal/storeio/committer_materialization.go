package storeio

import "fmt"

// materializationDevice executes the three durability phases without hiding
// their ordering inside the ordinary two-phase Commit contract.
type materializationDevice interface {
	CommitMaterialized(
		journal Write, targets []Write, root Write,
	) (completedPhases uint32, err error)
}

// MaterializationSequence returns the exact sequence the caller must encode
// into this batch's journal capsule. It remains reserved only by the
// single-producer ownership of Batch; Abort leaves it available to the next
// materialized batch, while a successful Publish advances it.
func (b *Batch) MaterializationSequence() (uint64, error) {
	if b == nil || b.state.Load() != batchOwned || !b.materialized {
		return 0, ErrBatchState
	}
	return b.committer.materializationNextSequence.Load(), nil
}

// BeginMaterialized acquires one isolated canonical-materialization batch.
// targetWriteCount is the exact number of journal patch spans, not the number
// of complete target pages. In addition to those sparse-sector buffers and the
// alternate-root buffer owned by Begin, this reserves one dedicated journal
// buffer. Materialized batches are hard queue boundaries: they are never
// grouped with another generation and never enter the coalescing window.
func (c *Committer) BeginMaterialized(targetWriteCount int) (*Batch, error) {
	if c == nil {
		return nil, ErrClosed
	}
	if c.options.MaterializationDamageGranule == 0 {
		return nil, ErrUnsupported
	}
	if targetWriteCount <= 0 {
		return nil, fmt.Errorf("%w: materialization requires a target write", ErrInvalidWrite)
	}
	if targetWriteCount > MaterializationJournalMaxPatches ||
		targetWriteCount > c.options.MaxPagesPerBatch ||
		targetWriteCount > c.bufferCount-2 {
		return nil, ErrTooManyPages
	}
	batch, err := c.Begin(targetWriteCount)
	if err != nil {
		return nil, err
	}
	buffer, err := c.acquire(c.freeBuffers)
	if err != nil {
		_ = batch.Abort()
		return nil, err
	}
	batch.journal = Write{Buffer: uint16(buffer)}
	batch.materialized = true
	return batch, nil
}

// MaterializationJournalBuffer returns the dedicated fixed-capsule buffer.
// The caller encodes one complete capsule, then calls
// SealMaterializationJournal before staging complete target after-images.
func (b *Batch) MaterializationJournalBuffer() ([]byte, error) {
	if b == nil || b.state.Load() != batchOwned || !b.materialized {
		return nil, ErrBatchState
	}
	return b.committer.buffers[b.journal.Buffer], nil
}

// SealMaterializationJournal validates and freezes the capsule framing. The
// physical offsets are never caller supplied: Publish derives the exact two
// allocator-excluded slots from MutableStoreLayout and selects the
// recovery-seeded next slot.
func (b *Batch) SealMaterializationJournal() error {
	if b == nil || b.state.Load() != batchOwned || !b.materialized {
		return ErrBatchState
	}
	buffer := b.committer.buffers[b.journal.Buffer]
	view, err := OpenMaterializationJournal(buffer[:MaterializationJournalSize])
	if err != nil {
		return err
	}
	header := view.Header()
	if header.SectorSize != b.committer.options.MaterializationDamageGranule ||
		view.PatchLen() != len(b.pages) {
		return fmt.Errorf("%w: materialization journal geometry", ErrInvalidWrite)
	}
	b.journal.Length = MaterializationJournalSize
	b.journalCapsuleChecksum = PageChecksum(buffer[:MaterializationJournalSize])
	b.materializationTargetMask = 0
	clear(b.materializationPatchChecksums[:])
	return nil
}

// StageMaterializationTarget proves one complete after-image against the
// journal's exact AfterChecksum and PageRef, then extracts only that target's
// aligned dirty sectors into Device buffers. The complete image remains
// caller-owned and can be discarded on return. Publish rechecks a checksum for
// every extracted sector, so a stale retained sparse buffer cannot slip under
// the new root.
func (b *Batch) StageMaterializationTarget(targetRank int, after []byte) error {
	if b == nil || b.state.Load() != batchOwned || !b.materialized ||
		b.journal.Length != MaterializationJournalSize {
		return ErrBatchState
	}
	journalBuffer := b.committer.buffers[b.journal.Buffer]
	if PageChecksum(journalBuffer[:MaterializationJournalSize]) != b.journalCapsuleChecksum {
		return ErrMaterializationJournalCorrupt
	}
	view, err := OpenMaterializationJournal(journalBuffer[:MaterializationJournalSize])
	if err != nil {
		return err
	}
	target, ok := view.TargetAt(targetRank)
	if !ok || len(after) != int(target.Ref.Length) ||
		PageChecksum(after) != target.AfterChecksum {
		return fmt.Errorf("%w: materialization after-image checksum", ErrInvalidWrite)
	}
	header, _, err := OpenPage(after)
	if err != nil || header.StoreID != view.Header().StoreID ||
		!materializationPageHeaderMatchesRef(header, target.Ref) {
		return fmt.Errorf("%w: materialization after-image identity", ErrInvalidWrite)
	}
	first, count, ok := view.TargetPatchRange(targetRank)
	if !ok || count == 0 {
		return fmt.Errorf("%w: materialization target patches", ErrInvalidWrite)
	}
	for patchRank := first; patchRank < first+count; patchRank++ {
		patch, _ := view.PatchAt(patchRank)
		end := uint64(patch.Offset) + uint64(len(patch.Data))
		if len(patch.Data) > b.committer.bufferSize || end > uint64(len(after)) {
			return fmt.Errorf("%w: materialization sparse sector", ErrInvalidWrite)
		}
		buffer := b.committer.buffers[b.pages[patchRank].Buffer]
		copy(buffer, after[patch.Offset:uint32(end)])
		b.pages[patchRank].Offset = int64(target.Ref.Offset + uint64(patch.Offset))
		b.pages[patchRank].Length = uint32(len(patch.Data))
		b.pages[patchRank].kind = 0
		b.materializationPatchChecksums[patchRank] =
			PageChecksum(buffer[:len(patch.Data)])
	}
	b.materializationTargetMask |= uint8(1) << targetRank
	return nil
}

// InitializeMaterializationRecovery seeds the next capsule sequence and the
// physical slot opposite the checksum-valid capsule selected by recovery. It
// is independent of root-slot state and must run before the first materialized
// publication. Fresh files require no call: they start at sequence one, slot
// zero.
func (c *Committer) InitializeMaterializationRecovery(
	selectedSequence uint64,
	selectedSlot int,
) error {
	if c == nil || c.closing.Load() ||
		c.options.MaterializationDamageGranule == 0 ||
		selectedSequence == 0 || selectedSequence == ^uint64(0) ||
		selectedSlot < 0 || selectedSlot >= materializationJournalCopies ||
		c.materializationPublished.Load() ||
		!c.materializationRecoveryInitialized.CompareAndSwap(false, true) {
		return ErrGenerationOrder
	}
	c.materializationNextSequence.Store(selectedSequence + 1)
	c.materializationNextSlot.Store(uint32(selectedSlot ^ 1))
	return nil
}

func (c *Committer) validateMaterializedBatch(batch *Batch, generation uint64) (uint64, error) {
	if batch.journal.Length != MaterializationJournalSize {
		return 0, fmt.Errorf("%w: materialization journal is not sealed", ErrInvalidWrite)
	}
	journalBuffer := c.buffers[batch.journal.Buffer]
	if PageChecksum(journalBuffer[:MaterializationJournalSize]) != batch.journalCapsuleChecksum {
		return 0, ErrMaterializationJournalCorrupt
	}
	view, err := OpenMaterializationJournal(journalBuffer[:MaterializationJournalSize])
	if err != nil {
		return 0, err
	}
	header := view.Header()
	if header.TargetGeneration != generation {
		return 0, ErrGenerationOrder
	}
	if header.Sequence != c.materializationNextSequence.Load() ||
		header.Sequence == ^uint64(0) ||
		header.SectorSize != c.options.MaterializationDamageGranule ||
		len(batch.pages) != view.PatchLen() {
		return 0, fmt.Errorf("%w: materialization sequence or geometry", ErrInvalidWrite)
	}
	wantTargets := uint8(1)<<view.Len() - 1
	if batch.materializationTargetMask != wantTargets {
		return 0, fmt.Errorf("%w: incomplete materialization after-images", ErrInvalidWrite)
	}

	if batch.root.Length < InlineSuperblockSize {
		return 0, fmt.Errorf("%w: materialization requires an inline root", ErrInvalidWrite)
	}
	rootBuffer := c.buffers[batch.root.Buffer]
	root, err := DecodeInlineSuperblock(rootBuffer[:batch.root.Length])
	if err != nil {
		return 0, err
	}
	if batch.rootGeneration != generation ||
		root.Generation != generation || root.StoreID != header.StoreID ||
		root.PageSize != header.PageSize || root.PageSize != batch.root.Length {
		return 0, fmt.Errorf("%w: materialization root identity", ErrInvalidWrite)
	}
	layout, err := MutableStoreLayout(root.PageSize)
	if err != nil {
		return 0, err
	}
	journalSlot := c.materializationNextSlot.Load()
	if journalSlot >= materializationJournalCopies {
		return 0, fmt.Errorf("%w: materialization journal slot", ErrInvalidWrite)
	}
	batch.journal.Offset = int64(layout.MaterializationJournalOffsets[journalSlot])

	for rank, write := range batch.pages {
		patch, ok := view.PatchAt(rank)
		if !ok {
			return 0, fmt.Errorf("%w: materialization patch rank", ErrInvalidWrite)
		}
		target, ok := view.TargetAt(int(patch.Target))
		if !ok || target.Ref.Offset+uint64(target.Ref.Length) > root.FileEnd {
			return 0, fmt.Errorf("%w: materialization target rank", ErrInvalidWrite)
		}
		offset := target.Ref.Offset + uint64(patch.Offset)
		if offset > uint64(^uint64(0)>>1) ||
			write.Offset != int64(offset) ||
			uint64(write.Length) != uint64(len(patch.Data)) ||
			uint64(write.Offset) < layout.DataStart ||
			PageChecksum(c.buffers[write.Buffer][:write.Length]) !=
				batch.materializationPatchChecksums[rank] {
			return 0, fmt.Errorf("%w: unjournaled materialization write", ErrInvalidWrite)
		}
	}
	return header.Sequence, nil
}

func (c *Committer) commitMaterialized(device Device, batch *Batch) error {
	layout, err := MutableStoreLayout(batch.root.Length)
	if err != nil {
		return err
	}
	if batch.journalSlot >= materializationJournalCopies ||
		batch.journal.Offset !=
			int64(layout.MaterializationJournalOffsets[batch.journalSlot]) {
		return fmt.Errorf("%w: materialization journal slot changed", ErrInvalidWrite)
	}

	rootSlot := c.nextRootSlot.Load()
	if batch.rootGeneration != 0 {
		batch.root.Offset = int64(layout.RootOffsets[rootSlot])
	}
	var targetBytes uint64
	for _, write := range batch.pages {
		targetBytes += uint64(write.Length)
	}
	materializer, ok := device.(materializationDevice)
	if !ok {
		return ErrUnsupported
	}
	completed, commitErr := materializer.CommitMaterialized(
		batch.journal, batch.pages, batch.root,
	)
	if completed > 3 {
		return fmt.Errorf("%w: materialization phase count", ErrInvalidWrite)
	}
	c.deviceCommits.Add(uint64(completed))
	if completed >= 1 {
		c.deviceBytes.Add(uint64(batch.journal.Length))
		c.materializationJournalBytes.Add(uint64(batch.journal.Length))
	}
	if completed >= 2 {
		c.deviceBytes.Add(targetBytes)
		c.materializationTargetBytes.Add(targetBytes)
	}
	if completed >= 3 {
		c.deviceBytes.Add(uint64(batch.root.Length))
	}
	if commitErr != nil {
		if completed >= 2 {
			return commitOutcomeUnknown(commitErr)
		}
		return commitErr
	}
	if completed != 3 {
		return fmt.Errorf("%w: incomplete materialization phases", ErrInvalidWrite)
	}

	if batch.rootGeneration != 0 {
		c.nextRootSlot.Store(rootSlot ^ 1)
		// The older physical root can reference the canonical bytes just
		// overwritten. The retained capsule makes it crash-recoverable, but it
		// is not an independently readable fallback after success.
		c.fallback.Store(batch.generation)
	}
	c.batchesDone.Add(1)
	c.materializedDone.Add(1)
	for old := c.largestGroup.Load(); old < 1 &&
		!c.largestGroup.CompareAndSwap(old, 1); old = c.largestGroup.Load() {
	}
	c.durable.Store(batch.generation)
	if callbacks := c.callbacks.Load(); callbacks != nil &&
		callbacks.durable != nil {
		callbacks.durable(batch.generation)
	}
	// Match the ordinary worker's physical-durable/callback-settled split.
	c.settled.Store(batch.generation)
	c.broadcast()
	c.release(batch)
	return nil
}
