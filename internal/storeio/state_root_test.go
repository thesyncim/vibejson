package storeio

import (
	"encoding/binary"
	"errors"
	"os"
	"testing"
)

func testStatePageRef(kind PageKind, page, logical, generation uint64) PageRef {
	return PageRef{
		Offset:     page * uint64(testSuperblockPageSize),
		LogicalID:  logical,
		Generation: generation,
		Length:     testSuperblockPageSize,
		Kind:       kind,
	}
}

func testStateRoot(generation uint64) (StateRoot, uint64) {
	root := StateRoot{
		StoreID:          testStoreID,
		Generation:       generation,
		PageSize:         testSuperblockPageSize,
		Options:          StateOptionShapeTapes | StateOptionHashKeys,
		DocumentCount:    129,
		TTLCount:         17,
		NextLogicalID:    10,
		ChunkHighWater:   4,
		LiveChunks:       3,
		ChunkDocuments:   64,
		IndexCount:       2,
		IndexMaxDepth:    1024,
		IndexCatalogHash: 0x123456789abcdef0,
		FreeChunkHint:    2,
		ChunkDirectory:   testStatePageRef(PageChunkDirectory, 3, 2, generation),
		KeyDirectory:     testStatePageRef(PageKeyDirectory, 4, 3, generation),
		IndexDirectory:   testStatePageRef(PageIndexDirectory, 5, 4, generation),
		TTLDirectory:     testStatePageRef(PageTTLDirectory, 6, 5, generation),
	}
	return root, 7 * uint64(testSuperblockPageSize)
}

func TestStateRootPageRoundTrip(t *testing.T) {
	want, fileEnd := testStateRoot(11)
	page := make([]byte, testSuperblockPageSize)
	encoded, err := EncodeStateRootPage(page, want, fileEnd)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != int(testSuperblockPageSize) {
		t.Fatalf("encoded length = %d", len(encoded))
	}
	got, err := DecodeStateRootPage(encoded, fileEnd)
	if err != nil || got != want {
		t.Fatalf("DecodeStateRootPage = (%+v,%v), want (%+v,nil)", got, err, want)
	}
	for cut := 0; cut < len(encoded); cut++ {
		if _, err := DecodeStateRootPage(encoded[:cut], fileEnd); !errors.Is(err, ErrStateRootCorrupt) {
			t.Fatalf("cut %d = %v, want %v", cut, err, ErrStateRootCorrupt)
		}
	}
}

func TestStateRootFloat64ScanHeadRoundTrip(t *testing.T) {
	want, _ := testStateRoot(11)
	want.Options |= StateOptionFloat64Columns
	want.Float64ScanHead = testStatePageRef(PageFloat64Catalog, 7, 6, want.Generation)
	fileEnd := 8 * uint64(testSuperblockPageSize)
	page := make([]byte, testSuperblockPageSize)
	encoded, err := EncodeStateRootPage(page, want, fileEnd)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeStateRootPage(encoded, fileEnd)
	if err != nil || got != want {
		t.Fatalf("float64 state root = (%+v,%v), want (%+v,nil)", got, err, want)
	}

	invalid := want
	invalid.Options &^= StateOptionFloat64Columns
	if _, err := EncodeStateRootPage(page, invalid, fileEnd); !errors.Is(err, ErrInvalidWrite) {
		t.Fatalf("scan head without columns = %v, want %v", err, ErrInvalidWrite)
	}
}

func TestStateRootSchemaOnlyCatalogRoundTrip(t *testing.T) {
	want := StateRoot{
		StoreID:          testStoreID,
		Generation:       1,
		PageSize:         testSuperblockPageSize,
		Options:          StateOptionSchema,
		NextLogicalID:    2,
		ChunkDocuments:   64,
		IndexCatalogHash: 0x6d4b3a291807f5e3,
	}
	fileEnd := 2 * uint64(testSuperblockPageSize)
	page := make([]byte, testSuperblockPageSize)
	encoded, err := EncodeStateRootPage(page, want, fileEnd)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeStateRootPage(encoded, fileEnd)
	if err != nil || got != want {
		t.Fatalf(
			"schema state root = (%+v,%v), want (%+v,nil)",
			got, err, want,
		)
	}

	invalid := want
	invalid.IndexCatalogHash = 0
	if _, err := EncodeStateRootPage(
		page, invalid, fileEnd,
	); !errors.Is(err, ErrInvalidWrite) {
		t.Fatalf("schema without identity = %v, want %v", err, ErrInvalidWrite)
	}
}

