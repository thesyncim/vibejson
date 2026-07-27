package storeio

import (
	"testing"
)

var (
	commonPrimaryLeafPrototypeBenchmarkSlot     uint8
	commonPrimaryLeafPrototypeBenchmarkBytes    []byte
	commonPrimaryLeafPrototypeBenchmarkValue    CommonPrimaryLeafPrototypeValue
	commonPrimaryLeafPrototypeBenchmarkRow      CommonPrimaryLeafPrototypeRow
	commonPrimaryLeafPrototypeBenchmarkView     CommonPrimaryLeafPrototypeView
	commonPrimaryLeafPrototypeBenchmarkPage     []byte
	commonPrimaryLeafPrototypeBenchmarkBool     bool
	commonPrimaryLeafPrototypeBenchmarkOverflow bool
	commonPrimaryLeafPrototypeBenchmarkInt      int
)

func commonPrimaryLeafPrototypeBenchmarkFixture(
	b testing.TB,
) ([]CommonPrimaryLeafPrototypeRecord, []byte, CommonPrimaryLeafPrototypeView, PageRef, CommonPrimaryLeafPrototypeBounds) {
	b.Helper()
	records := commonPrimaryLeafPrototypeTestRecords(
		b, CommonPrimaryLeafPrototypeNarrow,
		CommonPrimaryLeafPrototypeNarrowLive, 8,
	)
	page, view, ref, bounds := commonPrimaryLeafPrototypeOpenTest(
		b, CommonPrimaryLeafPrototypeNarrow, 4<<10, records,
	)
	return records, page, view, ref, bounds
}

func commonPrimaryLeafPrototypeAdaptiveBenchmarkFixture(
	b testing.TB,
	records []CommonPrimaryLeafPrototypeRecord,
) AdaptiveOrderedLeafLabView {
	b.Helper()
	adaptive := make([]AdaptiveOrderedLeafLabRecord, len(records))
	for index := range records {
		adaptive[index] = AdaptiveOrderedLeafLabRecord{
			Key: records[index].Key, Value: records[index].Value.Inline,
		}
	}
	if err := PlaceAdaptiveOrderedLeafLabRecords(
		AdaptiveOrderedLeafLabNarrow,
		commonPrimaryLeafPrototypeTestSeed, adaptive,
	); err != nil {
		b.Fatal(err)
	}
	page, err := EncodeAdaptiveOrderedLeafLab(
		make([]byte, AdaptiveOrderedLeafLabNarrowBytes),
		AdaptiveOrderedLeafLabNarrow,
		AdaptiveOrderedLeafLabHeader{BucketID: 991, Generation: 17},
		commonPrimaryLeafPrototypeTestSeed, adaptive,
	)
	if err != nil {
		b.Fatal(err)
	}
	view, err := OpenAdaptiveOrderedLeafLab(
		page, commonPrimaryLeafPrototypeTestSeed,
	)
	if err != nil {
		b.Fatal(err)
	}
	return view
}

