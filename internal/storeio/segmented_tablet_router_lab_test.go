package storeio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

var segmentedTabletRouterLabTestSeed = [16]byte{
	0x73, 0x65, 0x67, 0x6d, 0x65, 0x6e, 0x74, 0x65,
	0x64, 0x2d, 0x72, 0x6f, 0x75, 0x74, 0x65, 0x72,
}

func segmentedTabletRouterLabTestLeafLogicalID(
	t testing.TB, bucket BucketID,
) uint64 {
	t.Helper()
	logicalID, ok := SegmentedTabletRouterLabLeafLogicalID(bucket)
	if !ok {
		t.Fatalf("derive leaf logical ID for %d", bucket)
	}
	return logicalID
}

func segmentedTabletRouterLabTestAnchorLogicalID(
	t testing.TB, tabletID uint32, pageID uint8,
) uint64 {
	t.Helper()
	logicalID, ok := SegmentedTabletRouterLabAnchorLogicalID(
		tabletID, pageID,
	)
	if !ok {
		t.Fatalf("derive anchor logical ID for %d/%d", tabletID, pageID)
	}
	return logicalID
}

func segmentedTabletRouterLabTestBucket(ref PageRef) BucketID {
	return BucketID(ref.LogicalID - SegmentedTabletRouterLabLeafLogicalIDBase)
}

func segmentedTabletRouterLabTestInputs(
	t testing.TB, leafCount int,
) (
	SegmentedTabletRouterLabHeader,
	[]SegmentedTabletRouterLabLeaf,
	[]PageRef,
) {
	t.Helper()
	const tabletID = uint32(42)
	header := SegmentedTabletRouterLabHeader{
		StoreID:  segmentedTabletRouterLabTestSeed,
		TabletID: tabletID, Generation: 10_000,
		AnchorKind: PagePrimaryAnchor, LeafKind: PagePrimaryLeaf,
	}
	leaves := make([]SegmentedTabletRouterLabLeaf, leafCount)
	for rank := range leaves {
		localID := uint16((rank*2053 + 17) & 4095)
		bucket, ok := MakeTabletLocalIdentityLabBucket(
			tabletID, uint32(localID),
		)
		if !ok {
			t.Fatal("compose bucket")
		}
		var fence []byte
		if rank != 0 {
			fence = []byte(fmt.Sprintf(
				"tenant/0042/document/%010d", rank*230,
			))
		}
		var zone BucketZone
		binary.LittleEndian.PutUint32(
			zone[:], uint32(rank)^0xa5a55a5a,
		)
		length := uint32(1 + rank%65536)
		if rank%5 == 0 {
			length = uint32(4096 << ((rank / 5) % 5))
		}
		leaves[rank] = SegmentedTabletRouterLabLeaf{
			LocalID: localID,
			Fence:   fence,
			Ref: PageRef{
				Offset: uint64(rank+1) * 24,
				LogicalID: segmentedTabletRouterLabTestLeafLogicalID(
					t, BucketID(bucket),
				),
				Generation: uint64(1000 + rank),
				Length:     length,
				Kind:       header.LeafKind,
			},
			Zone: zone,
		}
	}
	pageCount := (leafCount + SegmentedTabletRouterLabRowsPerPage - 1) /
		SegmentedTabletRouterLabRowsPerPage
	anchorRefs := make([]PageRef, pageCount)
	for pageID := range anchorRefs {
		anchorRefs[pageID] = PageRef{
			Offset: uint64(pageID+1) * SegmentedTabletRouterLabAnchorPageBytes,
			LogicalID: segmentedTabletRouterLabTestAnchorLogicalID(
				t, tabletID, uint8(pageID),
			),
			Generation: header.Generation,
			Length:     SegmentedTabletRouterLabAnchorPageBytes,
			Kind:       header.AnchorKind,
		}
	}
	return header, leaves, anchorRefs
}

func segmentedTabletRouterLabTestView(
	t testing.TB, leafCount int,
) (
	SegmentedTabletRouterLabView,
	[]SegmentedTabletRouterLabLeaf,
	[]PageRef,
) {
	t.Helper()
	header, leaves, refs := segmentedTabletRouterLabTestInputs(t, leafCount)
	pageCount := (leafCount + SegmentedTabletRouterLabRowsPerPage - 1) /
		SegmentedTabletRouterLabRowsPerPage
	root, locator, pages, gotPageCount, err :=
		EncodeSegmentedTabletRouterLab(
			make([]byte, SegmentedTabletRouterLabRootBytes),
			make([]byte, SegmentedTabletRouterLabLocatorBytes),
			make([]byte, pageCount*SegmentedTabletRouterLabAnchorPageBytes),
			header, refs, leaves,
		)
	if err != nil {
		t.Fatal(err)
	}
	if gotPageCount != pageCount {
		t.Fatalf("pageCount = %d, want %d", gotPageCount, pageCount)
	}
	view, err := OpenSegmentedTabletRouterLab(root, locator, pages)
	if err != nil {
		t.Fatal(err)
	}
	return view, leaves, refs
}