func TestStateRootIndexGroupHeadRoundTrip(t *testing.T) {
	want, _ := testStateRoot(11)
	want.IndexGroupHead = testStatePageRef(
		PageIndexGroupCatalog, 7, 6, want.Generation,
	)
	fileEnd := 8 * uint64(testSuperblockPageSize)
	page := make([]byte, testSuperblockPageSize)
	encoded, err := EncodeStateRootPage(page, want, fileEnd)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeStateRootPage(encoded, fileEnd)
	if err != nil || got != want {
		t.Fatalf("index group state root = (%+v,%v), want (%+v,nil)", got, err, want)
	}

	invalid := want
	invalid.IndexCount = 0
	invalid.IndexCatalogHash = 0
	invalid.IndexDirectory = PageRef{}
	if _, err := EncodeStateRootPage(page, invalid, fileEnd); !errors.Is(err, ErrInvalidWrite) {
		t.Fatalf("group head without index = %v, want %v", err, ErrInvalidWrite)
	}
}

func TestStateRootValidation(t *testing.T) {
	valid, fileEnd := testStateRoot(11)
	for _, test := range []struct {
		name   string
		mutate func(*StateRoot, *uint64)
	}{
		{"options", func(root *StateRoot, _ *uint64) { root.Options |= 1 << 31 }},
		{"chunk documents", func(root *StateRoot, _ *uint64) { root.ChunkDocuments = 65 }},
		{"live high water", func(root *StateRoot, _ *uint64) { root.LiveChunks = root.ChunkHighWater + 1 }},
		{"document minimum", func(root *StateRoot, _ *uint64) { root.DocumentCount = 2 }},
		{"document maximum", func(root *StateRoot, _ *uint64) { root.DocumentCount = 193 }},
		{"ttl count", func(root *StateRoot, _ *uint64) { root.TTLCount = root.DocumentCount + 1 }},
		{"free chunk hint", func(root *StateRoot, _ *uint64) { root.FreeChunkHint = root.ChunkHighWater + 1 }},
		{"next logical id", func(root *StateRoot, _ *uint64) { root.NextLogicalID = 5 }},
		{"missing chunk root", func(root *StateRoot, _ *uint64) { root.ChunkDirectory = PageRef{} }},
		{"wrong key kind", func(root *StateRoot, _ *uint64) { root.KeyDirectory.Kind = PageTTLDirectory }},
		{"future generation", func(root *StateRoot, _ *uint64) { root.IndexDirectory.Generation++ }},
		{"short ref", func(root *StateRoot, _ *uint64) { root.TTLDirectory.Length-- }},
		{"unaligned ref", func(root *StateRoot, _ *uint64) { root.ChunkDirectory.Offset++ }},
		{"outside file", func(root *StateRoot, _ *uint64) { root.ChunkDirectory.Offset = fileEnd }},
		{"duplicate logical", func(root *StateRoot, _ *uint64) { root.KeyDirectory.LogicalID = root.ChunkDirectory.LogicalID }},
		{"duplicate physical", func(root *StateRoot, _ *uint64) { root.KeyDirectory.Offset = root.ChunkDirectory.Offset }},
		{"unaligned file end", func(_ *StateRoot, end *uint64) { (*end)-- }},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, end := valid, fileEnd
			test.mutate(&root, &end)
			page := make([]byte, testSuperblockPageSize)
			if _, err := EncodeStateRootPage(page, root, end); !errors.Is(err, ErrInvalidWrite) {
				t.Fatalf("EncodeStateRootPage = %v, want %v", err, ErrInvalidWrite)
			}
		})
	}

	empty := StateRoot{
		StoreID:        testStoreID,
		Generation:     1,
		PageSize:       testSuperblockPageSize,
		NextLogicalID:  2,
		ChunkDocuments: 64,
	}
	page := make([]byte, testSuperblockPageSize)
	if _, err := EncodeStateRootPage(page, empty, 2*uint64(testSuperblockPageSize)); err != nil {
		t.Fatalf("empty state root: %v", err)
	}
}

