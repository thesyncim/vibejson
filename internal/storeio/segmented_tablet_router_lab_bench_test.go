package storeio

import (
	"fmt"
	"testing"
)

var (
	segmentedTabletRouterLabBenchmarkRoute  SegmentedTabletRouterLabRoute
	segmentedTabletRouterLabBenchmarkRef    PageRef
	segmentedTabletRouterLabBenchmarkZone   BucketZone
	segmentedTabletRouterLabBenchmarkCursor SegmentedTabletRouterLabCursor
	segmentedTabletRouterLabBenchmarkCOW    SegmentedTabletRouterLabCOWResult
	segmentedTabletRouterLabBenchmarkSplit  SegmentedTabletRouterLabSplitResult
)

func BenchmarkSegmentedTabletRouterLabResident(b *testing.B) {
	view, leaves, _ := segmentedTabletRouterLabTestView(b, 3072)
	target := 1536
	key := leaves[target].Fence
	hash := KeyHashBytes(segmentedTabletRouterLabTestSeed, key)
	bucket := segmentedTabletRouterLabTestBucket(leaves[target].Ref)

	b.Run("route/reused-hash", func(b *testing.B) {
		b.ReportAllocs()
		var route SegmentedTabletRouterLabRoute
		for b.Loop() {
			route = view.RouteHashed(hash, key)
		}
		segmentedTabletRouterLabBenchmarkRoute = route
	})
	b.Run("route/hash-included", func(b *testing.B) {
		b.ReportAllocs()
		var route SegmentedTabletRouterLabRoute
		for b.Loop() {
			route = view.Route(segmentedTabletRouterLabTestSeed, key)
		}
		segmentedTabletRouterLabBenchmarkRoute = route
	})
	b.Run("resolve/posting-bucket-id", func(b *testing.B) {
		b.ReportAllocs()
		var ref PageRef
		var zone BucketZone
		for b.Loop() {
			ref, zone, _ = view.ResolveBucketID(bucket)
		}
		segmentedTabletRouterLabBenchmarkRef = ref
		segmentedTabletRouterLabBenchmarkZone = zone
	})
	b.Run("lower-bound", func(b *testing.B) {
		b.ReportAllocs()
		var cursor SegmentedTabletRouterLabCursor
		for b.Loop() {
			cursor = view.LowerBound(key)
		}
		segmentedTabletRouterLabBenchmarkCursor = cursor
	})
	b.Run("ordered-scan/256-leaves", func(b *testing.B) {
		b.ReportAllocs()
		var route SegmentedTabletRouterLabRoute
		for b.Loop() {
			cursor := view.LowerBound(key)
			for range 256 {
				route, _ = cursor.Route(hash)
				cursor.Next()
			}
		}
		segmentedTabletRouterLabBenchmarkRoute = route
		b.ReportMetric(256, "leaves/op")
	})
}

func BenchmarkSegmentedTabletRouterLabOrdinaryCOW(b *testing.B) {
	view, leaves, refs := segmentedTabletRouterLabTestView(b, 3072)
	target := 1537
	bucket := segmentedTabletRouterLabTestBucket(leaves[target].Ref)
	leafRef := leaves[target].Ref
	leafRef.Offset += 8
	leafRef.Generation = 10_001
	leafRef.Length = 317
	var zone BucketZone
	zone = BucketZone{9, 8, 7, 6}
	pageID := uint8(target / SegmentedTabletRouterLabRowsPerPage)
	anchorRef := refs[pageID]
	anchorRef.Offset += 1 << 20
	anchorRef.Generation = 10_001
	rootDst := make([]byte, SegmentedTabletRouterLabRootBytes)
	pageDst := make([]byte, SegmentedTabletRouterLabAnchorPageBytes)
	b.ReportAllocs()
	b.SetBytes(SegmentedTabletRouterLabRootBytes +
		SegmentedTabletRouterLabAnchorPageBytes)
	var result SegmentedTabletRouterLabCOWResult
	for b.Loop() {
		var err error
		result, err = view.RewriteHandle(
			rootDst, pageDst, 10_001, bucket, leafRef, zone, anchorRef,
		)
		if err != nil {
			b.Fatal(err)
		}
	}
	segmentedTabletRouterLabBenchmarkCOW = result
	b.ReportMetric(float64(result.Bytes), "COW-B/op")
}

func BenchmarkSegmentedTabletRouterLabStructuralSplit(b *testing.B) {
	view, _, refs := segmentedTabletRouterLabTestView(b, 3072)
	const pageID = uint8(5)
	newPageID := view.pageCount
	leftRef := refs[pageID]
	leftRef.Offset += 2 << 20
	leftRef.Generation = 10_001
	rightRef := PageRef{
		Offset: 3 << 20,
		LogicalID: segmentedTabletRouterLabAnchorLogicalID(
			view.tabletID, newPageID,
		),
		Generation: 10_001,
		Length:     SegmentedTabletRouterLabAnchorPageBytes,
		Kind:       view.anchorKind,
	}
	rootDst := make([]byte, SegmentedTabletRouterLabRootBytes)
	locatorDst := make([]byte, SegmentedTabletRouterLabLocatorBytes)
	leftDst := make([]byte, SegmentedTabletRouterLabAnchorPageBytes)
	rightDst := make([]byte, SegmentedTabletRouterLabAnchorPageBytes)
	b.ReportAllocs()
	b.SetBytes(SegmentedTabletRouterLabRootBytes +
		SegmentedTabletRouterLabLocatorBytes +
		2*SegmentedTabletRouterLabAnchorPageBytes)
	var result SegmentedTabletRouterLabSplitResult
	for b.Loop() {
		var err error
		result, err = view.SplitAnchorPage(
			rootDst, locatorDst, leftDst, rightDst,
			10_001, pageID, newPageID, 128, leftRef, rightRef,
		)
		if err != nil {
			b.Fatal(err)
		}
	}
	segmentedTabletRouterLabBenchmarkSplit = result
	b.ReportMetric(float64(result.Bytes), "COW-B/op")
}

func BenchmarkSegmentedTabletRouterLabRoutingSpace(b *testing.B) {
	for _, rows := range []int{187, 195, 230} {
		b.Run(fmt.Sprintf("%d-rows", rows), func(b *testing.B) {
			value := SegmentedTabletRouterLabRoutingBytesPerDocument(
				TabletLocalIdentityLabLocalCount, rows,
			)
			b.ReportMetric(value, "B/document")
			for b.Loop() {
			}
			b.ReportMetric(value, "B/document")
			b.ReportMetric(35, "B/leaf")
		})
	}
}
