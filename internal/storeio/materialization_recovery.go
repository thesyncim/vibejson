package storeio

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// MutableInlineRecovery is the value-only result of recovering an inline-root
// mutable Store. JournalSlot is -1 and JournalSequence is zero when neither
// fixed capsule slot contains a valid committed journal.
//
// The journal coordinates returned here seed the next writer: it must advance
// JournalSequence and write the slot opposite JournalSlot. Deriving the slot
// from sequence parity after recovery is unsafe because one physical slot may
// have been torn independently of the sequence it was intended to carry.
type MutableInlineRecovery struct {
	Root               InlineSuperblock
	State              StateRoot
	RootSlot           int
	FallbackGeneration uint64
	JournalSlot        int
	JournalSequence    uint64
}

// RecoverMutableInlineStateRoot recovers inline roots together with the
// allocator-excluded materialization undo capsules.
//
// Recovery first orders structurally valid roots without dereferencing their
// pages. It then resolves the newest valid capsule against each root candidate.
// A candidate older than the capsule target is exposed only after every target
// has been preflighted, rolled back, and synchronized. A candidate at or beyond
// the target generation never causes a rollback.
//
// pageScratch belongs to the caller and must hold the largest top-level
// reference or journal target named by either root. The fixed root and journal
// records use separate stack storage because MaterializationJournalView borrows
// its encoded capsule for the complete recovery operation.
//
// The caller must hold the Store writer lock. It must not open a PageCache or
// Committer over file until this function returns successfully.
func RecoverMutableInlineStateRoot(
	file *os.File,
	pageSize, damageGranule uint32,
	pageScratch []byte,
) (MutableInlineRecovery, error) {
	return recoverMutableInlineStateRoot(
		file, pageSize, damageGranule, pageScratch,
		func() error { return dataSync(file) },
	)
}

