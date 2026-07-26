package storeio

import (
	"bytes"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

type materializationDeviceCall struct {
	pages    []Write
	root     Write
	rootData []byte
}

type materializationRecordingDevice struct {
	buffers    [][]byte
	bufferSize int
	seen       []uint64
	failAt     int
	failErr    error

	mu    sync.Mutex
	calls []materializationDeviceCall
}

func newMaterializationRecordingDevice(bufferCount, bufferSize int) *materializationRecordingDevice {
	buffers := make([][]byte, bufferCount)
	for rank := range buffers {
		buffers[rank] = make([]byte, bufferSize)
	}
	return &materializationRecordingDevice{
		buffers: buffers, bufferSize: bufferSize,
		seen:   make([]uint64, (bufferCount+63)/64),
		failAt: -1,
	}
}

func (*materializationRecordingDevice) Backend() Backend { return BackendPortable }

func (d *materializationRecordingDevice) Buffer(index int) ([]byte, error) {
	if index < 0 || index >= len(d.buffers) {
		return nil, ErrInvalidWrite
	}
	return d.buffers[index], nil
}

func (d *materializationRecordingDevice) Commit(pages []Write, root Write) error {
	if err := validateCommit(len(d.buffers), d.bufferSize, d.seen, pages, root); err != nil {
		return err
	}
	d.mu.Lock()
	call := len(d.calls)
	d.calls = append(d.calls, materializationDeviceCall{
		pages: append([]Write(nil), pages...),
		root:  root,
		rootData: append(
			[]byte(nil), d.buffers[root.Buffer][:root.Length]...,
		),
	})
	d.mu.Unlock()
	if call == d.failAt {
		return d.failErr
	}
	return nil
}

func (*materializationRecordingDevice) Close() error { return nil }

func (d *materializationRecordingDevice) snapshot() []materializationDeviceCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]materializationDeviceCall, len(d.calls))
	copy(out, d.calls)
	return out
}

type committerMaterializationFixture struct {
	header  MaterializationJournalHeader
	ref     PageRef
	target  MaterializationTarget
	patches []MaterializationPatch
	after   []byte
}