func TestSegmentedTabletRouterLabRouteResolveAndOrderedCursor(
	t *testing.T,
) {
	view, leaves, _ := segmentedTabletRouterLabTestView(t, 3072)
	for _, rank := range []int{0, 1, 255, 256, 1536, 3071} {
		key := leaves[rank].Fence
		hash := KeyHashBytes(segmentedTabletRouterLabTestSeed, key)
		route := view.RouteHashed(hash, key)
		if route.Bucket != segmentedTabletRouterLabTestBucket(leaves[rank].Ref) ||
			route.Hash != hash ||
			int(route.PageID) != rank/SegmentedTabletRouterLabRowsPerPage ||
			int(route.RowSlot) != rank%SegmentedTabletRouterLabRowsPerPage ||
			route.Ref != leaves[rank].Ref ||
			route.Zone != leaves[rank].Zone {
			t.Fatalf("route rank %d = %+v", rank, route)
		}
		included := view.Route(segmentedTabletRouterLabTestSeed, key)
		if included != route {
			t.Fatalf("included route rank %d = %+v, want %+v",
				rank, included, route)
		}
		ref, zone, ok := view.ResolveBucketID(route.Bucket)
		if !ok || ref != leaves[rank].Ref || zone != leaves[rank].Zone {
			t.Fatalf("resolve rank %d = %+v/%x/%v", rank, ref, zone, ok)
		}
	}
	foreign, _ := MakeTabletLocalIdentityLabBucket(43, 17)
	if _, _, ok := view.ResolveBucketID(BucketID(foreign)); ok {
		t.Fatal("foreign bucket resolved")
	}
	missingLocalID := uint16(0)
	for binary.LittleEndian.Uint16(
		view.locator[int(missingLocalID)*2:],
	) != segmentedTabletRouterLabEmpty {
		missingLocalID++
	}
	missing, _ := MakeTabletLocalIdentityLabBucket(
		42, uint32(missingLocalID),
	)
	if _, _, ok := view.ResolveBucketID(BucketID(missing)); ok {
		t.Fatal("unused bucket resolved")
	}

	cursor := view.LowerBound(leaves[253].Fence)
	for rank := 253; rank < len(leaves); rank++ {
		route, ok := cursor.Route(uint64(rank))
		if !ok || route.Bucket != segmentedTabletRouterLabTestBucket(leaves[rank].Ref) ||
			route.Hash != uint64(rank) {
			t.Fatalf("cursor rank %d = %+v,%v", rank, route, ok)
		}
		if rank+1 < len(leaves) {
			if !cursor.Next() {
				t.Fatalf("cursor stopped at rank %d", rank)
			}
		} else if cursor.Next() {
			t.Fatal("cursor advanced past end")
		}
	}
}

func TestSegmentedTabletRouterLabUsesExactCommonAnchorEnvelope(t *testing.T) {
	header, leaves, refs := segmentedTabletRouterLabTestInputs(
		t, SegmentedTabletRouterLabRowsPerPage,
	)
	root, locator, pages, pageCount, err := EncodeSegmentedTabletRouterLab(
		make([]byte, SegmentedTabletRouterLabRootBytes),
		make([]byte, SegmentedTabletRouterLabLocatorBytes),
		make([]byte, SegmentedTabletRouterLabAnchorPageBytes),
		header, refs, leaves,
	)
	if err != nil {
		t.Fatal(err)
	}
	if pageCount != 1 {
		t.Fatalf("page count = %d, want 1", pageCount)
	}
	pageHeader, payload, err := OpenPage(pages)
	if err != nil {
		t.Fatal(err)
	}
	if pageHeader.StoreID != header.StoreID ||
		pageHeader.Generation != refs[0].Generation ||
		pageHeader.LogicalID != refs[0].LogicalID ||
		pageHeader.PageSize != SegmentedTabletRouterLabAnchorPageBytes ||
		pageHeader.PayloadLength != segmentedTabletRouterLabAnchorPayloadBytes ||
		pageHeader.Kind != PagePrimaryAnchor ||
		pageHeader.Flags != 0 ||
		len(payload) != segmentedTabletRouterLabAnchorPayloadBytes {
		t.Fatalf("common anchor header = %+v, payload=%d", pageHeader, len(payload))
	}
	if got := binary.LittleEndian.Uint16(payload[0:2]); got !=
		SegmentedTabletRouterLabRowsPerPage {
		t.Fatalf("anchor count = %d", got)
	}
	if !allZero(payload[6:segmentedTabletRouterLabAnchorPayloadHeaderBytes]) {
		t.Fatal("non-zero anchor payload reserved bytes")
	}
	if _, err := OpenSegmentedTabletRouterLab(root, locator, pages); err != nil {
		t.Fatalf("open full 256-row anchor: %v", err)
	}
}