func recoverMutableInlineStateRoot(
	file *os.File,
	pageSize, damageGranule uint32,
	pageScratch []byte,
	syncFile func() error,
) (MutableInlineRecovery, error) {
	if file == nil || syncFile == nil || !validPhysicalPageSize(pageSize) ||
		damageGranule < MaterializationJournalMinSectorSize ||
		damageGranule&(damageGranule-1) != 0 ||
		damageGranule > pageSize || pageSize%damageGranule != 0 {
		return MutableInlineRecovery{}, fmt.Errorf(
			"%w: invalid mutable recovery file or damage granule", ErrInvalidWrite,
		)
	}
	layout, err := MutableStoreLayout(pageSize)
	if err != nil {
		return MutableInlineRecovery{}, err
	}

	var rootRecords [superblockCopies][InlineSuperblockSize]byte
	for slot := range rootRecords {
		if err := readRecoveryRecord(
			file, rootRecords[slot][:], int64(layout.RootOffsets[slot]),
		); err != nil {
			return MutableInlineRecovery{}, err
		}
	}
	candidates, candidateCount, err := orderedInlineSuperblocks(
		rootRecords[0][:], rootRecords[1][:],
	)
	if err != nil {
		return MutableInlineRecovery{}, err
	}

	info, err := file.Stat()
	if err != nil {
		return MutableInlineRecovery{}, err
	}
	if info.Size() < 0 {
		return MutableInlineRecovery{}, ErrSuperblockNotFound
	}
	fileSize := uint64(info.Size())

	var journalRecords [materializationJournalCopies][MaterializationJournalSize]byte
	for slot := range journalRecords {
		if err := readRecoveryRecord(
			file, journalRecords[slot][:],
			int64(layout.MaterializationJournalOffsets[slot]),
		); err != nil {
			return MutableInlineRecovery{}, err
		}
	}
	journal, journalSlot, journalPresent, err := selectRecoveryMaterializationJournal(
		journalRecords[0][:], journalRecords[1][:],
	)
	if err != nil {
		return MutableInlineRecovery{}, err
	}
	if journalPresent {
		header := journal.Header()
		newest := candidates[0].root
		if header.StoreID != newest.StoreID ||
			header.PageSize != pageSize ||
			header.SectorSize != damageGranule {
			return MutableInlineRecovery{}, ErrMaterializationJournalConflict
		}
	}

	scratchNeed := int(pageSize)
	for rank := 0; rank < candidateCount; rank++ {
		scratchNeed = max(scratchNeed, inlineRecoveryScratchNeed(candidates[rank].root))
	}
	if journalPresent {
		for rank := 0; rank < journal.Len(); rank++ {
			target, _ := journal.TargetAt(rank)
			scratchNeed = max(scratchNeed, int(target.Ref.Length))
		}
	}
	if len(pageScratch) < scratchNeed {
		return MutableInlineRecovery{}, fmt.Errorf(
			"%w: have=%d need=%d",
			ErrRecoveryBufferTooSmall, len(pageScratch), scratchNeed,
		)
	}

	selectedRank := -1
	rolledBack := false
	for rank := 0; rank < candidateCount; rank++ {
		candidate := candidates[rank]
		root := candidate.root
		if root.PageSize != pageSize || root.FileEnd > fileSize {
			continue
		}
		if journalPresent && root.Generation < journal.Header().TargetGeneration &&
			!rolledBack {
			if err := validateJournalTargetsForCandidate(
				journal, root, layout, fileSize,
			); err != nil {
				return MutableInlineRecovery{}, err
			}
			if err := preflightMaterializationRollback(
				file, journal, pageScratch,
			); err != nil {
				return MutableInlineRecovery{}, err
			}
			if err := applyMaterializationRollback(
				file, journal, pageScratch, syncFile,
			); err != nil {
				return MutableInlineRecovery{}, err
			}
			rolledBack = true
		}
		ok, validateErr := validateRecoveredInlineRefs(
			file, root, pageScratch,
		)
		if validateErr != nil {
			return MutableInlineRecovery{}, validateErr
		}
		if ok {
			selectedRank = rank
			break
		}
	}
	if selectedRank < 0 {
		return MutableInlineRecovery{}, ErrSuperblockNotFound
	}

	selected := candidates[selectedRank]
	fallbackGeneration := selected.root.Generation
	for rank := selectedRank + 1; rank < candidateCount; rank++ {
		candidate := candidates[rank]
		root := candidate.root
		if root.PageSize != pageSize || root.FileEnd > fileSize {
			continue
		}
		// A committed selected root needs the after-images. Rolling the file
		// back merely to validate its older physical alternate would destroy
		// the selected root. Treat the selected generation itself as the
		// conservative one-root recovery fence.
		if journalPresent &&
			selected.root.Generation >= journal.Header().TargetGeneration &&
			root.Generation < journal.Header().TargetGeneration {
			continue
		}
		ok, validateErr := validateRecoveredInlineRefs(
			file, root, pageScratch,
		)
		if validateErr != nil {
			return MutableInlineRecovery{}, validateErr
		}
		if ok {
			fallbackGeneration = root.Generation
			break
		}
	}

	result := MutableInlineRecovery{
		Root: selected.root, State: selected.root.State,
		RootSlot: selected.slot, FallbackGeneration: fallbackGeneration,
		JournalSlot: -1,
	}
	if journalPresent {
		result.JournalSlot = journalSlot
		result.JournalSequence = journal.Header().Sequence
	}
	return result, nil
}

