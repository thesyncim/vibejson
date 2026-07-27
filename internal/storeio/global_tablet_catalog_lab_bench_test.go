package storeio

import (
	"encoding/binary"
	"fmt"
	"sort"
	"testing"
)

var (
	globalTabletCatalogLabBenchmarkNodeRoute GlobalTabletCatalogLabNodeRoute
	globalTabletCatalogLabBenchmarkRef       PageRef
	globalTabletCatalogLabBenchmarkState     GlobalTabletCatalogLabLocatorState
	globalTabletCatalogLabBenchmarkPageID    uint8
	globalTabletCatalogLabBenchmarkRowSlot   uint8
	globalTabletCatalogLabBenchmarkImage     []byte
	globalTabletCatalogLabBenchmarkCount     int
)

func globalTabletCatalogLabBenchmarkNode(
	b testing.TB, level, rootChild GlobalTabletCatalogLabNodeLevel, count int,
) GlobalTabletCatalogLabNodeView {
	return globalTabletCatalogLabBenchmarkNodeShape(
		b, level, rootChild, count, false,
	)
}

func globalTabletCatalogLabBenchmarkNodeShape(
	b testing.TB, level, rootChild GlobalTabletCatalogLabNodeLevel, count int,
	sharedHead bool,
) GlobalTabletCatalogLabNodeView {
	b.Helper()
	entries := make([]GlobalTabletCatalogLabNodeEntry, count)
	for at := range entries {
		var floor []byte
		if at != 0 {
			if sharedHead {
				floor = []byte(fmt.Sprintf("tenant/%08x", at))
			} else {
				floor = make([]byte, 4)
				binary.BigEndian.PutUint32(floor, uint32(at))
			}
		}
		id := uint32(at)
		childLogical, ok := globalTabletCatalogLabChildLogicalID(
			level,
			func() GlobalTabletCatalogLabNodeLevel {
				if level == GlobalTabletCatalogLabRoot {
					return rootChild
				}
				return GlobalTabletCatalogLabLeaf
			}(),
			id,
		)
		if !ok {
			b.Fatalf("child logical ID %d", at)
		}
		childKind := PageIndexDirectory
		childLength := uint32(GlobalTabletCatalogLabNodeBytes)
		if level == GlobalTabletCatalogLabLeaf {
			childKind = PageKeyDirectory
			childLength = GlobalTabletCatalogLabTabletBytes
		}
		entries[at] = GlobalTabletCatalogLabNodeEntry{
			Floor: floor, ID: id,
			Ref: globalTabletCatalogLabTestRef(
				uint64(at+1)*8192, childLogical, 100,
				childLength, childKind,
			),
		}
	}
	pageBytes, _ := globalTabletCatalogLabNodePageBytes(level)
	var logicalID uint64
	var pageID uint32
	switch level {
	case GlobalTabletCatalogLabLeaf:
		logicalID, _ = GlobalTabletCatalogLabCatalogLeafLogicalID(0)
	case GlobalTabletCatalogLabBranch:
		logicalID, _ = GlobalTabletCatalogLabCatalogBranchLogicalID(0)
	case GlobalTabletCatalogLabRoot:
		logicalID = GlobalTabletCatalogLabRootLogicalID
	}
	childKind := PageIndexDirectory
	childLength := uint32(GlobalTabletCatalogLabNodeBytes)
	if level == GlobalTabletCatalogLabLeaf {
		childKind = PageKeyDirectory
		childLength = GlobalTabletCatalogLabTabletBytes
	}
	image, err := EncodeGlobalTabletCatalogLabNode(
		make([]byte, pageBytes),
		GlobalTabletCatalogLabNodeHeader{
			StoreID:    globalTabletCatalogLabTestStoreID,
			Bounds:     globalTabletCatalogLabTestBounds,
			Generation: 100, LogicalID: logicalID, PageID: pageID,
			Level: level, RootChildLevel: rootChild,
			Kind: PageFloat64Catalog, ChildKind: childKind,
			ChildLength: childLength,
		},
		entries,
	)
	if err != nil {
		b.Fatal(err)
	}
	ref := globalTabletCatalogLabTestRef(
		1<<30, logicalID, 100, uint32(pageBytes), PageFloat64Catalog,
	)
	view, err := OpenGlobalTabletCatalogLabNode(
		image, ref, globalTabletCatalogLabTestBounds,
	)
	if err != nil {
		b.Fatal(err)
	}
	return view
}