func TestStateRootDecodesVersionTwoWithoutFreeHint(t *testing.T) {
	want, fileEnd := testStateRoot(11)
	page := make([]byte, testSuperblockPageSize)
	if _, err := EncodeStateRootPage(page, want, fileEnd); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint32(page[PageHeaderSize:PageHeaderSize+4], stateRootVersionV2)
	binary.LittleEndian.PutUint32(page[PageHeaderSize+stateRootFreeHintOffset:PageHeaderSize+stateRootChunkRefOffset], 0)
	resealTestPage(page)
	want.FreeChunkHint = 0
	got, err := DecodeStateRootPage(page, fileEnd)
	if err != nil || got != want {
		t.Fatalf("DecodeStateRootPage(v2) = (%+v,%v), want (%+v,nil)", got, err, want)
	}
}

func TestStateRootDecodesVersionFourWithoutIndexGroups(t *testing.T) {
	want, _ := testStateRoot(11)
	want.Options |= StateOptionFloat64Columns
	want.Float64ScanHead = testStatePageRef(
		PageFloat64Catalog, 7, 6, want.Generation,
	)
	fileEnd := 8 * uint64(testSuperblockPageSize)
	page := make([]byte, testSuperblockPageSize)
	if _, err := EncodeStateRootPage(page, want, fileEnd); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint32(
		page[PageHeaderSize:PageHeaderSize+4], stateRootVersionV4,
	)
	resealTestPage(page)
	got, err := DecodeStateRootPage(page, fileEnd)
	if err != nil || got != want {
		t.Fatalf("DecodeStateRootPage(v4) = (%+v,%v), want (%+v,nil)", got, err, want)
	}
}

func TestStateRootRejectsResealedSemanticCorruption(t *testing.T) {
	root, fileEnd := testStateRoot(3)
	page := make([]byte, testSuperblockPageSize)
	if _, err := EncodeStateRootPage(page, root, fileEnd); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int{
		PageHeaderSize + 192,
		PageHeaderSize + 224,
		PageHeaderSize + 64 + 30,
	} {
		corrupt := append([]byte(nil), page...)
		corrupt[offset] = 1
		resealTestPage(corrupt)
		if _, err := DecodeStateRootPage(corrupt, fileEnd); !errors.Is(err, ErrStateRootCorrupt) {
			t.Fatalf("offset %d = %v, want %v", offset, err, ErrStateRootCorrupt)
		}
	}

	corrupt := append([]byte(nil), page...)
	binary.LittleEndian.PutUint32(corrupt[PageHeaderSize+40:PageHeaderSize+44], 0)
	resealTestPage(corrupt)
	if _, err := DecodeStateRootPage(corrupt, fileEnd); !errors.Is(err, ErrStateRootCorrupt) {
		t.Fatalf("semantic corruption = %v, want %v", err, ErrStateRootCorrupt)
	}
}

func TestRecoverStateRootFallsBackOnSemanticMismatch(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "state-root-recovery")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	pageSize := uint64(testSuperblockPageSize)
	fileEnd := 4 * pageSize
	empty := func(generation uint64) StateRoot {
		return StateRoot{
			StoreID: testStoreID, Generation: generation, PageSize: testSuperblockPageSize,
			NextLogicalID: 2, ChunkDocuments: 64,
		}
	}
	state1 := make([]byte, testSuperblockPageSize)
	state2 := make([]byte, testSuperblockPageSize)
	if _, err := EncodeStateRootPage(state1, empty(1), fileEnd); err != nil {
		t.Fatal(err)
	}
	if _, err := EncodeStateRootPage(state2, empty(2), fileEnd); err != nil {
		t.Fatal(err)
	}
	root1 := testSuperblock(1, 2*pageSize, state1)
	root1.FileEnd = fileEnd
	root2 := testSuperblock(2, 3*pageSize, state2)
	root2.FileEnd = fileEnd
	first := encodeTestSuperblock(t, root1)
	second := encodeTestSuperblock(t, root2)
	if err := file.Truncate(int64(fileEnd)); err != nil {
		t.Fatal(err)
	}
	writeAtTest(t, file, first[:], 0)
	writeAtTest(t, file, second[:], int64(pageSize))
	writeAtTest(t, file, state1, int64(root1.StateOffset))
	writeAtTest(t, file, state2, int64(root2.StateOffset))
	scratch := make([]byte, testSuperblockPageSize)

	gotSuper, gotState, slot, err := RecoverStateRoot(file, testSuperblockPageSize, scratch)
	if err != nil || gotSuper != root2 || gotState != empty(2) || slot != 1 {
		t.Fatalf("recover newest = (%+v,%+v,%d,%v)", gotSuper, gotState, slot, err)
	}

	// Keep both CRC layers valid while breaking the state/superblock generation
	// binding. Recovery must reject generation two and select generation one.
	binary.LittleEndian.PutUint64(state2[24:32], 7)
	resealTestPage(state2)
	root2.StateChecksum = PageChecksum(state2)
	second = encodeTestSuperblock(t, root2)
	writeAtTest(t, file, state2, int64(root2.StateOffset))
	writeAtTest(t, file, second[:], int64(pageSize))
	gotSuper, gotState, slot, err = RecoverStateRoot(file, testSuperblockPageSize, scratch)
	if err != nil || gotSuper != root1 || gotState != empty(1) || slot != 0 {
		t.Fatalf("semantic fallback = (%+v,%+v,%d,%v)", gotSuper, gotState, slot, err)
	}
}

