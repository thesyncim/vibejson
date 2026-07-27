package storeio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
)

var (
	commonPrimaryLeafPrototypeTestSeed = [16]byte{
		0x92, 0x17, 0x44, 0x5a, 0x81, 0xe3, 0x29, 0x6c,
		0x0f, 0xb8, 0x37, 0xd1, 0xaa, 0x63, 0x54, 0x2e,
	}
	commonPrimaryLeafPrototypeTestStoreID = [16]byte{0x51, 0x39, 0x7a, 0x12}
)

func commonPrimaryLeafPrototypeTestBounds(t testing.TB) CommonPrimaryLeafPrototypeBounds {
	t.Helper()
	layout, err := MutableStoreLayout(physicalPageQuantum)
	if err != nil {
		t.Fatal(err)
	}
	return CommonPrimaryLeafPrototypeBounds{
		FileEnd:           layout.DataStart + 1024*uint64(physicalPageQuantum),
		NextLogicalID:     CommonPrimaryLeafPrototypeFirstDynamicLogicalID + 4096,
		AllocationQuantum: physicalPageQuantum,
	}
}

func commonPrimaryLeafPrototypeTestRecords(
	t testing.TB, class CommonPrimaryLeafPrototypeClass,
	count, valueBytes int,
) []CommonPrimaryLeafPrototypeRecord {
	t.Helper()
	records := make([]CommonPrimaryLeafPrototypeRecord, count)
	for index := range records {
		key := make([]byte, 8)
		binary.BigEndian.PutUint64(key, uint64(index+1))
		value := bytes.Repeat([]byte{byte(index + 1)}, valueBytes)
		records[index] = CommonPrimaryLeafPrototypeRecord{
			Key: key, Value: CommonPrimaryLeafPrototypeValue{Inline: value},
		}
	}
	if err := PlaceCommonPrimaryLeafPrototypeRecords(
		class, commonPrimaryLeafPrototypeTestSeed, records,
	); err != nil {
		t.Fatal(err)
	}
	return records
}

func commonPrimaryLeafPrototypeTestRef(
	t testing.TB,
	bounds CommonPrimaryLeafPrototypeBounds,
	bucket BucketID,
	generation uint64,
	pageSize uint32,
) PageRef {
	t.Helper()
	logicalID, ok := CommonPrimaryLeafPrototypeLogicalID(bucket)
	if !ok {
		t.Fatal("invalid test bucket")
	}
	layout, err := MutableStoreLayout(bounds.AllocationQuantum)
	if err != nil {
		t.Fatal(err)
	}
	return PageRef{
		Offset: layout.DataStart, LogicalID: logicalID,
		Generation: generation, Length: pageSize,
		Kind: commonPrimaryLeafPrototypePageKind,
	}
}

func commonPrimaryLeafPrototypeOpenTest(
	t testing.TB,
	class CommonPrimaryLeafPrototypeClass,
	pageSize uint32,
	records []CommonPrimaryLeafPrototypeRecord,
) ([]byte, CommonPrimaryLeafPrototypeView, PageRef, CommonPrimaryLeafPrototypeBounds) {
	t.Helper()
	const generation = uint64(17)
	const bucket = BucketID(991)
	bounds := commonPrimaryLeafPrototypeTestBounds(t)
	page, err := EncodeCommonPrimaryLeafPrototype(
		make([]byte, pageSize), class,
		CommonPrimaryLeafPrototypeHeader{
			StoreID:    commonPrimaryLeafPrototypeTestStoreID,
			Generation: generation, Bucket: bucket, PageSize: pageSize,
		},
		commonPrimaryLeafPrototypeTestSeed, records, bounds,
	)
	if err != nil {
		t.Fatal(err)
	}
	ref := commonPrimaryLeafPrototypeTestRef(
		t, bounds, bucket, generation, pageSize,
	)
	view, err := OpenCommonPrimaryLeafPrototype(
		page, commonPrimaryLeafPrototypeTestSeed, bucket,
		ref, generation, bounds,
	)
	if err != nil {
		t.Fatal(err)
	}
	return page, view, ref, bounds
}

