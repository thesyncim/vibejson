package storeio

import (
	"encoding/binary"
	"errors"
	"testing"
)

const testOverflowNextLogicalID = uint64(100)

func testOverflowHeader(logicalID, offset, total uint64, next PageRef) OverflowPageHeader {
	return OverflowPageHeader{
		StoreID: testStoreID, Generation: 11, LogicalID: logicalID,
		PageSize: testSuperblockPageSize, Chunk: 7, Slot: 5,
		Total: total, Offset: offset, Next: next,
	}
}

func testOverflowRef(logicalID, page, generation uint64) PageRef {
	return PageRef{
		Offset: page * uint64(testSuperblockPageSize), LogicalID: logicalID, Generation: generation,
		Length: testSuperblockPageSize, Kind: PageOverflow,
	}
}

func TestOverflowPageChainRoundTrip(t *testing.T) {
	pieces := [][]byte{[]byte("hello"), []byte(" wor"), []byte("ld!")}
	refs := []PageRef{
		testOverflowRef(10, 10, 11), testOverflowRef(11, 11, 11), testOverflowRef(12, 12, 11),
	}
	fileEnd := uint64(16 * testSuperblockPageSize)
	var offset uint64
	var joined []byte
	for i, piece := range pieces {
		var next PageRef
		if i+1 < len(refs) {
			next = refs[i+1]
		}
		header := testOverflowHeader(refs[i].LogicalID, offset, 12, next)
		page := make([]byte, testSuperblockPageSize)
		encoded, err := EncodeOverflowPage(page, header, piece, fileEnd, testOverflowNextLogicalID, testSuperblockPageSize, 8, 64)
		if err != nil {
			t.Fatal(err)
		}
		view, err := OpenOverflowPage(encoded, fileEnd, testOverflowNextLogicalID, testSuperblockPageSize, 8, 64)
		if err != nil {
			t.Fatal(err)
		}
		if view.Header() != header || string(view.Data()) != string(piece) || cap(view.Data()) != len(piece) {
			t.Fatalf("piece %d = (%+v,%q), want (%+v,%q)", i, view.Header(), view.Data(), header, piece)
		}
		joined = append(joined, view.Data()...)
		offset += uint64(len(piece))
	}
	if string(joined) != "hello world!" {
		t.Fatalf("joined = %q", joined)
	}
}

func TestOverflowPageVariableExtent(t *testing.T) {
	header := testOverflowHeader(10, 0, 5000, PageRef{})
	header.PageSize = 8192
	data := make([]byte, 5000)
	page := make([]byte, header.PageSize)
	fileEnd := uint64(16 * testSuperblockPageSize)
	if _, err := EncodeOverflowPage(page, header, data, fileEnd, testOverflowNextLogicalID, testSuperblockPageSize, 8, 64); err != nil {
		t.Fatal(err)
	}
	view, err := OpenOverflowPage(page, fileEnd, testOverflowNextLogicalID, testSuperblockPageSize, 8, 64)
	if err != nil || view.Header() != header || len(view.Data()) != len(data) {
		t.Fatalf("variable extent = (%+v,%d,%v)", view.Header(), len(view.Data()), err)
	}
}

func TestOverflowPageRejectsInvalidAndCorrupt(t *testing.T) {
	fileEnd := uint64(16 * testSuperblockPageSize)
	validNext := testOverflowRef(11, 11, 11)
	valid := testOverflowHeader(10, 0, 8, validNext)
	for _, mutate := range []func(*OverflowPageHeader, *[]byte){
		func(h *OverflowPageHeader, _ *[]byte) { h.StoreID = [16]byte{} },
		func(h *OverflowPageHeader, _ *[]byte) { h.LogicalID = StateRootLogicalID },
		func(h *OverflowPageHeader, _ *[]byte) { h.Chunk = 8 },
		func(h *OverflowPageHeader, _ *[]byte) { h.Slot = 64 },
		func(h *OverflowPageHeader, _ *[]byte) { h.Total = 4 },
		func(h *OverflowPageHeader, _ *[]byte) { h.Offset = 8 },
		func(h *OverflowPageHeader, _ *[]byte) { h.Next.LogicalID = h.LogicalID },
		func(h *OverflowPageHeader, _ *[]byte) { h.Next.Kind = PageDocument },
		func(_ *OverflowPageHeader, data *[]byte) { *data = nil },
	} {
		header := valid
		data := []byte("1234")
		mutate(&header, &data)
		page := make([]byte, testSuperblockPageSize)
		if _, err := EncodeOverflowPage(page, header, data, fileEnd, testOverflowNextLogicalID, testSuperblockPageSize, 8, 64); !errors.Is(err, ErrInvalidWrite) {
			t.Fatalf("EncodeOverflowPage = %v, want %v", err, ErrInvalidWrite)
		}
	}

	page := make([]byte, testSuperblockPageSize)
	if _, err := EncodeOverflowPage(page, valid, []byte("1234"), fileEnd, testOverflowNextLogicalID, testSuperblockPageSize, 8, 64); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func([]byte){
		func(p []byte) { binary.LittleEndian.PutUint32(p[PageHeaderSize:], 2) },
		func(p []byte) { p[PageHeaderSize+7] = 1 },
		func(p []byte) { binary.LittleEndian.PutUint32(p[PageHeaderSize+12:], 3) },
		func(p []byte) { binary.LittleEndian.PutUint64(p[PageHeaderSize+24:], 8) },
		func(p []byte) { p[PageHeaderSize+32+29] = 1 },
	} {
		corrupt := append([]byte(nil), page...)
		mutate(corrupt)
		resealTestPage(corrupt)
		if _, err := OpenOverflowPage(corrupt, fileEnd, testOverflowNextLogicalID, testSuperblockPageSize, 8, 64); !errors.Is(err, ErrOverflowPageCorrupt) {
			t.Fatalf("OpenOverflowPage = %v, want %v", err, ErrOverflowPageCorrupt)
		}
	}
}

func TestOverflowPageSteadyAllocation(t *testing.T) {
	fileEnd := uint64(16 * testSuperblockPageSize)
	header := testOverflowHeader(10, 0, 4, PageRef{})
	data := []byte("1234")
	page := make([]byte, testSuperblockPageSize)
	if allocs := testing.AllocsPerRun(1000, func() {
		if _, err := EncodeOverflowPage(page, header, data, fileEnd, testOverflowNextLogicalID, testSuperblockPageSize, 8, 64); err != nil {
			panic(err)
		}
		view, err := OpenOverflowPage(page, fileEnd, testOverflowNextLogicalID, testSuperblockPageSize, 8, 64)
		if err != nil {
			panic(err)
		}
		overflowDataSink = view.Data()
	}); allocs != 0 {
		t.Fatalf("overflow-page codec allocations = %g, want 0", allocs)
	}
}

var overflowDataSink []byte