func TestSegmentedTabletRouterLabLogicalIDNamespace(t *testing.T) {
	if SegmentedTabletRouterLabStateRootLogicalID != 1 ||
		SegmentedTabletRouterLabLeafLogicalIDBase != 2 ||
		SegmentedTabletRouterLabLeafLogicalIDLimit !=
			SegmentedTabletRouterLabAnchorLogicalIDBase ||
		SegmentedTabletRouterLabAnchorLogicalIDLimit !=
			PrimaryAnchorLogicalIDLimit ||
		SegmentedTabletRouterLabFirstDynamicLogicalID !=
			PrimaryFirstDynamicLogicalID ||
		SegmentedTabletRouterLabAnchorLogicalIDLimit >=
			SegmentedTabletRouterLabFirstDynamicLogicalID {
		t.Fatalf(
			"namespace boundaries state=%d leaf=[%d,%d) anchor=[%d,%d) dynamic=%d",
			SegmentedTabletRouterLabStateRootLogicalID,
			SegmentedTabletRouterLabLeafLogicalIDBase,
			SegmentedTabletRouterLabLeafLogicalIDLimit,
			SegmentedTabletRouterLabAnchorLogicalIDBase,
			SegmentedTabletRouterLabAnchorLogicalIDLimit,
			SegmentedTabletRouterLabFirstDynamicLogicalID,
		)
	}
	leafZero, ok := SegmentedTabletRouterLabLeafLogicalID(0)
	if !ok || leafZero != SegmentedTabletRouterLabLeafLogicalIDBase {
		t.Fatalf("bucket zero logical ID = %d,%v", leafZero, ok)
	}
	leafMax, ok := SegmentedTabletRouterLabLeafLogicalID(
		BucketID(PrimaryBucketIDLimit - 1),
	)
	if !ok || leafMax != SegmentedTabletRouterLabLeafLogicalIDLimit-1 {
		t.Fatalf("max bucket logical ID = %d,%v", leafMax, ok)
	}
	if _, ok := SegmentedTabletRouterLabLeafLogicalID(
		BucketID(PrimaryBucketIDLimit),
	); ok {
		t.Fatal("out-of-range BucketID accepted")
	}
	anchorZero, ok := SegmentedTabletRouterLabAnchorLogicalID(0, 0)
	if !ok || anchorZero != SegmentedTabletRouterLabAnchorLogicalIDBase {
		t.Fatalf("first anchor logical ID = %d,%v", anchorZero, ok)
	}
	anchorMax, ok := SegmentedTabletRouterLabAnchorLogicalID(
		TabletLocalIdentityLabTabletCount-1,
		SegmentedTabletRouterLabMaxPages-1,
	)
	if !ok || anchorMax != SegmentedTabletRouterLabAnchorLogicalIDLimit-1 {
		t.Fatalf("last anchor logical ID = %d,%v", anchorMax, ok)
	}
	if _, ok := SegmentedTabletRouterLabAnchorLogicalID(
		TabletLocalIdentityLabTabletCount, 0,
	); ok {
		t.Fatal("out-of-range tablet accepted")
	}
	if _, ok := SegmentedTabletRouterLabAnchorLogicalID(
		0, SegmentedTabletRouterLabMaxPages,
	); ok {
		t.Fatal("out-of-range anchor page accepted")
	}
	ids := []uint64{
		SegmentedTabletRouterLabStateRootLogicalID,
		leafZero, leafMax, anchorZero, anchorMax,
		SegmentedTabletRouterLabFirstDynamicLogicalID,
	}
	for left := range ids {
		for right := left + 1; right < len(ids); right++ {
			if ids[left] == ids[right] {
				t.Fatalf("namespace collision at logical ID %d", ids[left])
			}
		}
	}
	if SegmentedTabletRouterLabIsDynamicLogicalID(
		SegmentedTabletRouterLabFirstDynamicLogicalID-1,
	) || !SegmentedTabletRouterLabIsDynamicLogicalID(
		SegmentedTabletRouterLabFirstDynamicLogicalID,
	) {
		t.Fatal("dynamic logical-ID boundary is not exact")
	}
}

func TestSegmentedTabletRouterLabRejectsCollidingLogicalIDs(t *testing.T) {
	header, leaves, refs := segmentedTabletRouterLabTestInputs(t, 1)
	validLeafID := leaves[0].Ref.LogicalID
	validAnchorID := refs[0].LogicalID
	encode := func() error {
		_, _, _, _, err := EncodeSegmentedTabletRouterLab(
			make([]byte, SegmentedTabletRouterLabRootBytes),
			make([]byte, SegmentedTabletRouterLabLocatorBytes),
			make([]byte, SegmentedTabletRouterLabAnchorPageBytes),
			header, refs, leaves,
		)
		return err
	}
	bucket := segmentedTabletRouterLabTestBucket(leaves[0].Ref)
	for name, logicalID := range map[string]uint64{
		"state-root-as-leaf":     SegmentedTabletRouterLabStateRootLogicalID,
		"legacy-bucket-plus-one": uint64(bucket) + 1,
		"anchor-range-as-leaf":   SegmentedTabletRouterLabAnchorLogicalIDBase,
	} {
		t.Run(name, func(t *testing.T) {
			leaves[0].Ref.LogicalID = logicalID
			if err := encode(); err == nil {
				t.Fatalf("leaf logical ID %d accepted", logicalID)
			}
			leaves[0].Ref.LogicalID = validLeafID
		})
	}
	for name, logicalID := range map[string]uint64{
		"state-root-as-anchor": SegmentedTabletRouterLabStateRootLogicalID,
		"legacy-tablet-page": uint64(header.TabletID)*
			SegmentedTabletRouterLabMaxPages + 1,
		"leaf-range-as-anchor": SegmentedTabletRouterLabLeafLogicalIDBase,
	} {
		t.Run(name, func(t *testing.T) {
			refs[0].LogicalID = logicalID
			if err := encode(); err == nil {
				t.Fatalf("anchor logical ID %d accepted", logicalID)
			}
			refs[0].LogicalID = validAnchorID
		})
	}
}