func readRecoveryRecord(file *os.File, dst []byte, offset int64) error {
	clear(dst)
	n, err := file.ReadAt(dst, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if n < len(dst) {
		clear(dst[n:])
	}
	return nil
}

func selectRecoveryMaterializationJournal(
	first, second []byte,
) (MaterializationJournalView, int, bool, error) {
	journal, slot, err := SelectMaterializationJournal(first, second)
	if errors.Is(err, ErrMaterializationJournalNotFound) {
		return MaterializationJournalView{}, -1, false, nil
	}
	if err != nil {
		return MaterializationJournalView{}, -1, false, err
	}
	return journal, slot, true, nil
}

func inlineRecoveryScratchNeed(root InlineSuperblock) int {
	need := int(root.PageSize)
	refs := [...]PageRef{
		root.State.ChunkDirectory,
		root.State.KeyDirectory,
		root.State.IndexDirectory,
		root.State.Float64ScanHead,
		root.State.IndexGroupHead,
		root.FreeDelta.IndexHead(),
		root.FreeDelta.ExternalPrev(),
	}
	for _, ref := range refs {
		need = max(need, int(ref.Length))
	}
	return need
}

func validateJournalTargetsForCandidate(
	journal MaterializationJournalView,
	root InlineSuperblock,
	layout MutableStoreFileLayout,
	fileSize uint64,
) error {
	for rank := 0; rank < journal.Len(); rank++ {
		target, _ := journal.TargetAt(rank)
		ref := target.Ref
		end := ref.Offset + uint64(ref.Length)
		if target.StoreID != root.StoreID ||
			ref.Offset < layout.DataStart ||
			end < ref.Offset ||
			end > root.FileEnd ||
			end > fileSize {
			return fmt.Errorf(
				"%w: materialization target outside fallback root",
				ErrMaterializationJournalConflict,
			)
		}
	}
	return nil
}

// preflightMaterializationRollback proves every target is recoverable before
// the first repair write. It prevents deterministic corruption in a later
// target from leaving an earlier target partially repaired.
func preflightMaterializationRollback(
	file *os.File,
	journal MaterializationJournalView,
	pageScratch []byte,
) error {
	for rank := 0; rank < journal.Len(); rank++ {
		target, _ := journal.TargetAt(rank)
		page := pageScratch[:int(target.Ref.Length)]
		ok, err := readRecoveryPage(file, target.Ref, page)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf(
				"%w: short materialization target", ErrMaterializationTargetDiverged,
			)
		}
		if _, err := journal.Rollback(rank, page); err != nil {
			return err
		}
	}
	return nil
}

func applyMaterializationRollback(
	file *os.File,
	journal MaterializationJournalView,
	pageScratch []byte,
	syncFile func() error,
) error {
	wrote := false
	for rank := 0; rank < journal.Len(); rank++ {
		target, _ := journal.TargetAt(rank)
		page := pageScratch[:int(target.Ref.Length)]
		ok, err := readRecoveryPage(file, target.Ref, page)
		if err != nil {
			return syncMaterializationRepairOnError(syncFile, wrote, err)
		}
		if !ok {
			return syncMaterializationRepairOnError(
				syncFile, wrote,
				fmt.Errorf(
					"%w: short materialization target",
					ErrMaterializationTargetDiverged,
				),
			)
		}
		result, err := journal.Rollback(rank, page)
		if err != nil {
			return syncMaterializationRepairOnError(syncFile, wrote, err)
		}
		if result != MaterializationRollbackApplied {
			continue
		}
		first, count, _ := journal.TargetPatchRange(rank)
		for patchRank := first; patchRank < first+count; patchRank++ {
			patch, _ := journal.PatchAt(patchRank)
			src := page[patch.Offset:uint32(uint64(patch.Offset)+uint64(len(patch.Data)))]
			n, writeErr := file.WriteAt(
				src, int64(target.Ref.Offset+uint64(patch.Offset)),
			)
			if n != 0 {
				wrote = true
			}
			if writeErr == nil && n != len(src) {
				writeErr = io.ErrShortWrite
			}
			if writeErr != nil {
				return syncMaterializationRepairOnError(syncFile, wrote, writeErr)
			}
		}
	}
	// Even an already-restored image may come from a previous process recovery
	// that stopped after its writes and before its barrier.
	return syncFile()
}

func syncMaterializationRepairOnError(
	syncFile func() error,
	wrote bool,
	primary error,
) error {
	if !wrote {
		return primary
	}
	return errors.Join(primary, syncFile())
}

func readRecoveryPage(
	file *os.File,
	ref PageRef,
	dst []byte,
) (bool, error) {
	n, err := file.ReadAt(dst, int64(ref.Offset))
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return n == len(dst), nil
}