func BenchmarkCommonPrimaryLeafPrototypePointLookup(b *testing.B) {
	records, _, view, _, _ := commonPrimaryLeafPrototypeBenchmarkFixture(b)
	var normal, stash CommonPrimaryLeafPrototypeRecord
	for _, record := range records {
		if int(record.Slot) < CommonPrimaryLeafPrototypeNormalSlots {
			normal = record
		} else {
			stash = record
			break
		}
	}
	if normal.Key == nil || stash.Key == nil {
		b.Fatal("fixture lacks normal/stash")
	}
	miss := []byte("not-here")
	for _, test := range []struct {
		name string
		key  []byte
	}{
		{"NormalHit", normal.Key},
		{"StashHit", stash.Key},
		{"Miss", miss},
	} {
		hash := commonPrimaryLeafPrototypeHash(
			commonPrimaryLeafPrototypeTestSeed, test.key,
		)
		b.Run(test.name+"/Prehashed", func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				commonPrimaryLeafPrototypeBenchmarkSlot,
					commonPrimaryLeafPrototypeBenchmarkBytes,
					commonPrimaryLeafPrototypeBenchmarkOverflow,
					commonPrimaryLeafPrototypeBenchmarkBool =
					view.LookupRawHashed(hash, test.key)
			}
		})
		b.Run(test.name+"/HashIncluded", func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				commonPrimaryLeafPrototypeBenchmarkSlot,
					commonPrimaryLeafPrototypeBenchmarkBytes,
					commonPrimaryLeafPrototypeBenchmarkOverflow,
					commonPrimaryLeafPrototypeBenchmarkBool =
					view.LookupRaw(test.key)
			}
		})
	}

	hashes := make([]uint64, len(records))
	for index := range records {
		hashes[index] = commonPrimaryLeafPrototypeHash(
			commonPrimaryLeafPrototypeTestSeed, records[index].Key,
		)
	}
	b.Run("AllKeyAverage/Prehashed", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(records)))
		b.ResetTimer()
		for range b.N {
			for index := range records {
				commonPrimaryLeafPrototypeBenchmarkSlot,
					commonPrimaryLeafPrototypeBenchmarkBytes,
					commonPrimaryLeafPrototypeBenchmarkOverflow,
					commonPrimaryLeafPrototypeBenchmarkBool =
					view.LookupRawHashed(hashes[index], records[index].Key)
			}
		}
		b.StopTimer()
		b.ReportMetric(
			float64(b.Elapsed().Nanoseconds())/
				float64(int64(b.N)*int64(len(records))),
			"ns/key",
		)
	})
	adaptive := commonPrimaryLeafPrototypeAdaptiveBenchmarkFixture(b, records)
	adaptiveMissHash := adaptiveOrderedLeafLabKeyHash(
		commonPrimaryLeafPrototypeTestSeed, miss,
	)
	b.Run("FairAdaptive/Miss/Prehashed", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			commonPrimaryLeafPrototypeBenchmarkSlot,
				commonPrimaryLeafPrototypeBenchmarkBytes,
				commonPrimaryLeafPrototypeBenchmarkOverflow,
				commonPrimaryLeafPrototypeBenchmarkBool =
				adaptive.LookupHashed(adaptiveMissHash, miss)
		}
	})
}

func BenchmarkCommonPrimaryLeafPrototypeScan(b *testing.B) {
	records, _, view, _, _ := commonPrimaryLeafPrototypeBenchmarkFixture(b)
	adaptive := commonPrimaryLeafPrototypeAdaptiveBenchmarkFixture(b, records)
	b.Run("CommonPage", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(records)))
		b.ResetTimer()
		for range b.N {
			it := view.AllRows()
			for {
				key, value, overflow, ok := it.NextRawBorrowed()
				if !ok {
					break
				}
				commonPrimaryLeafPrototypeBenchmarkBytes = value
				commonPrimaryLeafPrototypeBenchmarkOverflow = overflow
				commonPrimaryLeafPrototypeBenchmarkInt = len(key)
			}
		}
		b.StopTimer()
		b.ReportMetric(
			float64(b.Elapsed().Nanoseconds())/
				float64(int64(b.N)*int64(len(records))),
			"ns/doc",
		)
	})
	b.Run("AdaptiveLab", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(records)))
		b.ResetTimer()
		for range b.N {
			it := adaptive.AllRows()
			for {
				key, value, overflow, ok := it.NextBorrowed()
				if !ok {
					break
				}
				commonPrimaryLeafPrototypeBenchmarkValue =
					CommonPrimaryLeafPrototypeValue{Inline: value}
				commonPrimaryLeafPrototypeBenchmarkBool = overflow
				commonPrimaryLeafPrototypeBenchmarkInt = len(key)
			}
		}
		b.StopTimer()
		b.ReportMetric(
			float64(b.Elapsed().Nanoseconds())/
				float64(int64(b.N)*int64(len(records))),
			"ns/doc",
		)
	})
}