func TestRecoverStateRootValidatesTopLevelDirectories(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "state-root-directories")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	pageSize := uint64(testSuperblockPageSize)
	fileEnd := 6 * pageSize
	empty := StateRoot{
		StoreID: testStoreID, Generation: 1, PageSize: testSuperblockPageSize,
		NextLogicalID: 2, ChunkDocuments: 64,
	}
	newer := StateRoot{
		StoreID: testStoreID, Generation: 2, PageSize: testSuperblockPageSize,
		DocumentCount: 1, NextLogicalID: 4, ChunkHighWater: 1, LiveChunks: 1, ChunkDocuments: 64,
		ChunkDirectory: testStatePageRef(PageChunkDirectory, 4, 2, 2),
		KeyDirectory:   testStatePageRef(PageKeyDirectory, 5, 3, 2),
	}
	state1 := make([]byte, testSuperblockPageSize)
	state2 := make([]byte, testSuperblockPageSize)
	if _, err := EncodeStateRootPage(state1, empty, fileEnd); err != nil {
		t.Fatal(err)
	}
	if _, err := EncodeStateRootPage(state2, newer, fileEnd); err != nil {
		t.Fatal(err)
	}
	chunkRoot := make([]byte, testSuperblockPageSize)
	keyRoot := make([]byte, testSuperblockPageSize)
	for _, item := range []struct {
		page []byte
		ref  PageRef
	}{
		{chunkRoot, newer.ChunkDirectory},
		{keyRoot, newer.KeyDirectory},
	} {
		if _, err := InitPage(item.page, PageHeader{
			StoreID: testStoreID, Generation: item.ref.Generation, LogicalID: item.ref.LogicalID,
			PageSize: testSuperblockPageSize, Kind: item.ref.Kind,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := SealPage(item.page); err != nil {
			t.Fatal(err)
		}
	}
	root1 := testSuperblock(1, 2*pageSize, state1)
	root1.FileEnd = fileEnd
	root2 := testSuperblock(2, 3*pageSize, state2)
	root2.FileEnd = fileEnd
	first := encodeTestSuperblock(t, root1)
	second := encodeTestSuperblock(t, root2)
	if err := file.Truncate(int64(fileEnd)); err != nil {
		t.Fatal(err)
	}
	writeAtTest(t, file, first[:], 0)
	writeAtTest(t, file, second[:], int64(pageSize))
	writeAtTest(t, file, state1, int64(root1.StateOffset))
	writeAtTest(t, file, state2, int64(root2.StateOffset))
	writeAtTest(t, file, chunkRoot, int64(newer.ChunkDirectory.Offset))
	writeAtTest(t, file, keyRoot, int64(newer.KeyDirectory.Offset))
	scratch := make([]byte, testSuperblockPageSize)

	gotSuper, gotState, slot, err := RecoverStateRoot(file, testSuperblockPageSize, scratch)
	if err != nil || gotSuper != root2 || gotState != newer || slot != 1 {
		t.Fatalf("recover directories = (%+v,%+v,%d,%v)", gotSuper, gotState, slot, err)
	}

	chunkRoot[PageHeaderSize] ^= 1
	writeAtTest(t, file, chunkRoot, int64(newer.ChunkDirectory.Offset))
	gotSuper, gotState, slot, err = RecoverStateRoot(file, testSuperblockPageSize, scratch)
	if err != nil || gotSuper != root1 || gotState != empty || slot != 0 {
		t.Fatalf("directory fallback = (%+v,%+v,%d,%v)", gotSuper, gotState, slot, err)
	}
}