func BenchmarkGlobalTabletCatalogLabResidentRoute(b *testing.B) {
	leaf := globalTabletCatalogLabBenchmarkNode(
		b, GlobalTabletCatalogLabLeaf, 0, 256,
	)
	root := globalTabletCatalogLabBenchmarkNode(
		b, GlobalTabletCatalogLabRoot, GlobalTabletCatalogLabLeaf, 256,
	)
	rootRare := globalTabletCatalogLabBenchmarkNode(
		b, GlobalTabletCatalogLabRoot, GlobalTabletCatalogLabBranch, 256,
	)
	branch := globalTabletCatalogLabBenchmarkNode(
		b, GlobalTabletCatalogLabBranch, 0, 256,
	)
	sharedHead := globalTabletCatalogLabBenchmarkNodeShape(
		b, GlobalTabletCatalogLabLeaf, 0, 256, true,
	)
	key := make([]byte, 4)
	binary.BigEndian.PutUint32(key, 128)
	b.Run("catalog-leaf", func(b *testing.B) {
		b.ReportAllocs()
		var route GlobalTabletCatalogLabNodeRoute
		for b.Loop() {
			route = leaf.Route(key)
		}
		globalTabletCatalogLabBenchmarkNodeRoute = route
	})
	b.Run("normal-two-level", func(b *testing.B) {
		b.ReportAllocs()
		var rootRoute, leafRoute GlobalTabletCatalogLabNodeRoute
		for b.Loop() {
			rootRoute = root.Route(key)
			leafRoute = leaf.Route(key)
		}
		leafRoute.ID ^= rootRoute.ID
		leafRoute.Ordinal ^= rootRoute.Ordinal
		globalTabletCatalogLabBenchmarkNodeRoute = leafRoute
	})
	b.Run("shared-head-leaf", func(b *testing.B) {
		key := []byte("tenant/00000080")
		b.ReportAllocs()
		var route GlobalTabletCatalogLabNodeRoute
		for b.Loop() {
			route = sharedHead.Route(key)
		}
		globalTabletCatalogLabBenchmarkNodeRoute = route
	})
	b.Run("rare-three-level", func(b *testing.B) {
		b.ReportAllocs()
		var rootRoute, branchRoute, leafRoute GlobalTabletCatalogLabNodeRoute
		for b.Loop() {
			rootRoute = rootRare.Route(key)
			branchRoute = branch.Route(key)
			leafRoute = leaf.Route(key)
		}
		leafRoute.ID ^= rootRoute.ID ^ branchRoute.ID
		leafRoute.Ordinal ^= rootRoute.Ordinal ^ branchRoute.Ordinal
		globalTabletCatalogLabBenchmarkNodeRoute = leafRoute
	})
	b.Run("ordered-256", func(b *testing.B) {
		b.ReportAllocs()
		var count int
		for b.Loop() {
			cursor := leaf.LowerBound(nil)
			count = 0
			for {
				if _, ok := cursor.Route(); !ok {
					break
				}
				count++
				if !cursor.Next() {
					break
				}
			}
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/
			float64(b.N*leaf.Count()), "ns/tablet")
		globalTabletCatalogLabBenchmarkCount = count
	})
}

func BenchmarkGlobalTabletCatalogLabCompactLocator(b *testing.B) {
	const tabletID = uint32(42)
	logicalID, _ := GlobalTabletCatalogLabLocatorLogicalID(tabletID)
	entries := make([]GlobalTabletCatalogLabLocatorEntry, TabletLocalIdentityLabLocalCount)
	for localID := range entries {
		entries[localID] = GlobalTabletCatalogLabLocatorEntry{
			LocalID: uint16(localID),
			PageID:  uint8(localID / SegmentedTabletRouterLabRowsPerPage),
			RowSlot: uint8(localID),
			State:   GlobalTabletCatalogLabLocatorLive,
		}
	}
	image, err := EncodeGlobalTabletCatalogLabLocator(
		make([]byte, GlobalTabletCatalogLabLocatorBytes),
		PageHeader{
			StoreID:    globalTabletCatalogLabTestStoreID,
			Generation: 100, LogicalID: logicalID,
			PageSize: GlobalTabletCatalogLabLocatorBytes,
			PayloadLength: GlobalTabletCatalogLabLocatorHeader +
				globalTabletCatalogLabPackedBytes,
			Kind: PageFingerprintDirectory,
		},
		globalTabletCatalogLabTestBounds,
		tabletID, 100, entries,
	)
	if err != nil {
		b.Fatal(err)
	}
	ref := globalTabletCatalogLabTestRef(
		1<<20, logicalID, 100,
		GlobalTabletCatalogLabLocatorBytes, PageFingerprintDirectory,
	)
	view, err := OpenGlobalTabletCatalogLabLocator(
		image, ref, globalTabletCatalogLabTestBounds,
	)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	var pageID, rowSlot uint8
	var state GlobalTabletCatalogLabLocatorState
	for b.Loop() {
		pageID, rowSlot, state = view.Resolve(3073)
	}
	globalTabletCatalogLabBenchmarkPageID = pageID
	globalTabletCatalogLabBenchmarkRowSlot = rowSlot
	globalTabletCatalogLabBenchmarkState = state
}

