package storeio

import (
	"encoding/binary"
	"errors"
	"testing"
)

func testTTLDirectoryHeader(level uint8, logicalID uint64) TTLDirectoryHeader {
	return TTLDirectoryHeader{
		StoreID: testStoreID, Generation: 11, LogicalID: logicalID,
		PageSize: testSuperblockPageSize, Level: level,
	}
}

func TestTTLDirectoryLeafRoundTrip(t *testing.T) {
	header := testTTLDirectoryHeader(0, 20)
	entries := []TTLKey{
		{Deadline: -1, Chunk: 2, Slot: 3},
		{Deadline: 100, Chunk: 1, Slot: 2},
		{Deadline: 100, Chunk: 1, Slot: 4},
		{Deadline: 100, Chunk: 7, Slot: 0},
		{Deadline: 200, Chunk: 0, Slot: 63},
	}
	page := make([]byte, testSuperblockPageSize)
	encoded, err := EncodeTTLDirectoryLeaf(page, header, entries, testKeyDirectoryNextLogicalID, 8, 64)
	if err != nil {
		t.Fatal(err)
	}
	view, err := OpenTTLDirectoryPage(encoded, testKeyDirectoryFileEnd, testKeyDirectoryNextLogicalID, 8, 64)
	if err != nil {
		t.Fatal(err)
	}
	if view.Header() != header || view.Len() != len(entries) {
		t.Fatalf("leaf = (%+v,%d), want (%+v,%d)", view.Header(), view.Len(), header, len(entries))
	}
	for i, want := range entries {
		got, ok := view.EntryAt(i)
		if !ok || got != want {
			t.Fatalf("EntryAt(%d) = (%+v,%v), want (%+v,true)", i, got, ok, want)
		}
	}
	for _, test := range []struct {
		key  TTLKey
		want int
	}{
		{TTLKey{Deadline: -2}, 0},
		{TTLKey{Deadline: 100, Chunk: 1, Slot: 3}, 2},
		{TTLKey{Deadline: 200}, 4},
		{TTLKey{Deadline: 201}, 5},
	} {
		if got := view.LowerBound(test.key); got != test.want {
			t.Fatalf("LowerBound(%+v) = %d, want %d", test.key, got, test.want)
		}
	}
	if _, ok := view.Child(TTLKey{}); ok {
		t.Fatal("leaf Child hit")
	}
}

func TestTTLDirectoryBranchRoundTrip(t *testing.T) {
	header := testTTLDirectoryHeader(1, 21)
	children := []TTLDirectoryChild{
		{Lower: TTLKey{Deadline: -100}, Ref: testTTLDirectoryRef(6, 6, 10)},
		{Lower: TTLKey{Deadline: 100, Chunk: 2}, Ref: testTTLDirectoryRef(7, 7, 11)},
		{Lower: TTLKey{Deadline: 200}, Ref: testTTLDirectoryRef(8, 8, 11)},
	}
	page := make([]byte, testSuperblockPageSize)
	encoded, err := EncodeTTLDirectoryBranch(page, header, children, testKeyDirectoryFileEnd, testKeyDirectoryNextLogicalID)
	if err != nil {
		t.Fatal(err)
	}
	view, err := OpenTTLDirectoryPage(encoded, testKeyDirectoryFileEnd, testKeyDirectoryNextLogicalID, 8, 64)
	if err != nil {
		t.Fatal(err)
	}
	for rank, want := range children {
		got, ok := view.ChildAt(rank)
		if !ok || got != want {
			t.Fatalf("ChildAt(%d) = (%+v,%v), want (%+v,true)", rank, got, ok, want)
		}
	}
	if _, ok := view.Child(TTLKey{Deadline: -101}); ok {
		t.Fatal("key before first lower bound hit")
	}
	for _, test := range []struct {
		key  TTLKey
		want PageRef
	}{
		{TTLKey{Deadline: -100}, children[0].Ref},
		{TTLKey{Deadline: 150}, children[1].Ref},
		{TTLKey{Deadline: 300}, children[2].Ref},
	} {
		got, ok := view.Child(test.key)
		if !ok || got != test.want {
			t.Fatalf("Child(%+v) = (%+v,%v), want (%+v,true)", test.key, got, ok, test.want)
		}
	}
}