func newCommitterMaterializationFixture(
	t *testing.T,
	pageSize uint32,
	generation, sequence uint64,
) committerMaterializationFixture {
	t.Helper()
	header := MaterializationJournalHeader{
		StoreID: testStoreID, Sequence: sequence,
		TargetGeneration: generation, PageSize: pageSize, SectorSize: 512,
	}
	ref := PageRef{
		Offset: 8 * uint64(pageSize), LogicalID: 7, Generation: 1,
		Length: pageSize, Kind: PageDocument,
	}
	before := make([]byte, pageSize)
	payload, err := InitPage(before, PageHeader{
		StoreID: testStoreID, Generation: ref.Generation,
		LogicalID: ref.LogicalID, PageSize: pageSize,
		PayloadLength: 128, Kind: ref.Kind,
	})
	if err != nil {
		t.Fatal(err)
	}
	for rank := range payload {
		payload[rank] = byte(rank*17 + 3)
	}
	if _, err := SealPage(before); err != nil {
		t.Fatal(err)
	}
	after := append([]byte(nil), before...)
	after[PageHeaderSize+11] ^= 0x5a
	if _, err := SealPage(after); err != nil {
		t.Fatal(err)
	}
	sector := int(header.SectorSize)
	patches := []MaterializationPatch{
		{Target: 0, Offset: 0, Data: before[:sector]},
		{
			Target: 0, Offset: pageSize - header.SectorSize,
			Data: before[len(before)-sector:],
		},
	}
	target, err := BuildMaterializationTarget(
		header, ref, before, after, patches, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	return committerMaterializationFixture{
		header: header, ref: ref, target: target, patches: patches, after: after,
	}
}

func publishCommitterMaterialization(
	t *testing.T,
	committer *Committer,
	generation, sequence uint64,
) committerMaterializationFixture {
	t.Helper()
	batch, fixture := prepareCommitterMaterialization(
		t, committer, generation, sequence,
	)
	if err := batch.StageMaterializationTarget(0, fixture.after); err != nil {
		t.Fatal(err)
	}
	if err := batch.Publish(generation); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func prepareCommitterMaterialization(
	t *testing.T,
	committer *Committer,
	generation, sequence uint64,
) (*Batch, committerMaterializationFixture) {
	t.Helper()
	fixture := newCommitterMaterializationFixture(
		t, uint32(committer.bufferSize), generation, sequence,
	)
	batch, err := committer.BeginMaterialized(len(fixture.patches))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := batch.MaterializationJournalBuffer()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EncodeMaterializationJournal(
		journal, fixture.header, []MaterializationTarget{fixture.target}, fixture.patches,
	); err != nil {
		t.Fatal(err)
	}
	if err := batch.SealMaterializationJournal(); err != nil {
		t.Fatal(err)
	}
	root := InlineSuperblock{
		StoreID: testStoreID, Generation: generation,
		FileEnd:  fixture.ref.Offset + uint64(fixture.ref.Length),
		PageSize: uint32(committer.bufferSize),
		State: StateRoot{
			StoreID: testStoreID, Generation: generation,
			PageSize: uint32(committer.bufferSize), NextLogicalID: 8,
			ChunkDocuments: 64,
		},
	}
	if err := batch.SetInlineSuperblock(root); err != nil {
		t.Fatal(err)
	}
	return batch, fixture
}

func newMaterializationTestCommitter(
	t *testing.T,
	device *materializationRecordingDevice,
	options CommitterOptions,
) *Committer {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "materialization-committer")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if options.MaterializationDamageGranule == 0 {
		options.MaterializationDamageGranule = MaterializationJournalMinSectorSize
	}
	committer, err := newCommitter(
		file,
		DeviceOptions{
			Backend: BackendPortable, BufferCount: len(device.buffers),
			BufferSize: device.bufferSize,
		},
		options,
		func(*os.File, DeviceOptions) (Device, error) { return device, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	return committer
}

func TestCommitterMaterializationUsesJournalThenSparseTargetsAndInlineRoot(t *testing.T) {
	pageSize := max(os.Getpagesize(), InlineSuperblockSize)
	device := newMaterializationRecordingDevice(8, pageSize)
	committer := newMaterializationTestCommitter(t, device, CommitterOptions{
		QueueSlots: 4, MaxPagesPerBatch: 4, GroupLimit: 4,
	})
	fixture := publishCommitterMaterialization(t, committer, 2, 1)
	if err := committer.Wait(2); err != nil {
		t.Fatal(err)
	}
	calls := device.snapshot()
	if len(calls) != 2 {
		t.Fatalf("Device.Commit calls = %d, want journal and data/root", len(calls))
	}
	layout, err := MutableStoreLayout(uint32(pageSize))
	if err != nil {
		t.Fatal(err)
	}
	if len(calls[0].pages) != 0 ||
		calls[0].root.Offset != int64(layout.MaterializationJournalOffsets[0]) ||
		!bytes.Equal(calls[0].rootData[:8], []byte(materializationJournalMagic)) {
		t.Fatalf("journal phase = %+v", calls[0])
	}
	if len(calls[1].pages) != len(fixture.patches) ||
		calls[1].root.Offset != 0 ||
		!bytes.Equal(calls[1].rootData[:8], []byte(inlineSuperblockMagic)) {
		t.Fatalf("target/root phase = %+v", calls[1])
	}
	var targetBytes uint64
	for rank, patch := range fixture.patches {
		write := calls[1].pages[rank]
		wantOffset := int64(fixture.ref.Offset + uint64(patch.Offset))
		if write.Offset != wantOffset || int(write.Length) != len(patch.Data) {
			t.Fatalf("sparse target %d = %+v", rank, write)
		}
		targetBytes += uint64(write.Length)
	}
	stats := committer.Stats()
	if stats.DeviceCommits != 2 || stats.CommittedBatches != 1 ||
		stats.MaterializedBatches != 1 ||
		stats.MaterializationJournalBytes != MaterializationJournalSize ||
		stats.MaterializationTargetBytes != targetBytes ||
		stats.DeviceBytes != MaterializationJournalSize+targetBytes+uint64(pageSize) ||
		stats.LargestGroup != 1 {
		t.Fatalf("materialization stats = %+v", stats)
	}
	if err := committer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCommitterMaterializationProvesCompleteAfterImageBeforeSparseStaging(t *testing.T) {
	pageSize := max(os.Getpagesize(), InlineSuperblockSize)
	device := newMaterializationRecordingDevice(8, pageSize)
	committer := newMaterializationTestCommitter(t, device, CommitterOptions{
		QueueSlots: 4, MaxPagesPerBatch: 4, GroupLimit: 4,
	})
	batch, fixture := prepareCommitterMaterialization(t, committer, 2, 1)
	corrupt := append([]byte(nil), fixture.after...)
	corrupt[PageHeaderSize+11] ^= 1
	if err := batch.StageMaterializationTarget(0, corrupt); !errors.Is(err, ErrInvalidWrite) {
		t.Fatalf("StageMaterializationTarget(corrupt) = %v, want %v", err, ErrInvalidWrite)
	}
	if err := batch.StageMaterializationTarget(0, fixture.after); err != nil {
		t.Fatal(err)
	}
	if _, err := batch.PageBuffer(0); !errors.Is(err, ErrBatchState) {
		t.Fatalf("materialized PageBuffer = %v, want private sparse buffers", err)
	}
	if err := batch.SetPage(0, int64(fixture.ref.Offset), 512); !errors.Is(err, ErrBatchState) {
		t.Fatalf("materialized SetPage = %v, want private sparse descriptors", err)
	}

	// Simulate a stale retained internal buffer. Publish rechecks the checksum
	// captured while extracting the exact after-image sector and must reject it.
	sparse := committer.buffers[batch.pages[0].Buffer]
	sparse[0] ^= 1
	if err := batch.Publish(2); !errors.Is(err, ErrInvalidWrite) {
		t.Fatalf("Publish(stale sparse sector) = %v, want %v", err, ErrInvalidWrite)
	}
	if err := batch.StageMaterializationTarget(0, fixture.after); err != nil {
		t.Fatal(err)
	}
	if err := batch.Publish(2); err != nil {
		t.Fatal(err)
	}
	if err := committer.Wait(2); err != nil {
		t.Fatal(err)
	}
	if err := committer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCommitterMaterializationJournalSlotAlternatesIndependently(t *testing.T) {
	// Exercise packed journal slots: with an 8-KiB Store page the second 4-KiB
	// capsule is not at the next page boundary.
	pageSize := max(os.Getpagesize(), 2*InlineSuperblockSize)
	device := newMaterializationRecordingDevice(8, pageSize)
	committer := newMaterializationTestCommitter(t, device, CommitterOptions{
		QueueSlots: 4, MaxPagesPerBatch: 4, GroupLimit: 4,
	})
	publishCommitterMaterialization(t, committer, 2, 1)
	if err := committer.Wait(2); err != nil {
		t.Fatal(err)
	}
	publishOrdinaryInlineRoot(t, committer, 3)
	if err := committer.Wait(3); err != nil {
		t.Fatal(err)
	}
	publishCommitterMaterialization(t, committer, 4, 2)
	if err := committer.Wait(4); err != nil {
		t.Fatal(err)
	}
	calls := device.snapshot()
	if len(calls) != 5 {
		t.Fatalf("Device.Commit calls = %d, want 5", len(calls))
	}
	layout, err := MutableStoreLayout(uint32(pageSize))
	if err != nil {
		t.Fatal(err)
	}
	if calls[0].root.Offset != int64(layout.MaterializationJournalOffsets[0]) ||
		calls[2].root.Offset != int64(pageSize) ||
		calls[3].root.Offset != int64(layout.MaterializationJournalOffsets[1]) ||
		calls[4].root.Offset != 0 {
		t.Fatalf(
			"journal/root offsets = [%d %d %d %d], want [%d %d %d %d]",
			calls[0].root.Offset, calls[2].root.Offset,
			calls[3].root.Offset, calls[4].root.Offset,
			layout.MaterializationJournalOffsets[0], pageSize,
			layout.MaterializationJournalOffsets[1], 0,
		)
	}
	if got := committer.FallbackGeneration(); got != 4 {
		t.Fatalf("materialized fallback generation = %d, want 4", got)
	}
	if err := committer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCommitterMaterializationRecoverySeedsSequenceAndOppositeSlot(t *testing.T) {
	pageSize := max(os.Getpagesize(), InlineSuperblockSize)
	device := newMaterializationRecordingDevice(8, pageSize)
	committer := newMaterializationTestCommitter(t, device, CommitterOptions{
		QueueSlots: 4, MaxPagesPerBatch: 4, GroupLimit: 4,
	})
	if err := committer.InitializeGenerationAt(10, 0); err != nil {
		t.Fatal(err)
	}
	if err := committer.InitializeMaterializationRecovery(40, 1); err != nil {
		t.Fatal(err)
	}
	if err := committer.InitializeMaterializationRecovery(39, 0); !errors.Is(err, ErrGenerationOrder) {
		t.Fatalf("second InitializeMaterializationRecovery = %v, want %v", err, ErrGenerationOrder)
	}
	wrong, wrongFixture := prepareCommitterMaterialization(t, committer, 11, 42)
	if err := wrong.StageMaterializationTarget(0, wrongFixture.after); err != nil {
		t.Fatal(err)
	}
	if err := wrong.Publish(11); !errors.Is(err, ErrInvalidWrite) {
		t.Fatalf("out-of-order capsule Publish = %v, want %v", err, ErrInvalidWrite)
	}
	if err := wrong.Abort(); err != nil {
		t.Fatal(err)
	}
	publishCommitterMaterialization(t, committer, 11, 41)
	if err := committer.Wait(11); err != nil {
		t.Fatal(err)
	}
	publishCommitterMaterialization(t, committer, 12, 42)
	if err := committer.Wait(12); err != nil {
		t.Fatal(err)
	}
	if err := committer.InitializeMaterializationRecovery(42, 1); !errors.Is(err, ErrGenerationOrder) {
		t.Fatalf("late InitializeMaterializationRecovery = %v, want %v", err, ErrGenerationOrder)
	}
	layout, err := MutableStoreLayout(uint32(pageSize))
	if err != nil {
		t.Fatal(err)
	}
	calls := device.snapshot()
	if len(calls) != 4 ||
		calls[0].root.Offset != int64(layout.MaterializationJournalOffsets[0]) ||
		calls[1].root.Offset != int64(pageSize) ||
		calls[2].root.Offset != int64(layout.MaterializationJournalOffsets[1]) ||
		calls[3].root.Offset != 0 {
		t.Fatalf("recovery-seeded calls = %+v", calls)
	}
	if got := committer.FallbackGeneration(); got != 12 {
		t.Fatalf("fallback generation = %d, want selected generation 12", got)
	}
	if err := committer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCommitterMaterializationFailuresAreStickyAndReleaseEveryBuffer(t *testing.T) {
	for _, failAt := range []int{0, 1} {
		t.Run([]string{"journal", "targets-root"}[failAt], func(t *testing.T) {
			pageSize := max(os.Getpagesize(), InlineSuperblockSize)
			device := newMaterializationRecordingDevice(8, pageSize)
			persistErr := errors.New("materialization persistence failed")
			device.failAt = failAt
			device.failErr = persistErr
			committer := newMaterializationTestCommitter(t, device, CommitterOptions{
				QueueSlots: 4, MaxPagesPerBatch: 4, GroupLimit: 4,
			})
			publishCommitterMaterialization(t, committer, 2, 1)
			if err := committer.Wait(2); !errors.Is(err, persistErr) {
				t.Fatalf("Wait = %v, want %v", err, persistErr)
			}
			stats := committer.Stats()
			wantSuccessfulCalls := uint64(failAt)
			if stats.DeviceCommits != wantSuccessfulCalls ||
				stats.CommittedBatches != 0 || stats.MaterializedBatches != 0 ||
				stats.MaterializationTargetBytes != 0 {
				t.Fatalf("failure stats = %+v", stats)
			}
			wantJournalBytes := uint64(0)
			if failAt == 1 {
				wantJournalBytes = MaterializationJournalSize
			}
			if stats.MaterializationJournalBytes != wantJournalBytes ||
				stats.DeviceBytes != wantJournalBytes {
				t.Fatalf("failure byte stats = %+v", stats)
			}
			if err := committer.Close(); !errors.Is(err, persistErr) {
				t.Fatalf("Close = %v, want %v", err, persistErr)
			}
			for rank := range committer.batches {
				batch := &committer.batches[rank]
				if batch.state.Load() != batchFree {
					t.Fatalf("batch %d was not released", batch.index)
				}
			}
			for range len(device.buffers) {
				if _, ok := committer.freeBuffers.pop(); !ok {
					t.Fatal("materialization failure leaked a staging buffer")
				}
			}
			if _, ok := committer.freeBuffers.pop(); ok {
				t.Fatal("free staging pool returned a duplicate buffer")
			}
		})
	}
}

func TestCommitterMaterializationAbortReturnsJournalBuffer(t *testing.T) {
	pageSize := max(os.Getpagesize(), InlineSuperblockSize)
	device := newMaterializationRecordingDevice(3, pageSize)
	committer := newMaterializationTestCommitter(t, device, CommitterOptions{
		QueueSlots: 2, MaxPagesPerBatch: 1, GroupLimit: 1,
	})
	if _, err := committer.BeginMaterialized(2); !errors.Is(err, ErrTooManyPages) {
		t.Fatalf("oversize BeginMaterialized = %v, want %v", err, ErrTooManyPages)
	}
	batch, err := committer.BeginMaterialized(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := batch.Abort(); err != nil {
		t.Fatal(err)
	}
	acquired := make(chan *Batch, 1)
	failed := make(chan error, 1)
	go func() {
		next, beginErr := committer.BeginMaterialized(1)
		if beginErr != nil {
			failed <- beginErr
			return
		}
		acquired <- next
	}()
	select {
	case next := <-acquired:
		if err := next.Abort(); err != nil {
			t.Fatal(err)
		}
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("BeginMaterialized blocked after Abort leaked its journal buffer")
	}
	if err := committer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCommitterMaterializationRequiresQualifiedDamageGranule(t *testing.T) {
	for _, granule := range []uint32{1, 256, 768, 4096} {
		if _, err := (CommitterOptions{
			MaterializationDamageGranule: granule,
		}).normalized(4); !errors.Is(err, ErrInvalidWrite) {
			t.Fatalf("damage granule %d = %v, want %v", granule, err, ErrInvalidWrite)
		}
	}
	if _, err := (CommitterOptions{
		MaterializationDamageGranule: 512,
	}).normalized(4); err != nil {
		t.Fatalf("qualified damage granule: %v", err)
	}

	pageSize := max(os.Getpagesize(), InlineSuperblockSize)
	device := newMaterializationRecordingDevice(3, pageSize)
	file, err := os.CreateTemp(t.TempDir(), "disabled-materialization")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	committer, err := newCommitter(
		file,
		DeviceOptions{
			Backend: BackendPortable, BufferCount: 3, BufferSize: pageSize,
		},
		CommitterOptions{QueueSlots: 2, MaxPagesPerBatch: 1, GroupLimit: 1},
		func(*os.File, DeviceOptions) (Device, error) { return device, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := committer.BeginMaterialized(1); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unqualified BeginMaterialized = %v, want %v", err, ErrUnsupported)
	}
	ordinary, err := committer.Begin(1)
	if err != nil {
		t.Fatalf("ordinary Begin changed by disabled lane: %v", err)
	}
	if err := ordinary.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := committer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCommitterOrdinaryMaterializedOrdinaryIsAHardBoundary(t *testing.T) {
	pageSize := max(os.Getpagesize(), InlineSuperblockSize)
	device := newMaterializationRecordingDevice(12, pageSize)
	committer := newMaterializationTestCommitter(t, device, CommitterOptions{
		QueueSlots: 8, MaxPagesPerBatch: 4, GroupLimit: 8,
		CoalesceDelay: 50 * time.Millisecond,
	})
	publishOrdinaryInlineRoot(t, committer, 1)
	publishCommitterMaterialization(t, committer, 2, 1)
	publishOrdinaryInlineRoot(t, committer, 3)
	if err := committer.Wait(3); err != nil {
		t.Fatal(err)
	}
	calls := device.snapshot()
	if len(calls) != 4 {
		t.Fatalf("Device.Commit calls = %d, want ordinary,journal,target/root,ordinary", len(calls))
	}
	if len(calls[0].pages) != 0 ||
		!bytes.Equal(calls[0].rootData[:8], []byte(inlineSuperblockMagic)) ||
		len(calls[1].pages) != 0 ||
		!bytes.Equal(calls[1].rootData[:8], []byte(materializationJournalMagic)) ||
		len(calls[2].pages) == 0 ||
		!bytes.Equal(calls[2].rootData[:8], []byte(inlineSuperblockMagic)) ||
		len(calls[3].pages) != 0 ||
		!bytes.Equal(calls[3].rootData[:8], []byte(inlineSuperblockMagic)) {
		t.Fatalf("ordinary/materialized boundary calls = %+v", calls)
	}
	stats := committer.Stats()
	if stats.CommittedBatches != 3 || stats.MaterializedBatches != 1 ||
		stats.DeviceCommits != 4 || stats.LargestGroup != 1 {
		t.Fatalf("boundary stats = %+v", stats)
	}
	if err := committer.Close(); err != nil {
		t.Fatal(err)
	}
}

func publishOrdinaryInlineRoot(t *testing.T, committer *Committer, generation uint64) {
	t.Helper()
	batch, err := committer.Begin(0)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := MutableStoreLayout(uint32(committer.bufferSize))
	if err != nil {
		t.Fatal(err)
	}
	root := InlineSuperblock{
		StoreID: testStoreID, Generation: generation,
		FileEnd:  layout.DataStart,
		PageSize: uint32(committer.bufferSize),
		State: StateRoot{
			StoreID: testStoreID, Generation: generation,
			PageSize: uint32(committer.bufferSize), NextLogicalID: 2,
			ChunkDocuments: 64,
		},
	}
	if err := batch.SetInlineSuperblock(root); err != nil {
		t.Fatal(err)
	}
	if err := batch.Publish(generation); err != nil {
		t.Fatal(err)
	}
}
