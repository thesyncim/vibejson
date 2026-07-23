package storeio

import (
	"errors"
	"testing"
)

func testKeyPageRef(offset, logical uint64) PageRef {
	return PageRef{
		Offset: offset, LogicalID: logical, Generation: 3,
		Length: 4096, Kind: PageKeyDirectory,
	}
}

func TestKeyLeafPageRoundTripAndCollisionRange(t *testing.T) {
	header := PageKeyDirectoryHeader{
		StoreID:    testStoreID,
		Generation: 3,
		LogicalID:  10,
		PageSize:   4096,
		MinHash:    11,
		MaxHash:    99,
		Next:       testKeyPageRef(4*4096, 11),
	}
	entries := []PageKeyLocation{
		{Hash: 11, Chunk: 1, Slot: 2},
		{Hash: 42, Chunk: 3, Slot: 4},
		{Hash: 42, Chunk: 3, Slot: 7},
		{Hash: 42, Chunk: 8, Slot: 1},
		{Hash: 99, Chunk: 9, Slot: 63},
	}
	page, err := EncodePageKeyLeaf(make([]byte, 4096), header, entries, 32*4096, 64, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	view, err := OpenPageKeyDirectory(page, 32*4096, 64, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if view.Header() != header || view.Len() != len(entries) {
		t.Fatalf("view = (%+v,%d), want (%+v,%d)", view.Header(), view.Len(), header, len(entries))
	}
	first, end, ok := view.CandidateRange(42)
	if !ok || first != 1 || end != 4 {
		t.Fatalf("collision range = (%d,%d,%v), want (1,4,true)", first, end, ok)
	}
	for i := range entries {
		got, ok := view.LocationAt(i)
		if !ok || got != entries[i] {
			t.Fatalf("LocationAt(%d) = (%+v,%v), want %+v", i, got, ok, entries[i])
		}
	}
	if _, _, ok := view.CandidateRange(43); ok {
		t.Fatal("absent hash matched")
	}
}

func TestKeyBranchPageRoundTrip(t *testing.T) {
	entries := []PageKeyBranch{
		{MaxHash: 100, Child: testKeyPageRef(2*4096, 20)},
		{MaxHash: 500, Child: testKeyPageRef(3*4096, 21)},
		{MaxHash: 900, Child: testKeyPageRef(4*4096, 22)},
	}
	header := PageKeyDirectoryHeader{
		StoreID: testStoreID, Generation: 3, LogicalID: 30, PageSize: 4096,
		MinHash: 7, MaxHash: 900, Level: 1,
	}
	page, err := EncodePageKeyBranch(make([]byte, 4096), header, entries, 32*4096, 64)
	if err != nil {
		t.Fatal(err)
	}
	view, err := OpenPageKeyDirectory(page, 32*4096, 64, 100, 64)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		hash uint64
		want PageRef
		rank int
		ok   bool
	}{
		{7, entries[0].Child, 0, true},
		{100, entries[0].Child, 0, true},
		{101, entries[1].Child, 1, true},
		{900, entries[2].Child, 2, true},
		{901, PageRef{}, 0, false},
	} {
		got, rank, ok := view.ChildIndex(test.hash)
		if ok != test.ok || got != test.want || rank != test.rank {
			t.Fatalf("ChildIndex(%d) = (%+v,%d,%v), want (%+v,%d,%v)",
				test.hash, got, rank, ok, test.want, test.rank, test.ok)
		}
	}
}

func TestKeyBranchAllowsCollisionAcrossChildren(t *testing.T) {
	entries := []PageKeyBranch{
		{MaxHash: 42, Child: testKeyPageRef(2*4096, 20)},
		{MaxHash: 42, Child: testKeyPageRef(3*4096, 21)},
		{MaxHash: 99, Child: testKeyPageRef(4*4096, 22)},
	}
	header := PageKeyDirectoryHeader{
		StoreID: testStoreID, Generation: 3, LogicalID: 30, PageSize: 4096,
		MinHash: 42, MaxHash: 99, Level: 1,
	}
	page, err := EncodePageKeyBranch(make([]byte, 4096), header, entries, 32*4096, 64)
	if err != nil {
		t.Fatal(err)
	}
	view, err := OpenPageKeyDirectory(page, 32*4096, 64, 100, 64)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := view.Child(42); !ok || got != entries[0].Child {
		t.Fatalf("collision child = (%+v,%v), want first collision page", got, ok)
	}
}

func TestKeyDirectoryRejectsMalformedPages(t *testing.T) {
	header := PageKeyDirectoryHeader{
		StoreID: testStoreID, Generation: 3, LogicalID: 10, PageSize: 4096,
		MinHash: 1, MaxHash: 2,
	}
	entries := []PageKeyLocation{{Hash: 1, Chunk: 0, Slot: 0}, {Hash: 2, Chunk: 0, Slot: 1}}
	page, err := EncodePageKeyLeaf(make([]byte, 4096), header, entries, 32*4096, 64, 1, 64)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func([]byte){
		func(p []byte) { p[PageHeaderSize+26] = 1 },
		func(p []byte) { p[PageHeaderSize+PageKeyDirectoryPayloadHeaderSize+13] = 1 },
		func(p []byte) { p[PageHeaderSize+16] = 0 },
	} {
		corrupt := append([]byte(nil), page...)
		mutate(corrupt)
		resealTestPage(corrupt)
		if _, err := OpenPageKeyDirectory(corrupt, 32*4096, 64, 1, 64); !errors.Is(err, ErrKeyDirectoryCorrupt) {
			t.Fatalf("malformed page error = %v", err)
		}
	}
}

func TestKeyHashStableVectors(t *testing.T) {
	storeID := [16]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	for _, test := range []struct {
		key  string
		want uint64
	}{
		{"", 0xabac0158050fc4dc},
		{"a", 0x1c2697ab786a6237},
		{"abcdefgh", 0x12d8c08c2ee9e620},
		{"abcdefghijk", 0x61a776dfdbd799c8},
	} {
		if got := KeyHash(storeID, test.key); got != test.want {
			t.Fatalf("KeyHash(%q) = %#x, want %#x", test.key, got, test.want)
		}
	}
}