func BenchmarkCommonPrimaryLeafPrototypeOpen(b *testing.B) {
	_, page, _, ref, bounds := commonPrimaryLeafPrototypeBenchmarkFixture(b)
	b.Run("4KiB/Narrow195", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(page)))
		for range b.N {
			var err error
			commonPrimaryLeafPrototypeBenchmarkView, err =
				OpenCommonPrimaryLeafPrototype(
					page, commonPrimaryLeafPrototypeTestSeed, 991,
					ref, ref.Generation, bounds,
				)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	representative := commonPrimaryLeafPrototypeTestRecords(
		b, CommonPrimaryLeafPrototypeNarrow, 195, 249,
	)
	largePage, _, largeRef, largeBounds :=
		commonPrimaryLeafPrototypeOpenTest(
			b, CommonPrimaryLeafPrototypeNarrow, 64<<10, representative,
		)
	b.Run("64KiB/Narrow195/249BValue", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(largePage)))
		for range b.N {
			var err error
			commonPrimaryLeafPrototypeBenchmarkView, err =
				OpenCommonPrimaryLeafPrototype(
					largePage, commonPrimaryLeafPrototypeTestSeed, 991,
					largeRef, largeRef.Generation, largeBounds,
				)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkCommonPrimaryLeafPrototypeMutation(b *testing.B) {
	records, _, view, ref, _ := commonPrimaryLeafPrototypeBenchmarkFixture(b)
	target := records[len(records)/2]
	replacement := CommonPrimaryLeafPrototypeValue{Inline: []byte("changed!")}
	b.Run("EqualLengthUpdate", func(b *testing.B) {
		dst := make([]byte, 4<<10)
		b.ReportAllocs()
		b.SetBytes(4 << 10)
		b.ResetTimer()
		for range b.N {
			var err error
			commonPrimaryLeafPrototypeBenchmarkPage, err = view.UpdateTo(
				dst, ref.Generation+1, target.Slot, target.Key, replacement,
			)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Delete", func(b *testing.B) {
		dst := make([]byte, 4<<10)
		b.ReportAllocs()
		b.SetBytes(4 << 10)
		b.ResetTimer()
		for range b.N {
			var err error
			commonPrimaryLeafPrototypeBenchmarkPage, err = view.DeleteTo(
				dst, ref.Generation+1, target.Slot, target.Key,
			)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	deletedPage, err := view.DeleteTo(
		make([]byte, 4<<10), ref.Generation+1, target.Slot, target.Key,
	)
	if err != nil {
		b.Fatal(err)
	}
	deletedRef := ref
	deletedRef.Generation++
	deleted, err := OpenCommonPrimaryLeafPrototype(
		deletedPage, commonPrimaryLeafPrototypeTestSeed, 991,
		deletedRef, deletedRef.Generation, view.bounds,
	)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("Insert", func(b *testing.B) {
		dst := make([]byte, 4<<10)
		b.ReportAllocs()
		b.SetBytes(4 << 10)
		b.ResetTimer()
		for range b.N {
			var err error
			commonPrimaryLeafPrototypeBenchmarkPage,
				commonPrimaryLeafPrototypeBenchmarkSlot, err =
				deleted.InsertTo(
					dst, deletedRef.Generation+1,
					target.Key, target.Value,
				)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("StableSlotRestore", func(b *testing.B) {
		dst := make([]byte, 4<<10)
		b.ReportAllocs()
		b.SetBytes(4 << 10)
		b.ResetTimer()
		for range b.N {
			var err error
			commonPrimaryLeafPrototypeBenchmarkPage, err =
				deleted.InsertSlotTo(
					dst, deletedRef.Generation+1, target.Slot,
					target.Key, target.Value,
				)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}
