package storeio

import (
	"encoding/binary"
	"errors"
	"testing"
)

func testIndexDirectoryHeader(level uint8, logicalID uint64) IndexDirectoryHeader {
	return IndexDirectoryHeader{
		StoreID: testStoreID, Generation: 11, LogicalID: logicalID,
		PageSize: testSuperblockPageSize, Level: level,
	}
}

func TestIndexDirectoryLeafRoundTripAndLookup(t *testing.T) {
	header := testIndexDirectoryHeader(0, 30)
	shared := testIndexPageRef(PageIndexPosting, 12, 12, 10)
	entries := []IndexDirectoryEntry{
		{Key: IndexDirectoryKey{IndexID: 0, TupleHash: 1}, Posting: IndexPostingRef{Page: shared, Segment: 0, Flags: IndexPostingImmutableBase}},
		{Key: IndexDirectoryKey{IndexID: 0, TupleHash: 5}, Posting: IndexPostingRef{Page: shared, Segment: 1, Flags: IndexPostingImmutableBase}},
		{Key: IndexDirectoryKey{IndexID: 2, TupleHash: 0}, Posting: IndexPostingRef{Page: testIndexPageRef(PageIndexPosting, 13, 13, 11), Segment: 7}},
	}
	page := make([]byte, testSuperblockPageSize)
	encoded, err := EncodeIndexDirectoryLeaf(page, header, entries, testKeyDirectoryFileEnd, testKeyDirectoryNextLogicalID, 3)
	if err != nil {
		t.Fatal(err)
	}
	view, err := OpenIndexDirectoryPage(encoded, testKeyDirectoryFileEnd, testKeyDirectoryNextLogicalID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if view.Header() != header || view.Len() != len(entries) {
		t.Fatalf("leaf = (%+v,%d), want (%+v,%d)", view.Header(), view.Len(), header, len(entries))
	}
	for rank, want := range entries {
		got, ok := view.Lookup(want.Key)
		if !ok || got != want.Posting {
			t.Fatalf("Lookup(%+v) = (%+v,%v), want (%+v,true)", want.Key, got, ok, want.Posting)
		}
		entry, ok := view.EntryAt(rank)
		if !ok || entry != want {
			t.Fatalf("EntryAt(%d) = (%+v,%v), want (%+v,true)", rank, entry, ok, want)
		}
	}
	for _, key := range []IndexDirectoryKey{{}, {IndexID: 0, TupleHash: 2}, {IndexID: 1}, {IndexID: 2, TupleHash: 1}} {
		if got, ok := view.Lookup(key); ok {
			t.Fatalf("Lookup(%+v) = (%+v,true), want miss", key, got)
		}
	}
	if _, ok := view.Child(IndexDirectoryKey{}); ok {
		t.Fatal("leaf Child hit")
	}
}

func TestIndexDirectoryBranchRoundTrip(t *testing.T) {
	header := testIndexDirectoryHeader(2, 31)
	children := []IndexDirectoryChild{
		{Lower: IndexDirectoryKey{}, Ref: testIndexPageRef(PageIndexDirectory, 4, 4, 10)},
		{Lower: IndexDirectoryKey{IndexID: 1}, Ref: testIndexPageRef(PageIndexDirectory, 5, 5, 11)},
		{Lower: IndexDirectoryKey{IndexID: 2, TupleHash: 100}, Ref: testIndexPageRef(PageIndexDirectory, 6, 6, 11)},
	}
	page := make([]byte, testSuperblockPageSize)
	encoded, err := EncodeIndexDirectoryBranch(page, header, children, testKeyDirectoryFileEnd, testKeyDirectoryNextLogicalID)
	if err != nil {
		t.Fatal(err)
	}
	view, err := OpenIndexDirectoryPage(encoded, testKeyDirectoryFileEnd, testKeyDirectoryNextLogicalID, 3)
	if err != nil {
		t.Fatal(err)
	}
	for rank, want := range children {
		got, ok := view.ChildAt(rank)
		if !ok || got != want {
			t.Fatalf("ChildAt(%d) = (%+v,%v), want (%+v,true)", rank, got, ok, want)
		}
	}
	for _, test := range []struct {
		key  IndexDirectoryKey
		want PageRef
	}{
		{IndexDirectoryKey{}, children[0].Ref},
		{IndexDirectoryKey{IndexID: 1, TupleHash: 99}, children[1].Ref},
		{IndexDirectoryKey{IndexID: 2, TupleHash: 200}, children[2].Ref},
	} {
		got, ok := view.Child(test.key)
		if !ok || got != test.want {
			t.Fatalf("Child(%+v) = (%+v,%v), want (%+v,true)", test.key, got, ok, test.want)
		}
	}
}

func TestIndexDirectoryRejectsInvalidAndCorrupt(t *testing.T) {
	header := testIndexDirectoryHeader(0, 30)
	ref := testIndexPageRef(PageIndexPosting, 12, 12, 11)
	valid := []IndexDirectoryEntry{
		{Key: IndexDirectoryKey{TupleHash: 1}, Posting: IndexPostingRef{Page: ref}},
		{Key: IndexDirectoryKey{IndexID: 1}, Posting: IndexPostingRef{Page: ref, Segment: 1}},
	}
	for _, entries := range [][]IndexDirectoryEntry{
		nil,
		{valid[0], valid[0]},
		{valid[1], valid[0]},
		{{Key: IndexDirectoryKey{IndexID: 2}, Posting: IndexPostingRef{Page: ref}}},
		{{Key: IndexDirectoryKey{}, Posting: IndexPostingRef{Page: testIndexPageRef(PageKeyDirectory, 12, 12, 11)}}},
		{{Key: IndexDirectoryKey{}, Posting: IndexPostingRef{Page: ref, Flags: 2}}},
	} {
		page := make([]byte, testSuperblockPageSize)
		if _, err := EncodeIndexDirectoryLeaf(page, header, entries, testKeyDirectoryFileEnd, testKeyDirectoryNextLogicalID, 2); !errors.Is(err, ErrInvalidWrite) {
			t.Fatalf("EncodeIndexDirectoryLeaf = %v, want %v", err, ErrInvalidWrite)
		}
	}

	page := make([]byte, testSuperblockPageSize)
	if _, err := EncodeIndexDirectoryLeaf(page, header, valid, testKeyDirectoryFileEnd, testKeyDirectoryNextLogicalID, 2); err != nil {
		t.Fatal(err)
	}
	first := PageHeaderSize + IndexDirectoryPayloadHeaderSize
	for _, mutate := range []func([]byte){
		func(p []byte) { binary.LittleEndian.PutUint32(p[PageHeaderSize:], 2) },
		func(p []byte) { p[PageHeaderSize+8] = 1 },
		func(p []byte) { p[first+46] = 1 },
		func(p []byte) { binary.LittleEndian.PutUint32(p[first:first+4], 2) },
		func(p []byte) { p[first+50] = 2 },
		func(p []byte) { p[first+52] = 1 },
	} {
		corrupt := append([]byte(nil), page...)
		mutate(corrupt)
		resealTestPage(corrupt)
		if _, err := OpenIndexDirectoryPage(corrupt, testKeyDirectoryFileEnd, testKeyDirectoryNextLogicalID, 2); !errors.Is(err, ErrIndexDirectoryCorrupt) {
			t.Fatalf("OpenIndexDirectoryPage = %v, want %v", err, ErrIndexDirectoryCorrupt)
		}
	}
}

func TestIndexDirectorySteadyAllocation(t *testing.T) {
	header := testIndexDirectoryHeader(0, 30)
	ref := testIndexPageRef(PageIndexPosting, 12, 12, 11)
	entries := []IndexDirectoryEntry{
		{Key: IndexDirectoryKey{TupleHash: 1}, Posting: IndexPostingRef{Page: ref}},
		{Key: IndexDirectoryKey{IndexID: 1}, Posting: IndexPostingRef{Page: ref, Segment: 1}},
	}
	page := make([]byte, testSuperblockPageSize)
	if allocs := testing.AllocsPerRun(1000, func() {
		if _, err := EncodeIndexDirectoryLeaf(page, header, entries, testKeyDirectoryFileEnd, testKeyDirectoryNextLogicalID, 2); err != nil {
			panic(err)
		}
		view, err := OpenIndexDirectoryPage(page, testKeyDirectoryFileEnd, testKeyDirectoryNextLogicalID, 2)
		if err != nil {
			panic(err)
		}
		indexPostingSink, _ = view.Lookup(entries[1].Key)
	}); allocs != 0 {
		t.Fatalf("index-directory codec and lookup allocations = %g, want 0", allocs)
	}
}

var indexPostingSink IndexPostingRef

func testIndexPageRef(kind PageKind, logicalID, page, generation uint64) PageRef {
	return PageRef{
		Offset: page * uint64(testSuperblockPageSize), LogicalID: logicalID, Generation: generation,
		Length: testSuperblockPageSize, Kind: kind,
	}
}