func TestSegmentedTabletRouterLabRandomizedRouting(t *testing.T) {
	view, leaves, _ := segmentedTabletRouterLabTestView(t, 3072)
	random := rand.New(rand.NewSource(7))
	for iteration := 0; iteration < 20_000; iteration++ {
		value := random.Intn(3072*230 + 230)
		key := []byte(fmt.Sprintf(
			"tenant/0042/document/%010d", value,
		))
		want := sort.Search(len(leaves), func(rank int) bool {
			return bytes.Compare(leaves[rank].Fence, key) > 0
		}) - 1
		if want < 0 {
			want = 0
		}
		got := view.RouteHashed(123, key)
		if got.Bucket != segmentedTabletRouterLabTestBucket(leaves[want].Ref) {
			t.Fatalf(
				"iteration %d route(%q) = %d, want rank %d bucket %d",
				iteration, key, got.Bucket, want,
				segmentedTabletRouterLabTestBucket(leaves[want].Ref),
			)
		}
	}
}

func TestSegmentedTabletRouterLabBinaryShortAndTiedHeads(t *testing.T) {
	header := SegmentedTabletRouterLabHeader{
		StoreID:  segmentedTabletRouterLabTestSeed,
		TabletID: 7, Generation: 100,
		AnchorKind: PagePrimaryAnchor, LeafKind: PagePrimaryLeaf,
	}
	fences := [][]byte{
		nil,
		{0},
		{0, 0},
		{0, 0, 0},
		{0, 0, 0, 0},
		{0, 0, 0, 0, 0},
		{0, 0, 0, 0, 1},
		{0, 0, 0, 1},
		{0, 0, 1},
		{0, 1},
		{1},
	}
	leaves := make([]SegmentedTabletRouterLabLeaf, len(fences))
	for rank, fence := range fences {
		bucket, _ := MakeTabletLocalIdentityLabBucket(
			header.TabletID, uint32(rank),
		)
		leaves[rank] = SegmentedTabletRouterLabLeaf{
			LocalID: uint16(rank), Fence: fence,
			Ref: PageRef{
				Offset: uint64(rank+1) * 8,
				LogicalID: segmentedTabletRouterLabTestLeafLogicalID(
					t, BucketID(bucket),
				),
				Generation: uint64(rank) + 1,
				Length:     uint32(rank) + 1,
				Kind:       header.LeafKind,
			},
		}
	}
	anchorRef := PageRef{
		Offset: SegmentedTabletRouterLabAnchorPageBytes,
		LogicalID: segmentedTabletRouterLabAnchorLogicalID(
			header.TabletID, 0,
		),
		Generation: header.Generation,
		Length:     SegmentedTabletRouterLabAnchorPageBytes,
		Kind:       header.AnchorKind,
	}
	root, locator, pages, _, err := EncodeSegmentedTabletRouterLab(
		make([]byte, SegmentedTabletRouterLabRootBytes),
		make([]byte, SegmentedTabletRouterLabLocatorBytes),
		make([]byte, SegmentedTabletRouterLabAnchorPageBytes),
		header, []PageRef{anchorRef}, leaves,
	)
	if err != nil {
		t.Fatal(err)
	}
	view, err := OpenSegmentedTabletRouterLab(root, locator, pages)
	if err != nil {
		t.Fatal(err)
	}
	if view.pages[0].headBytes != 4 {
		t.Fatalf("head width = %d, want 4", view.pages[0].headBytes)
	}
	queries := append([][]byte(nil), fences...)
	queries = append(queries,
		[]byte{0, 0, 0, 0, 0, 1},
		[]byte{0, 0, 0, 0, 2},
		[]byte{0, 0, 2},
		[]byte{2},
	)
	for _, key := range queries {
		want := sort.Search(len(fences), func(rank int) bool {
			return bytes.Compare(fences[rank], key) > 0
		}) - 1
		if want < 0 {
			want = 0
		}
		route := view.RouteHashed(9, key)
		if route.Bucket !=
			segmentedTabletRouterLabTestBucket(leaves[want].Ref) {
			t.Fatalf("route(%v) = %d, want rank %d bucket %d",
				key, route.Bucket, want,
				segmentedTabletRouterLabTestBucket(leaves[want].Ref))
		}
	}
}

func TestSegmentedTabletRouterLabExactLengthHandle(t *testing.T) {
	view, leaves, _ := segmentedTabletRouterLabTestView(t, 1024)
	for _, rank := range []int{0, 1, 127, 511, 1023} {
		bucket := segmentedTabletRouterLabTestBucket(leaves[rank].Ref)
		ref, _, ok := view.ResolveBucketID(bucket)
		if !ok || ref.Offset != leaves[rank].Ref.Offset ||
			ref.Length != leaves[rank].Ref.Length {
			t.Fatalf("exact handle rank %d = %+v,%v, want %+v",
				rank, ref, ok, leaves[rank].Ref)
		}
	}

	header, one, refs := segmentedTabletRouterLabTestInputs(t, 1)
	one[0].Ref.Offset = (uint64(1)<<48 - 1) << 3
	one[0].Ref.Length = 65536
	root, locator, pages, _, err := EncodeSegmentedTabletRouterLab(
		make([]byte, SegmentedTabletRouterLabRootBytes),
		make([]byte, SegmentedTabletRouterLabLocatorBytes),
		make([]byte, SegmentedTabletRouterLabAnchorPageBytes),
		header, refs, one,
	)
	if err != nil {
		t.Fatal(err)
	}
	maxView, err := OpenSegmentedTabletRouterLab(root, locator, pages)
	if err != nil {
		t.Fatal(err)
	}
	ref, _, ok := maxView.ResolveBucketID(
		segmentedTabletRouterLabTestBucket(one[0].Ref),
	)
	if !ok || ref.Offset != one[0].Ref.Offset || ref.Length != 65536 {
		t.Fatalf("maximum exact handle = %+v,%v", ref, ok)
	}
}

