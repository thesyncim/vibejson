package storeio

import (
	"fmt"
	"testing"
)

var (
	tabletLocalIdentityLabBenchLocation TabletLocalIdentityLabLocation
	tabletLocalIdentityLabBenchImage    []byte
	tabletLocalIdentityLabBenchDesc     TabletLocalIdentityLabDescriptor
	tabletLocalIdentityLabBenchRetired  []TabletLocalIdentityLabRetirement
)

func BenchmarkTabletLocalIdentityLabResolve(b *testing.B) {
	view := tabletLocalIdentityLabTestView(b, 3686)
	b.Run("hit", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(2)
		b.ResetTimer()
		var location TabletLocalIdentityLabLocation
		for i := 0; i < b.N; i++ {
			location, _ = view.Resolve(uint16(i & 2047))
		}
		tabletLocalIdentityLabBenchLocation = location
	})
	b.Run("empty", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(2)
		b.ResetTimer()
		var location TabletLocalIdentityLabLocation
		for i := 0; i < b.N; i++ {
			location, _ = view.Resolve(4000)
		}
		tabletLocalIdentityLabBenchLocation = location
	})
}

func BenchmarkTabletLocalIdentityLabBatchUpdate(b *testing.B) {
	base := tabletLocalIdentityLabTestView(b, 3072)
	edits := make([]TabletLocalIdentityLabEdit, 32)
	for i := range edits {
		edits[i] = TabletLocalIdentityLabEdit{
			LocalID:   uint16(i * 64),
			Operation: TabletLocalIdentityLabMove,
			Location: TabletLocalIdentityLabLocation{
				AnchorPageID: 12,
				RowSlot:      uint8(i),
			},
		}
	}
	images := [2][]byte{
		make([]byte, TabletLocalIdentityLabImageBytes),
		make([]byte, TabletLocalIdentityLabImageBytes),
	}
	retirements := [2][]TabletLocalIdentityLabRetirement{
		make([]TabletLocalIdentityLabRetirement, 0, 64),
		make([]TabletLocalIdentityLabRetirement, 0, 64),
	}
	b.ReportAllocs()
	b.SetBytes(TabletLocalIdentityLabImageBytes)
	b.ResetTimer()
	view := base
	for i := 0; i < b.N; i++ {
		slot := i & 1
		image, descriptor, retired, err := RewriteTabletLocalIdentityLab(
			images[slot], retirements[slot][:0], view,
			uint64(i)+8, 1, edits,
		)
		if err != nil {
			b.Fatal(err)
		}
		view = TabletLocalIdentityLabView{
			image: image, retirements: retired, descriptor: descriptor,
		}
	}
	tabletLocalIdentityLabBenchImage = view.image
	tabletLocalIdentityLabBenchDesc = view.descriptor
	tabletLocalIdentityLabBenchRetired = view.retirements
}

func BenchmarkTabletLocalIdentityLabWalk(b *testing.B) {
	view := tabletLocalIdentityLabTestView(b, 3686)
	b.ReportAllocs()
	b.SetBytes(int64(view.Descriptor().LiveCount))
	b.ResetTimer()
	var location TabletLocalIdentityLabLocation
	for i := 0; i < b.N; i++ {
		cursor := view.Cursor()
		for {
			_, next, ok := cursor.Next()
			if !ok {
				break
			}
			location = next
		}
	}
	tabletLocalIdentityLabBenchLocation = location
}

func BenchmarkTabletLocalIdentityLabBytesPerLive(b *testing.B) {
	for _, occupancy := range []int{50, 75, 90} {
		live := TabletLocalIdentityLabLocalCount * occupancy / 100
		b.Run(fmt.Sprintf("%d-percent", occupancy), func(b *testing.B) {
			view := tabletLocalIdentityLabTestView(b, live)
			b.ReportAllocs()
			b.ResetTimer()
			var location TabletLocalIdentityLabLocation
			for i := 0; i < b.N; i++ {
				location, _ = view.Resolve(uint16(i % live))
			}
			tabletLocalIdentityLabBenchLocation = location
			b.ReportMetric(
				TabletLocalIdentityLabBytesPerLive(live),
				"B/live",
			)
			b.ReportMetric(
				float64(TabletLocalIdentityLabDescriptorBytes),
				"descriptor-B",
			)
			b.ReportMetric(
				float64(TabletLocalIdentityLabImageBytes),
				"locator-B",
			)
			b.ReportMetric(
				float64(TabletLocalIdentityLabMaxRetirementBytes),
				"worst-retired-B",
			)
		})
	}
}