func TestCommonPrimaryLeafPrototypeGeometry(t *testing.T) {
	if got := CommonPrimaryLeafPrototypeStructuralBytes(
		CommonPrimaryLeafPrototypeNarrow,
		CommonPrimaryLeafPrototypeNarrowLive,
		CommonPrimaryLeafPrototypeNarrowBytes,
	); got != 973 {
		t.Fatalf("Narrow structural bytes=%d want=973", got)
	}
	if got := CommonPrimaryLeafPrototypeStructuralBytesPerKey(
		CommonPrimaryLeafPrototypeNarrow,
		CommonPrimaryLeafPrototypeNarrowLive,
		CommonPrimaryLeafPrototypeNarrowBytes,
	); got < 4.989 || got > 4.990 {
		t.Fatalf("Narrow B/key=%f", got)
	}
	if got := CommonPrimaryLeafPrototypeStructuralBytes(
		CommonPrimaryLeafPrototypeWide,
		CommonPrimaryLeafPrototypeWideSlots,
		CommonPrimaryLeafPrototypeWideBytes,
	); got != 1275 {
		t.Fatalf("Wide structural bytes=%d want=1275", got)
	}
	if got := CommonPrimaryLeafPrototypeStructuralBytesPerKey(
		CommonPrimaryLeafPrototypeWide,
		CommonPrimaryLeafPrototypeWideSlots,
		CommonPrimaryLeafPrototypeWideBytes,
	); got < 4.980 || got > 4.981 {
		t.Fatalf("Wide B/key=%f", got)
	}

	// Extent and slot class are orthogonal. A representative 249-byte inline
	// JSON value retains 195 stable rows in one 64 KiB Narrow leaf.
	records := commonPrimaryLeafPrototypeTestRecords(
		t, CommonPrimaryLeafPrototypeNarrow,
		CommonPrimaryLeafPrototypeNarrowLive, 249,
	)
	page, view, _, _ := commonPrimaryLeafPrototypeOpenTest(
		t, CommonPrimaryLeafPrototypeNarrow, 64<<10, records,
	)
	if len(page) != 64<<10 || view.Len() != len(records) {
		t.Fatalf("representative extent=%d rows=%d", len(page), view.Len())
	}
	t.Logf(
		"representative 8B key + 249B value: structural=%.3f B/key physical=%.3f B/key",
		CommonPrimaryLeafPrototypeStructuralBytesPerKey(
			CommonPrimaryLeafPrototypeNarrow, len(records), len(page),
		),
		float64(len(page))/float64(len(records)),
	)
}

func TestCommonPrimaryLeafPrototypeRoundTripAndScan(t *testing.T) {
	for _, test := range []struct {
		name     string
		class    CommonPrimaryLeafPrototypeClass
		pageSize uint32
		count    int
	}{
		{"Narrow", CommonPrimaryLeafPrototypeNarrow, 4 << 10, 195},
		{"Wide", CommonPrimaryLeafPrototypeWide, 8 << 10, 256},
	} {
		t.Run(test.name, func(t *testing.T) {
			records := commonPrimaryLeafPrototypeTestRecords(
				t, test.class, test.count, 8,
			)
			_, view, _, _ := commonPrimaryLeafPrototypeOpenTest(
				t, test.class, test.pageSize, records,
			)
			for index, record := range records {
				slot, value, ok := view.Lookup(record.Key)
				if !ok || slot != record.Slot ||
					!bytes.Equal(value.Inline, record.Value.Inline) {
					t.Fatalf("lookup rank=%d slot=%d ok=%v", index, slot, ok)
				}
			}
			if _, _, ok := view.Lookup([]byte("absent!!")); ok {
				t.Fatal("unexpected miss hit")
			}
			it := view.AllRows()
			for index := 0; index < len(records); index++ {
				row, ok := it.Next()
				if !ok || !bytes.Equal(row.Key, records[index].Key) ||
					!bytes.Equal(row.Value.Inline, records[index].Value.Inline) {
					t.Fatalf("scan row=%d ok=%v", index, ok)
				}
			}
			if _, ok := it.Next(); ok {
				t.Fatal("iterator ran past end")
			}
			lower := records[33].Key
			upper := records[47].Key
			rangeIt := view.Range(lower, upper)
			for index := 33; index < 47; index++ {
				row, ok := rangeIt.Next()
				if !ok || !bytes.Equal(row.Key, records[index].Key) {
					t.Fatalf("range rank=%d ok=%v", index, ok)
				}
			}
			if _, ok := rangeIt.Next(); ok {
				t.Fatal("range ignored upper bound")
			}
		})
	}
}

func TestCommonPrimaryLeafPrototypeEmpty(t *testing.T) {
	page, view, ref, bounds := commonPrimaryLeafPrototypeOpenTest(
		t, CommonPrimaryLeafPrototypeNarrow, 4<<10, nil,
	)
	if view.Len() != 0 {
		t.Fatalf("empty Len=%d", view.Len())
	}
	if _, _, ok := view.Lookup([]byte("missing")); ok {
		t.Fatal("empty lookup hit")
	}
	it := view.AllRows()
	if _, ok := it.Next(); ok {
		t.Fatal("empty scan produced a row")
	}
	if _, err := OpenCommonPrimaryLeafPrototype(
		page, commonPrimaryLeafPrototypeTestSeed, 991,
		ref, uint64(1)<<48, bounds,
	); !errors.Is(err, ErrCommonPrimaryLeafPrototypeCorrupt) {
		t.Fatalf("oversized selecting generation err=%v", err)
	}
}

