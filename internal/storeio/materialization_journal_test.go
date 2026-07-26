package storeio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

type materializationJournalFixture struct {
	header  MaterializationJournalHeader
	targets []MaterializationTarget
	patches []MaterializationPatch
	before  [][]byte
	after   [][]byte
}

func newMaterializationJournalFixture(tb testing.TB, sequence uint64) materializationJournalFixture {
	tb.Helper()
	storeID := [16]byte{0x91, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	header := MaterializationJournalHeader{
		StoreID: storeID, Sequence: sequence, TargetGeneration: 20 + sequence,
		PageSize: 4096, SectorSize: 512,
	}
	fixture := materializationJournalFixture{
		header: header,
		before: make([][]byte, 2),
		after:  make([][]byte, 2),
	}
	refs := [2]PageRef{
		{Offset: 4 * 4096, LogicalID: 7, Generation: 11, Length: 4096, Kind: PageDocument},
		{Offset: 5 * 4096, LogicalID: 8, Generation: 12, Length: 4096, Kind: PageDocument},
	}
	for rank, ref := range refs {
		before := makeMaterializationTestPage(tb, storeID, ref, byte(0x30+rank))
		after := append([]byte(nil), before...)
		changedAt := PageHeaderSize + 11 + rank*9
		for i := range 7 {
			after[changedAt+i] ^= byte(0x51 + i + rank)
		}
		if _, err := SealPage(after); err != nil {
			tb.Fatal(err)
		}
		fixture.before[rank] = before
		fixture.after[rank] = after
		changedSector := changedAt / int(header.SectorSize) * int(header.SectorSize)
		trailerSector := 4096 - int(header.SectorSize)
		fixture.patches = append(fixture.patches,
			MaterializationPatch{
				Target: uint16(rank), Offset: uint32(changedSector),
				Data: before[changedSector : changedSector+int(header.SectorSize)],
			},
			MaterializationPatch{
				Target: uint16(rank), Offset: uint32(trailerSector),
				Data: before[trailerSector:],
			},
		)
	}
	for rank, ref := range refs {
		target, err := BuildMaterializationTarget(
			header, ref, fixture.before[rank], fixture.after[rank], fixture.patches, uint16(rank),
		)
		if err != nil {
			tb.Fatalf("BuildMaterializationTarget(%d): %v", rank, err)
		}
		fixture.targets = append(fixture.targets, target)
	}
	return fixture
}

func makeMaterializationTestPage(
	tb testing.TB,
	storeID [16]byte,
	ref PageRef,
	fill byte,
) []byte {
	tb.Helper()
	page := make([]byte, ref.Length)
	payload, err := InitPage(page, PageHeader{
		StoreID: storeID, Generation: ref.Generation, LogicalID: ref.LogicalID,
		PageSize: ref.Length, PayloadLength: 128, Kind: ref.Kind, Flags: ref.Flags,
	})
	if err != nil {
		tb.Fatal(err)
	}
	for i := range payload {
		payload[i] = fill + byte(i%17)
	}
	if _, err := SealPage(page); err != nil {
		tb.Fatal(err)
	}
	return page
}

func encodeMaterializationFixture(tb testing.TB, fixture materializationJournalFixture) []byte {
	tb.Helper()
	slot := make([]byte, MaterializationJournalSize)
	encoded, err := EncodeMaterializationJournal(slot, fixture.header, fixture.targets, fixture.patches)
	if err != nil {
		tb.Fatal(err)
	}
	return encoded
}

func TestMaterializationJournalRoundTripAndCanonicalLayout(t *testing.T) {
	fixture := newMaterializationJournalFixture(t, 41)
	encoded := encodeMaterializationFixture(t, fixture)
	view, err := OpenMaterializationJournal(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if view.Header() != fixture.header || view.Len() != len(fixture.targets) ||
		view.PatchLen() != len(fixture.patches) || view.DataLen() != 2048 {
		t.Fatalf("view=(%+v targets=%d patches=%d data=%d)",
			view.Header(), view.Len(), view.PatchLen(), view.DataLen())
	}
	for rank, want := range fixture.targets {
		got, ok := view.TargetAt(rank)
		if !ok || got != want {
			t.Fatalf("target %d=(%+v,%v), want %+v", rank, got, ok, want)
		}
	}
	for rank, want := range fixture.patches {
		got, ok := view.PatchAt(rank)
		if !ok || got.Target != want.Target || got.Offset != want.Offset ||
			!bytes.Equal(got.Data, want.Data) {
			t.Fatalf("patch %d=(%+v,%v), want %+v", rank, got, ok, want)
		}
	}
	if _, ok := view.TargetAt(-1); ok {
		t.Fatal("negative target rank accepted")
	}
	if _, ok := view.TargetAt(view.Len()); ok {
		t.Fatal("past-end target rank accepted")
	}
	if _, ok := view.PatchAt(-1); ok {
		t.Fatal("negative patch rank accepted")
	}
	if _, ok := view.PatchAt(view.PatchLen()); ok {
		t.Fatal("past-end patch rank accepted")
	}

	second := make([]byte, MaterializationJournalSize)
	if _, err := EncodeMaterializationJournal(second, fixture.header, fixture.targets, fixture.patches); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, second) {
		t.Fatal("encoding is not deterministic")
	}
	dataEnd := int(view.dataOffset + view.dataLength)
	if !allZero(encoded[dataEnd:materializationTrailerAt]) {
		t.Fatal("capsule padding is not canonical zero")
	}
}

func TestMaterializationJournalRollbackBeforeTornAndAfterImages(t *testing.T) {
	fixture := newMaterializationJournalFixture(t, 42)
	view, err := OpenMaterializationJournal(encodeMaterializationFixture(t, fixture))
	if err != nil {
		t.Fatal(err)
	}
	for targetRank := range view.Len() {
		page := append([]byte(nil), fixture.after[targetRank]...)
		result, err := view.Rollback(targetRank, page)
		if err != nil || result != MaterializationRollbackApplied ||
			!bytes.Equal(page, fixture.before[targetRank]) {
			t.Fatalf("after target %d=(%d,%v) equal=%v", targetRank, result, err,
				bytes.Equal(page, fixture.before[targetRank]))
		}

		page = append(page[:0], fixture.before[targetRank]...)
		result, err = view.Rollback(targetRank, page)
		if err != nil || result != MaterializationAlreadyRolledBack ||
			!bytes.Equal(page, fixture.before[targetRank]) {
			t.Fatalf("before target %d=(%d,%v)", targetRank, result, err)
		}

		page = append(page[:0], fixture.before[targetRank]...)
		first, count, _ := view.TargetPatchRange(targetRank)
		for rank := first; rank < first+count; rank++ {
			patch, _ := view.PatchAt(rank)
			half := len(patch.Data) / 2
			copy(page[patch.Offset:], fixture.after[targetRank][patch.Offset:patch.Offset+uint32(half)])
		}
		result, err = view.Rollback(targetRank, page)
		if err != nil || result != MaterializationRollbackApplied ||
			!bytes.Equal(page, fixture.before[targetRank]) {
			t.Fatalf("torn target %d=(%d,%v) equal=%v", targetRank, result, err,
				bytes.Equal(page, fixture.before[targetRank]))
		}
	}
}

func TestMaterializationJournalRejectsDivergedTargetContext(t *testing.T) {
	fixture := newMaterializationJournalFixture(t, 43)
	view, err := OpenMaterializationJournal(encodeMaterializationFixture(t, fixture))
	if err != nil {
		t.Fatal(err)
	}
	page := append([]byte(nil), fixture.after[0]...)
	page[700] ^= 0x80 // Outside both undo sectors.
	want := append([]byte(nil), page...)
	if _, err := view.Rollback(0, page); !errors.Is(err, ErrMaterializationTargetDiverged) {
		t.Fatalf("Rollback = %v, want %v", err, ErrMaterializationTargetDiverged)
	}
	if !bytes.Equal(page, want) {
		t.Fatal("diverged page was modified before the context check")
	}
	if _, err := view.Rollback(-1, page); !errors.Is(err, ErrInvalidWrite) {
		t.Fatalf("negative Rollback = %v, want %v", err, ErrInvalidWrite)
	}
	if _, err := view.Rollback(0, page[:len(page)-1]); !errors.Is(err, ErrInvalidWrite) {
		t.Fatalf("short Rollback = %v, want %v", err, ErrInvalidWrite)
	}
}

func TestMaterializationJournalRootGenerationIsTheCommitMarker(t *testing.T) {
	fixture := newMaterializationJournalFixture(t, 47)
	view, err := OpenMaterializationJournal(encodeMaterializationFixture(t, fixture))
	if err != nil {
		t.Fatal(err)
	}
	page := append([]byte(nil), fixture.after[0]...)
	result, err := view.RecoverTarget(fixture.header.TargetGeneration-1, 0, page)
	if err != nil || result != MaterializationRollbackApplied ||
		!bytes.Equal(page, fixture.before[0]) {
		t.Fatalf("fallback root recovery=(%d,%v), restored=%v",
			result, err, bytes.Equal(page, fixture.before[0]))
	}
	page = append(page[:0], fixture.after[0]...)
	result, err = view.RecoverTarget(fixture.header.TargetGeneration, 0, page)
	if err != nil || result != MaterializationRollbackNotNeeded ||
		!bytes.Equal(page, fixture.after[0]) {
		t.Fatalf("committed root recovery=(%d,%v), unchanged=%v",
			result, err, bytes.Equal(page, fixture.after[0]))
	}
	result, err = view.RecoverTarget(fixture.header.TargetGeneration+10, -1, nil)
	if err != nil || result != MaterializationRollbackNotNeeded {
		t.Fatalf("newer root recovery=(%d,%v)", result, err)
	}
	if _, err := view.NeedsRollback(0); !errors.Is(err, ErrInvalidWrite) {
		t.Fatalf("zero root generation = %v, want %v", err, ErrInvalidWrite)
	}
}

func TestMaterializationJournalRollsBackEverySectorPrefixTear(t *testing.T) {
	fixture := newMaterializationJournalFixture(t, 48)
	view, err := OpenMaterializationJournal(encodeMaterializationFixture(t, fixture))
	if err != nil {
		t.Fatal(err)
	}
	first, count, _ := view.TargetPatchRange(0)
	for tornRank := first; tornRank < first+count; tornRank++ {
		tornPatch, _ := view.PatchAt(tornRank)
		for cut := 0; cut <= len(tornPatch.Data); cut++ {
			page := append([]byte(nil), fixture.before[0]...)
			for rank := first; rank < tornRank; rank++ {
				patch, _ := view.PatchAt(rank)
				copy(page[patch.Offset:], fixture.after[0][patch.Offset:patch.Offset+uint32(len(patch.Data))])
			}
			copy(page[tornPatch.Offset:],
				fixture.after[0][tornPatch.Offset:tornPatch.Offset+uint32(cut)])
			if _, err := view.Rollback(0, page); err != nil {
				t.Fatalf("sector patch %d cut %d: %v", tornRank-first, cut, err)
			}
			if !bytes.Equal(page, fixture.before[0]) {
				t.Fatalf("sector patch %d cut %d did not restore before image", tornRank-first, cut)
			}
		}
	}
}

func TestMaterializationJournalSelectSurvivesEveryPrefixTear(t *testing.T) {
	oldFixture := newMaterializationJournalFixture(t, 100)
	newFixture := newMaterializationJournalFixture(t, 101)
	oldSlot := encodeMaterializationFixture(t, oldFixture)
	newSlot := encodeMaterializationFixture(t, newFixture)
	empty := make([]byte, MaterializationJournalSize)

	for cut := 0; cut <= MaterializationJournalSize; cut++ {
		torn := make([]byte, MaterializationJournalSize)
		copy(torn, newSlot[:cut])
		view, slot, err := SelectMaterializationJournal(oldSlot, torn)
		if cut < MaterializationJournalSize {
			if err != nil || slot != 0 || view.Header().Sequence != oldFixture.header.Sequence {
				t.Fatalf("cut %d selected=(slot=%d seq=%d err=%v), want old",
					cut, slot, view.Header().Sequence, err)
			}
			if _, _, err := SelectMaterializationJournal(empty, torn); !errors.Is(err, ErrMaterializationJournalNotFound) {
				t.Fatalf("first-use cut %d = %v, want %v",
					cut, err, ErrMaterializationJournalNotFound)
			}
			continue
		}
		if err != nil || slot != 1 || view.Header().Sequence != newFixture.header.Sequence {
			t.Fatalf("complete selected=(slot=%d seq=%d err=%v), want new",
				slot, view.Header().Sequence, err)
		}
		view, slot, err = SelectMaterializationJournal(empty, torn)
		if err != nil || slot != 1 || view.Header().Sequence != newFixture.header.Sequence {
			t.Fatalf("first complete selected=(slot=%d seq=%d err=%v)",
				slot, view.Header().Sequence, err)
		}
	}
}

func TestMaterializationJournalRejectsEverySingleBitCorruptionAndTruncation(t *testing.T) {
	fixture := newMaterializationJournalFixture(t, 44)
	encoded := encodeMaterializationFixture(t, fixture)
	for at := range encoded {
		corrupt := append([]byte(nil), encoded...)
		corrupt[at] ^= 1
		if _, err := OpenMaterializationJournal(corrupt); !errors.Is(err, ErrMaterializationJournalCorrupt) {
			t.Fatalf("byte %d = %v, want %v", at, err, ErrMaterializationJournalCorrupt)
		}
	}
	for cut := 0; cut < len(encoded); cut++ {
		if _, err := OpenMaterializationJournal(encoded[:cut]); !errors.Is(err, ErrMaterializationJournalCorrupt) {
			t.Fatalf("cut %d = %v, want %v", cut, err, ErrMaterializationJournalCorrupt)
		}
	}
	if _, err := OpenMaterializationJournal(make([]byte, MaterializationJournalSize)); !errors.Is(err, ErrMaterializationJournalNotFound) {
		t.Fatalf("empty = %v, want %v", err, ErrMaterializationJournalNotFound)
	}
}

func TestMaterializationJournalRejectsRechecksummedNonCanonicalStates(t *testing.T) {
	fixture := newMaterializationJournalFixture(t, 45)
	encoded := encodeMaterializationFixture(t, fixture)
	patchOffset := int(binary.LittleEndian.Uint32(encoded[92:96]))
	dataEnd := int(binary.LittleEndian.Uint32(encoded[96:100]) +
		binary.LittleEndian.Uint32(encoded[100:104]))
	cases := []struct {
		name   string
		mutate func([]byte)
	}{
		{"commit-marker", func(p []byte) { p[16] ^= 1 }},
		{"invalid-sector-size", func(p []byte) { binary.LittleEndian.PutUint32(p[108:112], 768) }},
		{"target-reserved", func(p []byte) { p[materializationTargetOffset+48] = 1 }},
		{"target-aux", func(p []byte) { p[materializationTargetOffset+30] = 1 }},
		{"empty-target-range", func(p []byte) {
			binary.LittleEndian.PutUint16(p[materializationTargetOffset+46:], 0)
		}},
		{"wrong-patch-owner", func(p []byte) {
			binary.LittleEndian.PutUint16(p[materializationTargetOffset+46:], 1)
			second := materializationTargetOffset + MaterializationTargetRecordSize
			binary.LittleEndian.PutUint16(p[second+44:], 1)
			binary.LittleEndian.PutUint16(p[second+46:], 3)
		}},
		{"patch-reserved", func(p []byte) { p[patchOffset+12] = 1 }},
		{"patch-overlap", func(p []byte) {
			firstOffset := binary.LittleEndian.Uint32(p[patchOffset+4:])
			binary.LittleEndian.PutUint32(p[patchOffset+MaterializationPatchRecordSize+4:], firstOffset)
		}},
		{"patch-adjacency", func(p []byte) {
			firstOffset := binary.LittleEndian.Uint32(p[patchOffset+4:])
			firstLength := binary.LittleEndian.Uint16(p[patchOffset+2:])
			binary.LittleEndian.PutUint32(
				p[patchOffset+MaterializationPatchRecordSize+4:],
				firstOffset+uint32(firstLength),
			)
		}},
		{"patch-data-gap", func(p []byte) {
			binary.LittleEndian.PutUint32(p[patchOffset+MaterializationPatchRecordSize+8:], 1)
		}},
		{"layout-offset", func(p []byte) {
			binary.LittleEndian.PutUint32(p[92:96], uint32(patchOffset+8))
		}},
		{"padding", func(p []byte) { p[dataEnd] = 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			corrupt := append([]byte(nil), encoded...)
			tc.mutate(corrupt)
			resealMaterializationTestCapsule(corrupt)
			if _, err := OpenMaterializationJournal(corrupt); !errors.Is(err, ErrMaterializationJournalCorrupt) {
				t.Fatalf("OpenMaterializationJournal = %v, want %v",
					err, ErrMaterializationJournalCorrupt)
			}
		})
	}
}

func TestMaterializationJournalBuilderAndEncoderRejectInvalidInput(t *testing.T) {
	fixture := newMaterializationJournalFixture(t, 46)
	ref := fixture.targets[0].Ref
	uncovered := append([]byte(nil), fixture.patches[0].Data...)
	uncovered = uncovered[:len(uncovered)-1]
	badPatches := append([]MaterializationPatch(nil), fixture.patches...)
	badPatches[0].Data = uncovered
	if _, err := BuildMaterializationTarget(
		fixture.header, ref, fixture.before[0], fixture.after[0], badPatches, 0,
	); !errors.Is(err, ErrInvalidWrite) {
		t.Fatalf("uncovered BuildMaterializationTarget = %v, want %v", err, ErrInvalidWrite)
	}

	tests := []struct {
		name    string
		header  MaterializationJournalHeader
		targets []MaterializationTarget
		patches []MaterializationPatch
	}{
		{"zero-header", MaterializationJournalHeader{}, fixture.targets, fixture.patches},
		{"no-targets", fixture.header, nil, fixture.patches},
		{"no-patches", fixture.header, fixture.targets, nil},
		{"unordered-targets", fixture.header,
			[]MaterializationTarget{fixture.targets[1], fixture.targets[0]}, fixture.patches},
		{"missing-target-patches", fixture.header, fixture.targets, fixture.patches[:2]},
		{"patch-past-target", fixture.header, fixture.targets, []MaterializationPatch{
			{Target: 0, Offset: ref.Length - 1, Data: []byte{1, 2}},
			fixture.patches[1], fixture.patches[2], fixture.patches[3],
		}},
		{"patch-order", fixture.header, fixture.targets, []MaterializationPatch{
			fixture.patches[1], fixture.patches[0], fixture.patches[2], fixture.patches[3],
		}},
		{"target-aux", fixture.header, []MaterializationTarget{
			func() MaterializationTarget {
				target := fixture.targets[0]
				target.Ref.Aux = 1
				return target
			}(),
		}, fixture.patches[:2]},
		{"wrong-target-store", fixture.header, []MaterializationTarget{
			func() MaterializationTarget {
				target := fixture.targets[0]
				target.StoreID[0] ^= 1
				return target
			}(),
		}, fixture.patches[:2]},
		{"capacity", fixture.header, fixture.targets[:1], []MaterializationPatch{
			{Target: 0, Offset: 0, Data: make([]byte, MaterializationJournalSize)},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := EncodeMaterializationJournal(
				make([]byte, MaterializationJournalSize), tc.header, tc.targets, tc.patches,
			); !errors.Is(err, ErrInvalidWrite) {
				t.Fatalf("EncodeMaterializationJournal = %v, want %v", err, ErrInvalidWrite)
			}
		})
	}
}

func TestMaterializationJournalExactUndoCapacityAndFourKiBFallback(t *testing.T) {
	fixture := newMaterializationJournalFixture(t, 49)
	header := fixture.header
	ref := fixture.targets[0].Ref
	before := make([]byte, ref.Length)
	payload, err := InitPage(before, PageHeader{
		StoreID: header.StoreID, Generation: ref.Generation, LogicalID: ref.LogicalID,
		PageSize: ref.Length, PayloadLength: 3800, Kind: ref.Kind,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range payload {
		payload[i] = byte(i*17 + 3)
	}
	if _, err := SealPage(before); err != nil {
		t.Fatal(err)
	}
	after := append([]byte(nil), before...)
	after[700] ^= 0x5a
	if _, err := SealPage(after); err != nil {
		t.Fatal(err)
	}
	patches := []MaterializationPatch{{
		Target: 0, Offset: 512, Data: before[512:],
	}}
	target, err := BuildMaterializationTarget(header, ref, before, after, patches, 0)
	if err != nil {
		t.Fatal(err)
	}
	slot := make([]byte, MaterializationJournalSize)
	if _, err := EncodeMaterializationJournal(
		slot, header, []MaterializationTarget{target}, patches,
	); err != nil {
		t.Fatalf("maximum %d-byte undo = %v", MaterializationJournalMaxData, err)
	}
	view, err := OpenMaterializationJournal(slot)
	if err != nil || view.DataLen() != MaterializationJournalMaxData {
		t.Fatalf("maximum undo view=(data=%d err=%v)", view.DataLen(), err)
	}

	sector4K := header
	sector4K.SectorSize = 4096
	fullPatch := []MaterializationPatch{{Target: 0, Offset: 0, Data: before}}
	target, err = BuildMaterializationTarget(sector4K, ref, before, after, fullPatch, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EncodeMaterializationJournal(
		slot, sector4K, []MaterializationTarget{target}, fullPatch,
	); !errors.Is(err, ErrInvalidWrite) {
		t.Fatalf("4-KiB damage granule = %v, want bounded copy-on-write fallback", err)
	}
}

func TestMaterializationJournalSelectionRejectsConflictingValidSlots(t *testing.T) {
	fixture := newMaterializationJournalFixture(t, 70)
	first := encodeMaterializationFixture(t, fixture)

	sameSequence := newMaterializationJournalFixture(t, 70)
	second := encodeMaterializationFixture(t, sameSequence)
	if _, _, err := SelectMaterializationJournal(first, second); !errors.Is(err, ErrMaterializationJournalConflict) {
		t.Fatalf("same sequence = %v, want %v", err, ErrMaterializationJournalConflict)
	}

	otherStore := newMaterializationJournalFixture(t, 71)
	otherStore.header.StoreID[0] ^= 0xff
	for rank := range otherStore.before {
		// Rebuild the images and targets so each capsule is individually valid
		// for its own Store identity.
		ref := otherStore.targets[rank].Ref
		otherStore.before[rank] = makeMaterializationTestPage(t, otherStore.header.StoreID, ref, byte(0x60+rank))
		otherStore.after[rank] = append([]byte(nil), otherStore.before[rank]...)
		changedAt := PageHeaderSize + 11 + rank*9
		for i := range 7 {
			otherStore.after[rank][changedAt+i] ^= byte(0x21 + i)
		}
		if _, err := SealPage(otherStore.after[rank]); err != nil {
			t.Fatal(err)
		}
		otherStore.patches[rank*2].Data = otherStore.before[rank][:otherStore.header.SectorSize]
		otherStore.patches[rank*2+1].Data =
			otherStore.before[rank][4096-otherStore.header.SectorSize:]
		target, err := BuildMaterializationTarget(
			otherStore.header, ref,
			otherStore.before[rank], otherStore.after[rank], otherStore.patches, uint16(rank),
		)
		if err != nil {
			t.Fatal(err)
		}
		otherStore.targets[rank] = target
	}
	second = encodeMaterializationFixture(t, otherStore)
	if _, _, err := SelectMaterializationJournal(first, second); !errors.Is(err, ErrMaterializationJournalConflict) {
		t.Fatalf("different store = %v, want %v", err, ErrMaterializationJournalConflict)
	}

	lowerGeneration := newMaterializationJournalFixture(t, 71)
	lowerGeneration.header.TargetGeneration = fixture.header.TargetGeneration - 1
	second = encodeMaterializationFixture(t, lowerGeneration)
	if _, _, err := SelectMaterializationJournal(first, second); !errors.Is(err, ErrMaterializationJournalConflict) {
		t.Fatalf("generation reversal = %v, want %v", err, ErrMaterializationJournalConflict)
	}
	sameGeneration := newMaterializationJournalFixture(t, 71)
	sameGeneration.header.TargetGeneration = fixture.header.TargetGeneration
	second = encodeMaterializationFixture(t, sameGeneration)
	if _, _, err := SelectMaterializationJournal(first, second); !errors.Is(err, ErrMaterializationJournalConflict) {
		t.Fatalf("generation reuse = %v, want %v", err, ErrMaterializationJournalConflict)
	}
}

func TestMaterializationJournalHotPathsAllocateNothing(t *testing.T) {
	fixture := newMaterializationJournalFixture(t, 80)
	slot := make([]byte, MaterializationJournalSize)
	if allocs := testing.AllocsPerRun(1000, func() {
		if _, err := EncodeMaterializationJournal(slot, fixture.header, fixture.targets, fixture.patches); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("EncodeMaterializationJournal allocations = %f", allocs)
	}
	encoded := encodeMaterializationFixture(t, fixture)
	var view MaterializationJournalView
	if allocs := testing.AllocsPerRun(1000, func() {
		var err error
		view, err = OpenMaterializationJournal(encoded)
		if err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("OpenMaterializationJournal allocations = %f", allocs)
	}
	page := append([]byte(nil), fixture.after[0]...)
	if allocs := testing.AllocsPerRun(1000, func() {
		if _, err := view.Rollback(0, page); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("MaterializationJournalView.Rollback allocations = %f", allocs)
	}
}

func resealMaterializationTestCapsule(slot []byte) {
	checksum := PageChecksum(slot[:materializationTrailerAt])
	binary.LittleEndian.PutUint32(slot[materializationTrailerAt:materializationTrailerAt+4], checksum)
	binary.LittleEndian.PutUint32(slot[materializationTrailerAt+4:], ^checksum)
}