func TestTTLDirectoryRejectsInvalidAndCorrupt(t *testing.T) {
	header := testTTLDirectoryHeader(0, 20)
	valid := []TTLKey{{Deadline: 1, Chunk: 0, Slot: 1}, {Deadline: 2, Chunk: 1, Slot: 2}}
	for _, entries := range [][]TTLKey{
		nil,
		{valid[0], valid[0]},
		{valid[1], valid[0]},
		{{Deadline: 1, Chunk: 2}},
		{{Deadline: 1, Slot: 64}},
	} {
		page := make([]byte, testSuperblockPageSize)
		if _, err := EncodeTTLDirectoryLeaf(page, header, entries, testKeyDirectoryNextLogicalID, 2, 64); !errors.Is(err, ErrInvalidWrite) {
			t.Fatalf("EncodeTTLDirectoryLeaf = %v, want %v", err, ErrInvalidWrite)
		}
	}

	page := make([]byte, testSuperblockPageSize)
	if _, err := EncodeTTLDirectoryLeaf(page, header, valid, testKeyDirectoryNextLogicalID, 2, 64); err != nil {
		t.Fatal(err)
	}
	first := PageHeaderSize + TTLDirectoryPayloadHeaderSize
	for _, mutate := range []func([]byte){
		func(p []byte) { binary.LittleEndian.PutUint32(p[PageHeaderSize:], 2) },
		func(p []byte) { p[PageHeaderSize+8] = 1 },
		func(p []byte) { p[first+13] = 1 },
		func(p []byte) { p[first+12] = 64 },
		func(p []byte) { binary.LittleEndian.PutUint64(p[first:first+8], 3) },
	} {
		corrupt := append([]byte(nil), page...)
		mutate(corrupt)
		resealTestPage(corrupt)
		if _, err := OpenTTLDirectoryPage(corrupt, testKeyDirectoryFileEnd, testKeyDirectoryNextLogicalID, 2, 64); !errors.Is(err, ErrTTLDirectoryCorrupt) {
			t.Fatalf("OpenTTLDirectoryPage = %v, want %v", err, ErrTTLDirectoryCorrupt)
		}
	}
}

func TestTTLDirectorySteadyAllocation(t *testing.T) {
	header := testTTLDirectoryHeader(0, 20)
	entries := []TTLKey{{Deadline: 1, Chunk: 0, Slot: 1}, {Deadline: 2, Chunk: 1, Slot: 2}}
	page := make([]byte, testSuperblockPageSize)
	if allocs := testing.AllocsPerRun(1000, func() {
		if _, err := EncodeTTLDirectoryLeaf(page, header, entries, testKeyDirectoryNextLogicalID, 2, 64); err != nil {
			panic(err)
		}
		view, err := OpenTTLDirectoryPage(page, testKeyDirectoryFileEnd, testKeyDirectoryNextLogicalID, 2, 64)
		if err != nil {
			panic(err)
		}
		ttlKeySink, _ = view.EntryAt(view.LowerBound(TTLKey{Deadline: 2}))
	}); allocs != 0 {
		t.Fatalf("TTL-directory codec and lookup allocations = %g, want 0", allocs)
	}
}

var ttlKeySink TTLKey

func testTTLDirectoryRef(logicalID, page, generation uint64) PageRef {
	return PageRef{
		Offset: page * uint64(testSuperblockPageSize), LogicalID: logicalID, Generation: generation,
		Length: testSuperblockPageSize, Kind: PageTTLDirectory,
	}
}