func BenchmarkGlobalTabletCatalogLabCatalogCOW(b *testing.B) {
	for _, test := range []struct {
		name  string
		level GlobalTabletCatalogLabNodeLevel
		child GlobalTabletCatalogLabNodeLevel
		count int
	}{
		{"leaf-8K", GlobalTabletCatalogLabLeaf, 0, 256},
		{"branch-8K", GlobalTabletCatalogLabBranch, 0, 256},
		{"root-64K", GlobalTabletCatalogLabRoot, GlobalTabletCatalogLabLeaf, 256},
	} {
		b.Run(test.name, func(b *testing.B) {
			view := globalTabletCatalogLabBenchmarkNode(
				b, test.level, test.child, test.count,
			)
			id := uint32(test.count / 2)
			key := make([]byte, 4)
			binary.BigEndian.PutUint32(key, id)
			route := view.Route(key)
			replacement := route.Ref
			replacement.Offset += 1 << 30
			replacement.Generation = 101
			dst := make([]byte, len(view.image))
			b.SetBytes(int64(len(dst)))
			b.ReportAllocs()
			var image []byte
			for b.Loop() {
				var err error
				image, err = view.RewriteHandle(
					dst, 101, globalTabletCatalogLabTestBounds,
					id, replacement,
				)
				if err != nil {
					b.Fatal(err)
				}
			}
			globalTabletCatalogLabBenchmarkImage = image
		})
	}
}

func BenchmarkGlobalTabletCatalogLabRoutingSpace(b *testing.B) {
	for _, rows := range []uint64{187, 195, 230} {
		b.Run(fmt.Sprintf("%d-rows", rows), func(b *testing.B) {
			space, ok := GlobalTabletCatalogLabRoutingSpace(
				100_000_000_000, rows, 4096, 6<<20,
			)
			if !ok {
				b.Fatal("space")
			}
			b.ReportMetric(space.BytesPerDoc, "B/doc")
			b.ReportMetric(float64(space.Tablets), "tablets")
			b.ReportMetric(float64(space.TotalBytes)/(1<<30), "GiB")
		})
	}
}