func TestCommonPrimaryLeafPrototypeLongKeyEscape(t *testing.T) {
	lengths := []int{1, 126, 127, 128, 255, 256}
	records := make([]CommonPrimaryLeafPrototypeRecord, len(lengths))
	for index, length := range lengths {
		key := bytes.Repeat([]byte{byte('a' + index)}, length)
		records[index] = CommonPrimaryLeafPrototypeRecord{
			Key: key,
			Value: CommonPrimaryLeafPrototypeValue{
				Inline: []byte{byte(index + 1)},
			},
		}
	}
	if err := PlaceCommonPrimaryLeafPrototypeRecords(
		CommonPrimaryLeafPrototypeNarrow,
		commonPrimaryLeafPrototypeTestSeed, records,
	); err != nil {
		t.Fatal(err)
	}
	_, view, _, _ := commonPrimaryLeafPrototypeOpenTest(
		t, CommonPrimaryLeafPrototypeNarrow, 4<<10, records,
	)
	for _, record := range records {
		slot, value, ok := view.Lookup(record.Key)
		if !ok || slot != record.Slot || len(value.Inline) != 1 {
			t.Fatalf("long-key lookup length=%d", len(record.Key))
		}
	}
	tooLong := bytes.Repeat([]byte("z"), 257)
	if _, _, ok := view.Lookup(tooLong); ok {
		t.Fatal("257-byte key was accepted")
	}
}

func TestCommonPrimaryLeafPrototypeOverflowRef(t *testing.T) {
	bounds := commonPrimaryLeafPrototypeTestBounds(t)
	layout, err := MutableStoreLayout(bounds.AllocationQuantum)
	if err != nil {
		t.Fatal(err)
	}
	overflow := PageRef{
		Offset:     layout.DataStart + 8*uint64(bounds.AllocationQuantum),
		LogicalID:  CommonPrimaryLeafPrototypeFirstDynamicLogicalID + 7,
		Generation: 9, Length: bounds.AllocationQuantum,
		Kind: PageOverflow,
	}
	records := []CommonPrimaryLeafPrototypeRecord{{
		Key: []byte("overflow"),
		Value: CommonPrimaryLeafPrototypeValue{
			Overflow: overflow,
		},
	}}
	if err := PlaceCommonPrimaryLeafPrototypeRecords(
		CommonPrimaryLeafPrototypeNarrow,
		commonPrimaryLeafPrototypeTestSeed, records,
	); err != nil {
		t.Fatal(err)
	}
	_, view, _, _ := commonPrimaryLeafPrototypeOpenTest(
		t, CommonPrimaryLeafPrototypeNarrow, 4<<10, records,
	)
	_, got, ok := view.Lookup(records[0].Key)
	if !ok || got.Overflow != overflow || len(got.Inline) != 0 {
		t.Fatalf("overflow=%+v ok=%v", got, ok)
	}

	bad := records
	bad[0].Value.Overflow.Generation = 18
	if _, err := EncodeCommonPrimaryLeafPrototype(
		make([]byte, 4<<10), CommonPrimaryLeafPrototypeNarrow,
		CommonPrimaryLeafPrototypeHeader{
			StoreID:    commonPrimaryLeafPrototypeTestStoreID,
			Generation: 17, Bucket: 991, PageSize: 4 << 10,
		},
		commonPrimaryLeafPrototypeTestSeed, bad, bounds,
	); !errors.Is(err, ErrInvalidWrite) {
		t.Fatalf("future overflow generation err=%v", err)
	}
}