func TestSegmentedTabletRouterLabOrdinaryCOWIsLocalized(t *testing.T) {
	view, leaves, refs := segmentedTabletRouterLabTestView(t, 3072)
	target := 1537
	bucket := segmentedTabletRouterLabTestBucket(leaves[target].Ref)
	oldLocator := append([]byte(nil), view.locator...)
	oldPages := make([]byte, len(view.pages[0].image)*int(view.pageCount))
	for pageID := 0; pageID < int(view.pageCount); pageID++ {
		copy(oldPages[pageID*SegmentedTabletRouterLabAnchorPageBytes:],
			view.pages[pageID].image)
	}
	newLeaf := leaves[target].Ref
	newLeaf.Offset += 8
	newLeaf.Generation = 10_001
	newLeaf.Length = 317
	var newZone BucketZone
	copy(newZone[:], []byte{9, 8, 7, 6})
	pageID := uint8(target / SegmentedTabletRouterLabRowsPerPage)
	newAnchor := refs[pageID]
	newAnchor.Offset += 1 << 20
	newAnchor.Generation = 10_001
	result, err := view.RewriteHandle(
		make([]byte, SegmentedTabletRouterLabRootBytes),
		make([]byte, SegmentedTabletRouterLabAnchorPageBytes),
		10_001, bucket, newLeaf, newZone, newAnchor,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Bytes != SegmentedTabletRouterLabRootBytes+
		SegmentedTabletRouterLabAnchorPageBytes ||
		result.PageID != pageID ||
		!bytes.Equal(view.locator, oldLocator) {
		t.Fatalf("ordinary COW result = %+v, locator changed=%v",
			result, !bytes.Equal(view.locator, oldLocator))
	}
	nextPages := append([]byte(nil), oldPages...)
	copy(nextPages[int(pageID)*SegmentedTabletRouterLabAnchorPageBytes:],
		result.AnchorPage)
	next, err := OpenSegmentedTabletRouterLab(
		result.Root, view.locator, nextPages,
	)
	if err != nil {
		t.Fatal(err)
	}
	gotRef, gotZone, ok := next.ResolveBucketID(bucket)
	if !ok || gotRef != newLeaf || gotZone != newZone {
		t.Fatalf("rewritten handle = %+v/%x/%v", gotRef, gotZone, ok)
	}
	other := target - 1
	otherRef, otherZone, ok := next.ResolveBucketID(
		segmentedTabletRouterLabTestBucket(leaves[other].Ref),
	)
	if !ok || otherRef != leaves[other].Ref ||
		otherZone != leaves[other].Zone {
		t.Fatalf("neighbor changed = %+v/%x/%v", otherRef, otherZone, ok)
	}
}

func TestSegmentedTabletRouterLabStructuralSplitIsAtomic(t *testing.T) {
	view, leaves, refs := segmentedTabletRouterLabTestView(t, 3072)
	const sourcePageID = uint8(5)
	newPageID := view.pageCount
	leftRef := refs[sourcePageID]
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
	result, err := view.SplitAnchorPage(
		make([]byte, SegmentedTabletRouterLabRootBytes),
		make([]byte, SegmentedTabletRouterLabLocatorBytes),
		make([]byte, SegmentedTabletRouterLabAnchorPageBytes),
		make([]byte, SegmentedTabletRouterLabAnchorPageBytes),
		10_001, sourcePageID, newPageID, 128, leftRef, rightRef,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Bytes != SegmentedTabletRouterLabRootBytes+
		SegmentedTabletRouterLabLocatorBytes+
		2*SegmentedTabletRouterLabAnchorPageBytes {
		t.Fatalf("split bytes = %d", result.Bytes)
	}
	nextPages := make([]byte,
		(int(view.pageCount)+1)*SegmentedTabletRouterLabAnchorPageBytes)
	for pageID := 0; pageID < int(view.pageCount); pageID++ {
		copy(nextPages[pageID*SegmentedTabletRouterLabAnchorPageBytes:],
			view.pages[pageID].image)
	}
	copy(nextPages[int(sourcePageID)*SegmentedTabletRouterLabAnchorPageBytes:],
		result.LeftPage)
	copy(nextPages[int(newPageID)*SegmentedTabletRouterLabAnchorPageBytes:],
		result.RightPage)
	next, err := OpenSegmentedTabletRouterLab(
		result.Root, result.Locator, nextPages,
	)
	if err != nil {
		t.Fatal(err)
	}
	first := int(sourcePageID) * SegmentedTabletRouterLabRowsPerPage
	for _, rank := range []int{first, first + 127, first + 128, first + 255} {
		route := next.RouteHashed(7, leaves[rank].Fence)
		wantPage := sourcePageID
		if rank >= first+128 {
			wantPage = newPageID
		}
		if route.PageID != wantPage ||
			route.Bucket != segmentedTabletRouterLabTestBucket(leaves[rank].Ref) ||
			route.RowSlot != uint8(rank-first) {
			t.Fatalf("split route rank %d = %+v, want page %d",
				rank, route, wantPage)
		}
		ref, zone, ok := next.ResolveBucketID(route.Bucket)
		if !ok || ref != leaves[rank].Ref || zone != leaves[rank].Zone {
			t.Fatalf("split resolve rank %d = %+v/%x/%v",
				rank, ref, zone, ok)
		}
	}
}

func TestSegmentedTabletRouterLabReadPathsAllocateNothing(t *testing.T) {
	view, leaves, _ := segmentedTabletRouterLabTestView(t, 3072)
	key := leaves[1536].Fence
	hash := KeyHashBytes(segmentedTabletRouterLabTestSeed, key)
	bucket := segmentedTabletRouterLabTestBucket(leaves[1536].Ref)
	pages := make([]byte, int(view.pageCount)*
		SegmentedTabletRouterLabAnchorPageBytes)
	for pageID := 0; pageID < int(view.pageCount); pageID++ {
		copy(
			pages[pageID*SegmentedTabletRouterLabAnchorPageBytes:],
			view.pages[pageID].image,
		)
	}
	if got := testing.AllocsPerRun(1000, func() {
		_, _ = OpenSegmentedTabletRouterLab(view.root, view.locator, pages)
	}); got != 0 {
		t.Fatalf("Open allocations = %v", got)
	}
	if got := testing.AllocsPerRun(1000, func() {
		_ = view.RouteHashed(hash, key)
	}); got != 0 {
		t.Fatalf("RouteHashed allocations = %v", got)
	}
	if got := testing.AllocsPerRun(1000, func() {
		_, _, _ = view.ResolveBucketID(bucket)
	}); got != 0 {
		t.Fatalf("ResolveBucketID allocations = %v", got)
	}
	if got := testing.AllocsPerRun(1000, func() {
		cursor := view.LowerBound(key)
		_, _ = cursor.Route(hash)
		_ = cursor.Next()
	}); got != 0 {
		t.Fatalf("cursor allocations = %v", got)
	}
}

func TestSegmentedTabletRouterLabSpaceProjection(t *testing.T) {
	const leafCount = TabletLocalIdentityLabLocalCount
	view, _, _ := segmentedTabletRouterLabTestView(t, leafCount)
	if view.pageCount != SegmentedTabletRouterLabMaxPages {
		t.Fatalf("full tablet pages = %d", view.pageCount)
	}
	const persistentBytes = SegmentedTabletRouterLabRootBytes +
		SegmentedTabletRouterLabLocatorBytes +
		SegmentedTabletRouterLabMaxPages*
			SegmentedTabletRouterLabAnchorPageBytes
	if persistentBytes != 143360 ||
		float64(persistentBytes)/leafCount != 35 {
		t.Fatalf("full tablet = %d bytes, %.3f B/leaf",
			persistentBytes, float64(persistentBytes)/leafCount)
	}
	wants := map[int]float64{
		187: float64(persistentBytes) / float64(leafCount*187),
		195: float64(persistentBytes) / float64(leafCount*195),
		230: float64(persistentBytes) / float64(leafCount*230),
	}
	for rows, want := range wants {
		got := SegmentedTabletRouterLabRoutingBytesPerDocument(
			leafCount, rows,
		)
		if got != want || got >= 0.19 {
			t.Fatalf("%d rows = %.9f, want %.9f", rows, got, want)
		}
	}
}

func TestSegmentedTabletRouterLabRejectsCorruption(t *testing.T) {
	view, _, _ := segmentedTabletRouterLabTestView(t, 1024)
	root := append([]byte(nil), view.root...)
	locator := append([]byte(nil), view.locator...)
	pages := make([]byte, int(view.pageCount)*
		SegmentedTabletRouterLabAnchorPageBytes)
	for pageID := 0; pageID < int(view.pageCount); pageID++ {
		copy(pages[pageID*SegmentedTabletRouterLabAnchorPageBytes:],
			view.pages[pageID].image)
	}
	tests := []struct {
		name     string
		edit     func(root, locator, pages []byte)
		sealRoot bool
		sealPage int
	}{
		{
			name:     "root-checksum",
			edit:     func(root, _, _ []byte) { root[100] ^= 1 },
			sealPage: -1,
		},
		{
			name: "legacy-root-version",
			edit: func(root, _, _ []byte) {
				binary.LittleEndian.PutUint32(root[8:12], 2)
			},
			sealRoot: true, sealPage: -1,
		},
		{
			name: "root-store-id",
			edit: func(root, _, _ []byte) {
				root[44] ^= 0xff
			},
			sealRoot: true, sealPage: -1,
		},
		{
			name: "root-anchor-kind",
			edit: func(root, _, _ []byte) {
				root[15] = byte(PageTabletRoute)
			},
			sealRoot: true, sealPage: -1,
		},
		{
			name: "root-leaf-kind",
			edit: func(root, _, _ []byte) {
				root[16] = byte(PageDocument)
			},
			sealRoot: true, sealPage: -1,
		},
		{
			name: "root-generation-overflow",
			edit: func(root, _, _ []byte) {
				binary.LittleEndian.PutUint64(
					root[24:32], uint64(1)<<48,
				)
			},
			sealRoot: true, sealPage: -1,
		},
		{
			name: "future-anchor-generation",
			edit: func(root, _, _ []byte) {
				segmentedTabletRouterLabPutUint48(
					root[segmentedTabletRouterLabRootRefsAt+6:],
					view.generation+1,
				)
			},
			sealRoot: true, sealPage: -1,
		},
		{
			name:     "locator-binding",
			edit:     func(_, locator, _ []byte) { locator[0] ^= 1 },
			sealPage: -1,
		},
		{
			name: "duplicate-page-rank",
			edit: func(root, _, _ []byte) {
				root[segmentedTabletRouterLabRootRanksAt+1] =
					root[segmentedTabletRouterLabRootRanksAt]
			},
			sealRoot: true, sealPage: -1,
		},
		{
			name:     "anchor-checksum",
			edit:     func(_, _, pages []byte) { pages[100] ^= 1 },
			sealPage: -1,
		},
		{
			name: "legacy-common-anchor-version",
			edit: func(_, _, pages []byte) {
				binary.LittleEndian.PutUint16(pages[8:10], pageVersion-1)
			},
			sealPage: 0,
		},
		{
			name: "legacy-private-anchor-envelope",
			edit: func(_, _, pages []byte) {
				copy(pages[:8], "STRPAGE1")
			},
			sealPage: 0,
		},
		{
			name: "wrong-common-anchor-kind",
			edit: func(_, _, pages []byte) {
				pages[12] = byte(PageTabletRoute)
			},
			sealPage: 0,
		},
		{
			name: "cross-store-anchor",
			edit: func(_, _, pages []byte) {
				pages[40] ^= 0xff
			},
			sealPage: 0,
		},
		{
			name: "future-leaf-generation",
			edit: func(_, _, pages []byte) {
				slot := pages[segmentedTabletRouterLabAnchorRanksAt]
				segmentedTabletRouterLabPutUint48(
					pages[segmentedTabletRouterLabAnchorHandlesAt+
						int(slot)*SegmentedTabletRouterLabHandleBytes+6:],
					view.pages[0].generation+1,
				)
			},
			sealPage: 0,
		},
		{
			name: "duplicate-row-rank",
			edit: func(_, _, pages []byte) {
				pages[segmentedTabletRouterLabAnchorRanksAt+1] =
					pages[segmentedTabletRouterLabAnchorRanksAt]
			},
			sealPage: 0,
		},
		{
			name: "bad-compressed-offset",
			edit: func(_, _, pages []byte) {
				binary.LittleEndian.PutUint16(
					pages[segmentedTabletRouterLabAnchorOffsetsAt+2:],
					65535,
				)
			},
			sealPage: 0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			nextRoot := append([]byte(nil), root...)
			nextLocator := append([]byte(nil), locator...)
			nextPages := append([]byte(nil), pages...)
			test.edit(nextRoot, nextLocator, nextPages)
			if test.sealRoot {
				segmentedTabletRouterLabSeal(
					nextRoot, segmentedTabletRouterLabRootTrailerAt,
				)
			}
			if test.sealPage >= 0 {
				image := nextPages[test.sealPage*
					SegmentedTabletRouterLabAnchorPageBytes:]
				segmentedTabletRouterLabSeal(
					image[:SegmentedTabletRouterLabAnchorPageBytes],
					segmentedTabletRouterLabAnchorTrailerAt,
				)
			}
			if _, err := OpenSegmentedTabletRouterLab(
				nextRoot, nextLocator, nextPages,
			); !errors.Is(err, ErrSegmentedTabletRouterLabCorrupt) {
				t.Fatalf("Open error = %v", err)
			}
		})
	}
}

func TestSegmentedTabletRouterLabGenerationBounds(t *testing.T) {
	header, leaves, refs := segmentedTabletRouterLabTestInputs(t, 1)
	encode := func() ([]byte, []byte, []byte, error) {
		root, locator, pages, _, err := EncodeSegmentedTabletRouterLab(
			make([]byte, SegmentedTabletRouterLabRootBytes),
			make([]byte, SegmentedTabletRouterLabLocatorBytes),
			make([]byte, SegmentedTabletRouterLabAnchorPageBytes),
			header, refs, leaves,
		)
		return root, locator, pages, err
	}

	t.Run("future-leaf-encode", func(t *testing.T) {
		old := leaves[0].Ref.Generation
		leaves[0].Ref.Generation = header.Generation + 1
		if _, _, _, err := encode(); err == nil {
			t.Fatal("future leaf generation accepted")
		}
		leaves[0].Ref.Generation = old
	})
	t.Run("future-anchor-encode", func(t *testing.T) {
		refs[0].Generation = header.Generation + 1
		if _, _, _, err := encode(); err == nil {
			t.Fatal("future anchor generation accepted")
		}
		refs[0].Generation = header.Generation
	})
	t.Run("maximum-generation", func(t *testing.T) {
		const maximum = uint64(1)<<48 - 1
		header.Generation = maximum
		refs[0].Generation = maximum
		root, locator, pages, err := encode()
		if err != nil {
			t.Fatal(err)
		}
		view, err := OpenSegmentedTabletRouterLab(root, locator, pages)
		if err != nil || view.generation != maximum {
			t.Fatalf("open maximum generation = %d,%v",
				view.generation, err)
		}
	})
	t.Run("overflow-generation", func(t *testing.T) {
		header.Generation = uint64(1) << 48
		refs[0].Generation = uint64(1)<<48 - 1
		if _, _, _, err := encode(); err == nil {
			t.Fatal("48-bit-overflow root generation accepted")
		}
	})

	view, currentLeaves, currentRefs :=
		segmentedTabletRouterLabTestView(t, 1)
	bucket := segmentedTabletRouterLabTestBucket(currentLeaves[0].Ref)
	const maximum = uint64(1)<<48 - 1
	leafRef := currentLeaves[0].Ref
	leafRef.Generation = maximum
	anchorRef := currentRefs[0]
	anchorRef.Generation = maximum
	if _, err := view.RewriteHandle(
		make([]byte, SegmentedTabletRouterLabRootBytes),
		make([]byte, SegmentedTabletRouterLabAnchorPageBytes),
		maximum, bucket, leafRef, BucketZone{}, anchorRef,
	); err != nil {
		t.Fatalf("maximum-generation COW: %v", err)
	}
	leafRef.Generation = uint64(1) << 48
	anchorRef.Generation = uint64(1) << 48
	if _, err := view.RewriteHandle(
		make([]byte, SegmentedTabletRouterLabRootBytes),
		make([]byte, SegmentedTabletRouterLabAnchorPageBytes),
		uint64(1)<<48, bucket, leafRef, BucketZone{}, anchorRef,
	); err == nil {
		t.Fatal("48-bit-overflow COW generation accepted")
	}
}

func TestSegmentedTabletRouterLabRejectsInvalidWrites(t *testing.T) {
	header, leaves, refs := segmentedTabletRouterLabTestInputs(t, 256)
	encode := func() error {
		_, _, _, _, err := EncodeSegmentedTabletRouterLab(
			make([]byte, SegmentedTabletRouterLabRootBytes),
			make([]byte, SegmentedTabletRouterLabLocatorBytes),
			make([]byte, SegmentedTabletRouterLabAnchorPageBytes),
			header, refs, leaves,
		)
		return err
	}
	t.Run("duplicate-local-id", func(t *testing.T) {
		old := leaves[1].LocalID
		leaves[1].LocalID = leaves[0].LocalID
		leaves[1].Ref.LogicalID = leaves[0].Ref.LogicalID
		if err := encode(); err == nil {
			t.Fatal("duplicate LocalID accepted")
		}
		leaves[1].LocalID = old
		bucket, _ := MakeTabletLocalIdentityLabBucket(
			header.TabletID, uint32(old),
		)
		leaves[1].Ref.LogicalID =
			segmentedTabletRouterLabTestLeafLogicalID(t, BucketID(bucket))
	})
	t.Run("unaligned-cold-offset", func(t *testing.T) {
		leaves[1].Ref.Offset++
		if err := encode(); err == nil {
			t.Fatal("unaligned leaf accepted")
		}
		leaves[1].Ref.Offset--
	})
	t.Run("zero-length", func(t *testing.T) {
		old := leaves[1].Ref.Length
		leaves[1].Ref.Length = 0
		if err := encode(); err == nil {
			t.Fatal("zero length accepted")
		}
		leaves[1].Ref.Length = old
	})
	t.Run("zero-store-id", func(t *testing.T) {
		old := header.StoreID
		header.StoreID = [16]byte{}
		if err := encode(); err == nil {
			t.Fatal("zero StoreID accepted")
		}
		header.StoreID = old
	})
	t.Run("wrong-anchor-kind", func(t *testing.T) {
		header.AnchorKind = PageTabletRoute
		refs[0].Kind = PageTabletRoute
		if err := encode(); err == nil {
			t.Fatal("non-primary anchor kind accepted")
		}
		header.AnchorKind = PagePrimaryAnchor
		refs[0].Kind = PagePrimaryAnchor
	})
	t.Run("wrong-leaf-kind", func(t *testing.T) {
		header.LeafKind = PageDocument
		for rank := range leaves {
			leaves[rank].Ref.Kind = PageDocument
		}
		if err := encode(); err == nil {
			t.Fatal("non-primary leaf kind accepted")
		}
		header.LeafKind = PagePrimaryLeaf
		for rank := range leaves {
			leaves[rank].Ref.Kind = PagePrimaryLeaf
		}
	})
}

func FuzzOpenSegmentedTabletRouterLab(f *testing.F) {
	view, _, _ := segmentedTabletRouterLabTestView(f, 32)
	pages := append([]byte(nil), view.pages[0].image...)
	f.Add(
		append([]byte(nil), view.root...),
		append([]byte(nil), view.locator...),
		pages,
	)
	f.Fuzz(func(t *testing.T, root, locator, pages []byte) {
		_, _ = OpenSegmentedTabletRouterLab(root, locator, pages)
	})
}