func BenchmarkGlobalTabletCatalogLabRootAndSelectedAnchor(b *testing.B) {
	header, leaves, anchorRefs := segmentedTabletRouterLabTestInputs(b, 3072)
	header.StoreID = globalTabletCatalogLabTestStoreID
	bounds := globalTabletCatalogLabTestBounds
	bounds.SelectedRootGeneration = header.Generation
	root, rawLocator, anchors, _, err := EncodeSegmentedTabletRouterLab(
		make([]byte, SegmentedTabletRouterLabRootBytes),
		make([]byte, SegmentedTabletRouterLabLocatorBytes),
		make([]byte, 12*SegmentedTabletRouterLabAnchorPageBytes),
		header, anchorRefs, leaves,
	)
	if err != nil {
		b.Fatal(err)
	}
	locatorLogical, _ := GlobalTabletCatalogLabLocatorLogicalID(header.TabletID)
	locatorRef := globalTabletCatalogLabTestRef(
		1<<20, locatorLogical, header.Generation,
		GlobalTabletCatalogLabLocatorBytes, PageFingerprintDirectory,
	)
	tabletLogical, _ := GlobalTabletCatalogLabTabletRootLogicalID(header.TabletID)
	tabletRef := globalTabletCatalogLabTestRef(
		2<<20, tabletLogical, header.Generation,
		GlobalTabletCatalogLabTabletBytes, PageChunkDirectory,
	)
	tabletImage, err := EncodeGlobalTabletCatalogLabTabletRoot(
		make([]byte, GlobalTabletCatalogLabTabletBytes),
		PageHeader{
			StoreID:    globalTabletCatalogLabTestStoreID,
			Generation: header.Generation, LogicalID: tabletLogical,
			PageSize: GlobalTabletCatalogLabTabletBytes,
			PayloadLength: GlobalTabletCatalogLabRootHeader +
				SegmentedTabletRouterLabRootBytes,
			Kind: tabletRef.Kind,
		},
		bounds,
		locatorRef, root,
	)
	if err != nil {
		b.Fatal(err)
	}
	tablet, err := OpenGlobalTabletCatalogLabTabletRoot(
		tabletImage, tabletRef, bounds,
	)
	if err != nil {
		b.Fatal(err)
	}
	key := leaves[1536].Fence
	selected, ok := tablet.RouteAnchor(key)
	if !ok {
		b.Fatal("select anchor")
	}
	start := int(selected.PageID) * SegmentedTabletRouterLabAnchorPageBytes
	anchor, err := OpenGlobalTabletCatalogLabAnchor(
		anchors[start:start+SegmentedTabletRouterLabAnchorPageBytes],
		&tablet, selected.PageID,
	)
	if err != nil {
		b.Fatal(err)
	}
	hash := KeyHashBytes(segmentedTabletRouterLabTestSeed, key)
	b.Run("point", func(b *testing.B) {
		b.ReportAllocs()
		var route SegmentedTabletRouterLabRoute
		for b.Loop() {
			selected, _ = tablet.RouteAnchor(key)
			route, _ = anchor.RouteHashed(hash, key)
		}
		globalTabletCatalogLabBenchmarkRef = selected.Ref
		segmentedTabletRouterLabBenchmarkRoute = route
	})

	locatorEntries := make([]GlobalTabletCatalogLabLocatorEntry, len(leaves))
	for rank, leaf := range leaves {
		code := binary.LittleEndian.Uint16(rawLocator[int(leaf.LocalID)*2:])
		locatorEntries[rank] = GlobalTabletCatalogLabLocatorEntry{
			LocalID: leaf.LocalID, PageID: uint8(code >> 8),
			RowSlot: uint8(code), State: GlobalTabletCatalogLabLocatorLive,
		}
	}
	sort.Slice(locatorEntries, func(left, right int) bool {
		return locatorEntries[left].LocalID < locatorEntries[right].LocalID
	})
	locatorImage, err := EncodeGlobalTabletCatalogLabLocator(
		make([]byte, GlobalTabletCatalogLabLocatorBytes),
		PageHeader{
			StoreID:    globalTabletCatalogLabTestStoreID,
			Generation: header.Generation, LogicalID: locatorLogical,
			PageSize: GlobalTabletCatalogLabLocatorBytes,
			PayloadLength: GlobalTabletCatalogLabLocatorHeader +
				globalTabletCatalogLabPackedBytes,
			Kind: locatorRef.Kind,
		},
		bounds,
		header.TabletID, header.Generation, locatorEntries,
	)
	if err != nil {
		b.Fatal(err)
	}
	locator, err := OpenGlobalTabletCatalogLabLocator(
		locatorImage, locatorRef, bounds,
	)
	if err != nil {
		b.Fatal(err)
	}
	bucket := segmentedTabletRouterLabTestBucket(leaves[1536].Ref)
	b.Run("posting-bound-locator", func(b *testing.B) {
		b.ReportAllocs()
		var ref PageRef
		var zone BucketZone
		var ok bool
		for b.Loop() {
			ref, zone, ok = anchor.ResolveBucket(&locator, bucket)
		}
		if !ok {
			b.Fatal("posting resolve")
		}
		globalTabletCatalogLabBenchmarkRef = ref
		segmentedTabletRouterLabBenchmarkZone = zone
	})
}

func BenchmarkGlobalTabletCatalogLabPacked14Decode(b *testing.B) {
	var packed [globalTabletCatalogLabPackedBytes]byte
	for id := uint16(0); id < TabletLocalIdentityLabLocalCount; id++ {
		code := uint16(GlobalTabletCatalogLabLocatorLive)<<12 |
			uint16(id&0x0fff)
		globalTabletCatalogLabPut14(packed[:], id, code)
	}
	b.ReportAllocs()
	var code uint16
	for b.Loop() {
		code = globalTabletCatalogLabGet14(packed[:], 4095)
	}
	binary.LittleEndian.PutUint16(packed[:2], code)
}