func TestCommonPrimaryLeafPrototypeMutations(t *testing.T) {
	records := commonPrimaryLeafPrototypeTestRecords(
		t, CommonPrimaryLeafPrototypeNarrow, 100, 8,
	)
	_, view, ref, bounds := commonPrimaryLeafPrototypeOpenTest(
		t, CommonPrimaryLeafPrototypeNarrow, 4<<10, records,
	)
	target := records[40]
	updatedValue := CommonPrimaryLeafPrototypeValue{
		Inline: []byte("updated!"),
	}
	updated, err := view.UpdateTo(
		make([]byte, 4<<10), ref.Generation+1,
		target.Slot, target.Key, updatedValue,
	)
	if err != nil {
		t.Fatal(err)
	}
	updatedRef := ref
	updatedRef.Generation++
	updatedView, err := OpenCommonPrimaryLeafPrototype(
		updated, commonPrimaryLeafPrototypeTestSeed, view.header.Bucket,
		updatedRef, updatedRef.Generation, bounds,
	)
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := updatedView.LookupSlot(target.Slot, target.Key); !ok || !bytes.Equal(value.Inline, updatedValue.Inline) {
		t.Fatal("equal-length update not visible")
	}
	for _, record := range records {
		if bytes.Equal(record.Key, target.Key) {
			continue
		}
		if _, ok := updatedView.LookupSlot(record.Slot, record.Key); !ok {
			t.Fatalf("update moved stable slot %d", record.Slot)
		}
	}

	deleted, err := updatedView.DeleteTo(
		make([]byte, 4<<10), updatedRef.Generation+1,
		target.Slot, target.Key,
	)
	if err != nil {
		t.Fatal(err)
	}
	deletedRef := updatedRef
	deletedRef.Generation++
	deletedView, err := OpenCommonPrimaryLeafPrototype(
		deleted, commonPrimaryLeafPrototypeTestSeed, view.header.Bucket,
		deletedRef, deletedRef.Generation, bounds,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := deletedView.Lookup(target.Key); ok {
		t.Fatal("delete retained target")
	}
	inserted, slot, err := deletedView.InsertTo(
		make([]byte, 4<<10), deletedRef.Generation+1,
		target.Key, target.Value,
	)
	if err != nil {
		t.Fatal(err)
	}
	insertedRef := deletedRef
	insertedRef.Generation++
	insertedView, err := OpenCommonPrimaryLeafPrototype(
		inserted, commonPrimaryLeafPrototypeTestSeed, view.header.Bucket,
		insertedRef, insertedRef.Generation, bounds,
	)
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := insertedView.LookupSlot(slot, target.Key); !ok || !bytes.Equal(value.Inline, target.Value.Inline) {
		t.Fatal("insert not visible")
	}
}

func TestCommonPrimaryLeafPrototypeMutationCanonical(t *testing.T) {
	for _, test := range []struct {
		name     string
		class    CommonPrimaryLeafPrototypeClass
		pageSize uint32
		count    int
	}{
		{"Narrow", CommonPrimaryLeafPrototypeNarrow, 4 << 10, 195},
		{"Wide", CommonPrimaryLeafPrototypeWide, 8 << 10, 224},
	} {
		t.Run(test.name, func(t *testing.T) {
			records := commonPrimaryLeafPrototypeTestRecords(
				t, test.class, test.count, 8,
			)
			_, view, ref, bounds := commonPrimaryLeafPrototypeOpenTest(
				t, test.class, test.pageSize, records,
			)
			targets := []int{0, len(records) - 1}
			for index := range records {
				if int(records[index].Slot) >= CommonPrimaryLeafPrototypeNormalSlots {
					targets = append(targets, index)
					break
				}
			}
			for _, targetIndex := range targets {
				target := records[targetIndex]
				t.Run(fmt.Sprintf("rank-%d-slot-%d", targetIndex, target.Slot), func(t *testing.T) {
					deleted, err := view.DeleteTo(
						make([]byte, test.pageSize), ref.Generation+1,
						target.Slot, target.Key,
					)
					if err != nil {
						t.Fatal(err)
					}
					expectedRecords := make(
						[]CommonPrimaryLeafPrototypeRecord, 0, len(records)-1,
					)
					expectedRecords = append(expectedRecords, records[:targetIndex]...)
					expectedRecords = append(expectedRecords, records[targetIndex+1:]...)
					expectedDeleted, err := EncodeCommonPrimaryLeafPrototype(
						make([]byte, test.pageSize), test.class,
						CommonPrimaryLeafPrototypeHeader{
							StoreID:    commonPrimaryLeafPrototypeTestStoreID,
							Generation: ref.Generation + 1,
							Bucket:     refBucket(ref), PageSize: test.pageSize,
						},
						commonPrimaryLeafPrototypeTestSeed, expectedRecords, bounds,
					)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(deleted, expectedDeleted) {
						t.Fatal("localized delete is not canonical")
					}
					deletedRef := ref
					deletedRef.Generation++
					deletedView, err := OpenCommonPrimaryLeafPrototype(
						deleted, commonPrimaryLeafPrototypeTestSeed,
						refBucket(ref), deletedRef, deletedRef.Generation, bounds,
					)
					if err != nil {
						t.Fatal(err)
					}
					restored, err := deletedView.InsertSlotTo(
						make([]byte, test.pageSize), deletedRef.Generation+1,
						target.Slot, target.Key, target.Value,
					)
					if err != nil {
						t.Fatal(err)
					}
					expectedRestored, err := EncodeCommonPrimaryLeafPrototype(
						make([]byte, test.pageSize), test.class,
						CommonPrimaryLeafPrototypeHeader{
							StoreID:    commonPrimaryLeafPrototypeTestStoreID,
							Generation: deletedRef.Generation + 1,
							Bucket:     refBucket(ref), PageSize: test.pageSize,
						},
						commonPrimaryLeafPrototypeTestSeed, records, bounds,
					)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(restored, expectedRestored) {
						t.Fatal("localized stable-slot restore is not canonical")
					}
				})
			}
		})
	}
}

func refBucket(ref PageRef) BucketID {
	return BucketID(ref.LogicalID - CommonPrimaryLeafPrototypeLeafLogicalIDBase)
}

func TestCommonPrimaryLeafPrototypeFailsClosed(t *testing.T) {
	records := commonPrimaryLeafPrototypeTestRecords(
		t, CommonPrimaryLeafPrototypeNarrow, 64, 8,
	)
	page, _, ref, bounds := commonPrimaryLeafPrototypeOpenTest(
		t, CommonPrimaryLeafPrototypeNarrow, 4<<10, records,
	)
	for _, test := range []struct {
		name   string
		mutate func([]byte, *PageRef)
		reseal bool
	}{
		{"checksum", func(page []byte, _ *PageRef) { page[100] ^= 1 }, false},
		{"wrong logical", func(_ []byte, ref *PageRef) { ref.LogicalID++ }, false},
		{"future generation", func(_ []byte, ref *PageRef) { ref.Generation++ }, false},
		{"bad class", func(page []byte, _ *PageRef) {
			page[PageHeaderSize+2] = 7
		}, true},
		{"bad long escape", func(page []byte, _ *PageRef) {
			// Turn the first short key into an escape whose prefix decodes to
			// length one; structural validation must reject it.
			payload := page[PageHeaderSize:]
			layout := commonPrimaryLeafPrototypeLayoutFor(
				CommonPrimaryLeafPrototypeNarrow, len(records), len(page),
			)
			commonPrimaryLeafPrototypePutKeyLength(
				payload, &layout, 0, commonPrimaryLeafPrototypeEscapeLength,
			)
			payload[layout.heapStart] = 0
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			corrupt := append([]byte(nil), page...)
			badRef := ref
			test.mutate(corrupt, &badRef)
			if test.reseal {
				if _, err := sealPage(corrupt, false); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := OpenCommonPrimaryLeafPrototype(
				corrupt, commonPrimaryLeafPrototypeTestSeed, 991,
				badRef, ref.Generation, bounds,
			); !errors.Is(err, ErrCommonPrimaryLeafPrototypeCorrupt) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestCommonPrimaryLeafPrototypeNoReadAllocations(t *testing.T) {
	records := commonPrimaryLeafPrototypeTestRecords(
		t, CommonPrimaryLeafPrototypeNarrow, 195, 8,
	)
	page, view, ref, bounds := commonPrimaryLeafPrototypeOpenTest(
		t, CommonPrimaryLeafPrototypeNarrow, 4<<10, records,
	)
	if allocs := testing.AllocsPerRun(100, func() {
		opened, err := OpenCommonPrimaryLeafPrototype(
			page, commonPrimaryLeafPrototypeTestSeed, 991,
			ref, ref.Generation, bounds,
		)
		if err != nil || opened.Len() != len(records) {
			panic(fmt.Sprintf("open: %v", err))
		}
	}); allocs != 0 {
		t.Fatalf("Open allocations=%f", allocs)
	}
	target := records[len(records)/2]
	hash := commonPrimaryLeafPrototypeHash(
		commonPrimaryLeafPrototypeTestSeed, target.Key,
	)
	if allocs := testing.AllocsPerRun(1000, func() {
		_, value, ok := view.LookupHashed(hash, target.Key)
		if !ok || len(value.Inline) != 8 {
			panic("lookup")
		}
	}); allocs != 0 {
		t.Fatalf("Lookup allocations=%f", allocs)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		it := view.AllRows()
		count := 0
		for {
			_, ok := it.Next()
			if !ok {
				break
			}
			count++
		}
		if count != len(records) {
			panic("scan")
		}
	}); allocs != 0 {
		t.Fatalf("scan allocations=%f", allocs)
	}
}