func validateRecoveredInlineRefs(
	file *os.File,
	root InlineSuperblock,
	scratch []byte,
) (bool, error) {
	stateRefs := [...]PageRef{
		root.State.ChunkDirectory,
		root.State.KeyDirectory,
		root.State.IndexDirectory,
		root.State.Float64ScanHead,
		root.State.IndexGroupHead,
	}
	for _, ref := range stateRefs {
		if ref == (PageRef{}) {
			continue
		}
		ok, err := validateRecoveredStateRef(file, root, ref, scratch)
		if err != nil || !ok {
			return ok, err
		}
	}

	indexHead := root.FreeDelta.IndexHead()
	if indexHead != (PageRef{}) {
		page := scratch[:int(indexHead.Length)]
		ok, err := readRecoveryPage(file, indexHead, page)
		if err != nil || !ok {
			return ok, err
		}
		index, openErr := OpenFreeIndexPage(
			page, root.FileEnd, root.State.NextLogicalID,
		)
		header := index.Header()
		if openErr != nil ||
			!recoveryFreeHeaderMatchesRef(header, root.StoreID, indexHead) ||
			header.PageSize != root.PageSize ||
			header.Generation > root.Generation {
			return false, nil
		}
	}

	externalPrev := root.FreeDelta.ExternalPrev()
	if externalPrev != (PageRef{}) {
		page := scratch[:int(externalPrev.Length)]
		ok, err := readRecoveryPage(file, externalPrev, page)
		if err != nil || !ok {
			return ok, err
		}
		delta, openErr := OpenFreeDeltaPage(
			page, root.FileEnd, root.State.NextLogicalID,
		)
		header := delta.Header()
		if openErr != nil ||
			!recoveryFreeHeaderMatchesRef(header, root.StoreID, externalPrev) ||
			header.PageSize != root.PageSize ||
			header.Generation > root.Generation ||
			delta.IndexHead() != indexHead {
			return false, nil
		}
	}
	return true, nil
}

func validateRecoveredStateRef(
	file *os.File,
	root InlineSuperblock,
	ref PageRef,
	scratch []byte,
) (bool, error) {
	page := scratch[:int(ref.Length)]
	ok, err := readRecoveryPage(file, ref, page)
	if err != nil || !ok {
		return ok, err
	}
	header, _, openErr := OpenPage(page)
	if openErr != nil ||
		!recoveryPageHeaderMatchesRef(header, root.StoreID, ref) {
		return false, nil
	}

	state := root.State
	switch ref.Kind {
	case PageChunkDirectory:
		_, openErr = OpenChunkDirectoryPage(
			page, root.FileEnd, state.NextLogicalID,
		)
	case PageKeyDirectory:
		_, openErr = OpenKeyDirectoryPage(
			page, root.FileEnd, state.NextLogicalID,
			state.ChunkHighWater, uint8(state.ChunkDocuments),
		)
	case PageFingerprintDirectory:
		_, openErr = OpenPageFingerprintDirectory(
			page, root.FileEnd, state.NextLogicalID,
			state.ChunkHighWater, state.ChunkDocuments,
		)
	case PageIndexDirectory:
		_, openErr = OpenIndexDirectoryPage(
			page, root.FileEnd, state.NextLogicalID, state.IndexCount,
		)
	case PageFloat64Catalog:
		_, openErr = OpenFloat64Directory(
			page, root.FileEnd, state.NextLogicalID, root.PageSize,
		)
	case PageIndexGroupCatalog:
		_, openErr = OpenIndexGroupCatalog(
			page, state.IndexCount, state.ChunkHighWater,
			state.ChunkDocuments, root.FileEnd, state.NextLogicalID,
			root.PageSize,
		)
	default:
		return false, nil
	}
	return openErr == nil, nil
}

func recoveryPageHeaderMatchesRef(
	header PageHeader,
	storeID [16]byte,
	ref PageRef,
) bool {
	return header.StoreID == storeID &&
		header.LogicalID == ref.LogicalID &&
		header.Generation == ref.Generation &&
		header.PageSize == ref.Length &&
		header.Kind == ref.Kind &&
		header.Flags == ref.Flags
}

func recoveryFreeHeaderMatchesRef(
	header FreeLogHeader,
	storeID [16]byte,
	ref PageRef,
) bool {
	return header.StoreID == storeID &&
		header.LogicalID == ref.LogicalID &&
		header.Generation == ref.Generation
}
