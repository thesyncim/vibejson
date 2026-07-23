package storeio

import (
	"encoding/binary"
	"errors"
	"os"
	"testing"
)

func testFreeDirectoryHeader(level uint8, logicalID uint64) FreeDirectoryHeader {
	return FreeDirectoryHeader{
		StoreID: testStoreID, Generation: 11, LogicalID: logicalID,
		PageSize: testSuperblockPageSize, Level: level,
	}
}

func TestFreeDirectoryLeafRoundTrip(t *testing.T) {
	header := testFreeDirectoryHeader(0, 40)
	pageSize := uint64(testSuperblockPageSize)
	extents := []FreeExtent{
		{Offset: 2 * pageSize, Length: pageSize, RetiredGeneration: 7},
		{Offset: 5 * pageSize, Length: 2 * pageSize, RetiredGeneration: 9},
		{Offset: 10 * pageSize, Length: pageSize, RetiredGeneration: 10},
	}
	page := make([]byte, testSuperblockPageSize)
	encoded, err := EncodeFreeDirectoryLeaf(page, header, extents, testKeyDirectoryFileEnd, testKeyDirectoryNextLogicalID)
	if err != nil {
		t.Fatal(err)
	}
	view, err := OpenFreeDirectoryPage(encoded, testKeyDirectoryFileEnd, testKeyDirectoryNextLogicalID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Header() != header || view.Len() != len(extents) {
		t.Fatalf("leaf = (%+v,%d), want (%+v,%d)", view.Header(), view.Len(), header, len(extents))
	}
	for rank, want := range extents {
		got, ok := view.ExtentAt(rank)
		if !ok || got != want {
			t.Fatalf("ExtentAt(%d) = (%+v,%v), want (%+v,true)", rank, got, ok, want)
		}
	}
	for _, test := range []struct {
		offset uint64
		want   int
	}{
		{0, 0}, {2 * pageSize, 0}, {3 * pageSize, 1},
		{6 * pageSize, 1}, {7 * pageSize, 2}, {20 * pageSize, 3},
	} {
		if got := view.LowerBound(test.offset); got != test.want {
			t.Fatalf("LowerBound(%d) = %d, want %d", test.offset, got, test.want)
		}
	}
}

func TestFreeDirectoryBranchRoundTrip(t *testing.T) {
	header := testFreeDirectoryHeader(1, 41)
	pageSize := uint64(testSuperblockPageSize)
	children := []FreeDirectoryChild{
		{Lower: 2 * pageSize, Ref: testFreeDirectoryRef(7, 20, 10)},
		{Lower: 8 * pageSize, Ref: testFreeDirectoryRef(8, 21, 11)},
	}
	page := make([]byte, testSuperblockPageSize)
	encoded, err := EncodeFreeDirectoryBranch(page, header, children, testKeyDirectoryFileEnd, testKeyDirectoryNextLogicalID)
	if err != nil {
		t.Fatal(err)
	}
	view, err := OpenFreeDirectoryPage(encoded, testKeyDirectoryFileEnd, testKeyDirectoryNextLogicalID)
	if err != nil {
		t.Fatal(err)
	}
	for rank, want := range children {
		got, ok := view.ChildAt(rank)
		if !ok || got != want {
			t.Fatalf("ChildAt(%d) = (%+v,%v), want (%+v,true)", rank, got, ok, want)
		}
	}
	if _, ok := view.Child(pageSize); ok {
		t.Fatal("offset before first lower bound hit")
	}
	for _, test := range []struct {
		offset uint64
		want   PageRef
	}{
		{2 * pageSize, children[0].Ref}, {7 * pageSize, children[0].Ref}, {9 * pageSize, children[1].Ref},
	} {
		got, ok := view.Child(test.offset)
		if !ok || got != test.want {
			t.Fatalf("Child(%d) = (%+v,%v), want (%+v,true)", test.offset, got, ok, test.want)
		}
	}
}

func TestFreeDirectoryRejectsInvalidAndCorrupt(t *testing.T) {
	header := testFreeDirectoryHeader(0, 40)
	pageSize := uint64(testSuperblockPageSize)
	valid := []FreeExtent{
		{Offset: 2 * pageSize, Length: pageSize, RetiredGeneration: 7},
		{Offset: 5 * pageSize, Length: pageSize, RetiredGeneration: 9},
	}
	for _, extents := range [][]FreeExtent{
		nil,
		{{Offset: pageSize, Length: pageSize, RetiredGeneration: 1}},
		{{Offset: 2*pageSize + 1, Length: pageSize, RetiredGeneration: 1}},
		{{Offset: 2 * pageSize, Length: pageSize}},
		{valid[1], valid[0]},
		{{Offset: 2 * pageSize, Length: 4 * pageSize, RetiredGeneration: 1}, valid[1]},
	} {
		page := make([]byte, testSuperblockPageSize)
		if _, err := EncodeFreeDirectoryLeaf(page, header, extents, testKeyDirectoryFileEnd, testKeyDirectoryNextLogicalID); !errors.Is(err, ErrInvalidWrite) {
			t.Fatalf("EncodeFreeDirectoryLeaf = %v, want %v", err, ErrInvalidWrite)
		}
	}

	page := make([]byte, testSuperblockPageSize)
	if _, err := EncodeFreeDirectoryLeaf(page, header, valid, testKeyDirectoryFileEnd, testKeyDirectoryNextLogicalID); err != nil {
		t.Fatal(err)
	}
	first := PageHeaderSize + FreeDirectoryPayloadHeaderSize
	for _, mutate := range []func([]byte){
		func(p []byte) { binary.LittleEndian.PutUint32(p[PageHeaderSize:], 2) },
		func(p []byte) { p[PageHeaderSize+8] = 1 },
		func(p []byte) { binary.LittleEndian.PutUint64(p[first+8:first+16], 1) },
		func(p []byte) { binary.LittleEndian.PutUint64(p[first+16:first+24], 0) },
		func(p []byte) { binary.LittleEndian.PutUint64(p[first:first+8], 6*pageSize) },
	} {
		corrupt := append([]byte(nil), page...)
		mutate(corrupt)
		resealTestPage(corrupt)
		if _, err := OpenFreeDirectoryPage(corrupt, testKeyDirectoryFileEnd, testKeyDirectoryNextLogicalID); !errors.Is(err, ErrFreeDirectoryCorrupt) {
			t.Fatalf("OpenFreeDirectoryPage = %v, want %v", err, ErrFreeDirectoryCorrupt)
		}
	}
}

func TestFreeDirectorySteadyAllocation(t *testing.T) {
	header := testFreeDirectoryHeader(0, 40)
	pageSize := uint64(testSuperblockPageSize)
	extents := []FreeExtent{
		{Offset: 2 * pageSize, Length: pageSize, RetiredGeneration: 7},
		{Offset: 5 * pageSize, Length: pageSize, RetiredGeneration: 9},
	}
	page := make([]byte, testSuperblockPageSize)
	if allocs := testing.AllocsPerRun(1000, func() {
		if _, err := EncodeFreeDirectoryLeaf(page, header, extents, testKeyDirectoryFileEnd, testKeyDirectoryNextLogicalID); err != nil {
			panic(err)
		}
		view, err := OpenFreeDirectoryPage(page, testKeyDirectoryFileEnd, testKeyDirectoryNextLogicalID)
		if err != nil {
			panic(err)
		}
		freeExtentSink, _ = view.ExtentAt(view.LowerBound(5 * pageSize))
	}); allocs != 0 {
		t.Fatalf("free-directory codec and lookup allocations = %g, want 0", allocs)
	}
}

func TestRecoverStateRootValidatesFreeDirectorySchema(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "free-directory-recovery-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	pageSize := uint64(testSuperblockPageSize)
	fileEnd := 5 * pageSize
	state := StateRoot{
		StoreID: testStoreID, Generation: 1, PageSize: testSuperblockPageSize,
		NextLogicalID: 3, ChunkDocuments: 64,
	}
	statePage := make([]byte, testSuperblockPageSize)
	if _, err := EncodeStateRootPage(statePage, state, fileEnd); err != nil {
		t.Fatal(err)
	}
	freePage := make([]byte, testSuperblockPageSize)
	extents := []FreeExtent{{Offset: 4 * pageSize, Length: pageSize, RetiredGeneration: 1}}
	freeHeader := testFreeDirectoryHeader(0, 2)
	freeHeader.Generation = 1
	if _, err := EncodeFreeDirectoryLeaf(freePage, freeHeader, extents, fileEnd, state.NextLogicalID); err != nil {
		t.Fatal(err)
	}
	root := testSuperblock(1, 2*pageSize, statePage)
	root.FileEnd = fileEnd
	root.FreeOffset = 3 * pageSize
	root.FreeLength = testSuperblockPageSize
	root.FreeChecksum = PageChecksum(freePage)
	encodedRoot := encodeTestSuperblock(t, root)
	if err := file.Truncate(int64(fileEnd)); err != nil {
		t.Fatal(err)
	}
	writeAtTest(t, file, encodedRoot[:], 0)
	writeAtTest(t, file, statePage, int64(root.StateOffset))
	writeAtTest(t, file, freePage, int64(root.FreeOffset))
	scratch := make([]byte, testSuperblockPageSize)
	gotRoot, gotState, slot, err := RecoverStateRoot(file, testSuperblockPageSize, scratch)
	if err != nil || gotRoot != root || gotState != state || slot != 0 {
		t.Fatalf("RecoverStateRoot = (%+v,%+v,%d,%v)", gotRoot, gotState, slot, err)
	}

	freePage[PageHeaderSize+8] = 1
	resealTestPage(freePage)
	root.FreeChecksum = PageChecksum(freePage)
	encodedRoot = encodeTestSuperblock(t, root)
	writeAtTest(t, file, freePage, int64(root.FreeOffset))
	writeAtTest(t, file, encodedRoot[:], 0)
	if _, _, _, err := RecoverStateRoot(file, testSuperblockPageSize, scratch); !errors.Is(err, ErrSuperblockNotFound) {
		t.Fatalf("semantic free corruption = %v, want %v", err, ErrSuperblockNotFound)
	}
}

var freeExtentSink FreeExtent

func testFreeDirectoryRef(logicalID, page, generation uint64) PageRef {
	return PageRef{
		Offset: page * uint64(testSuperblockPageSize), LogicalID: logicalID, Generation: generation,
		Length: testSuperblockPageSize, Kind: PageFreeDirectory,
	}
}
