package storeio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
)

var globalTabletCatalogLabTestStoreID = [16]byte{
	'g', 'l', 'o', 'b', 'a', 'l', '-', 'c',
	'a', 't', 'a', 'l', 'o', 'g', '0', '1',
}

var globalTabletCatalogLabTestBounds = GlobalTabletCatalogLabBounds{
	StoreID:                globalTabletCatalogLabTestStoreID,
	SelectedRootGeneration: 200,
	FileEnd:                1 << 44, NextLogicalID: GlobalTabletCatalogLabFirstDynamicLogicalID + 1<<20,
}

func globalTabletCatalogLabTestRef(
	offset, logicalID, generation uint64, length uint32, kind PageKind,
) PageRef {
	return PageRef{
		Offset: offset, LogicalID: logicalID, Generation: generation,
		Length: length, Kind: kind,
	}
}

func globalTabletCatalogLabTestNode(
	t testing.TB,
) (
	GlobalTabletCatalogLabNodeView,
	[]byte,
	PageRef,
	[]GlobalTabletCatalogLabNodeEntry,
) {
	t.Helper()
	const generation = uint64(100)
	// Stable IDs are deliberately non-monotonic in lexical floor order. A
	// middle split allocates a fresh ID without renumbering either neighbor.
	tablets := []uint32{7, 19, 11}
	floors := [][]byte{nil, []byte("m"), []byte("z")}
	entries := make([]GlobalTabletCatalogLabNodeEntry, len(tablets))
	for at, tabletID := range tablets {
		logicalID, _ := GlobalTabletCatalogLabTabletRootLogicalID(tabletID)
		entries[at] = GlobalTabletCatalogLabNodeEntry{
			Floor: floors[at], ID: tabletID,
			Ref: globalTabletCatalogLabTestRef(
				uint64(at+1)*GlobalTabletCatalogLabTabletBytes,
				logicalID, generation, GlobalTabletCatalogLabTabletBytes,
				PageKeyDirectory,
			),
		}
	}
	logicalID, _ := GlobalTabletCatalogLabCatalogLeafLogicalID(3)
	image, err := EncodeGlobalTabletCatalogLabNode(
		make([]byte, GlobalTabletCatalogLabNodeBytes),
		GlobalTabletCatalogLabNodeHeader{
			StoreID:    globalTabletCatalogLabTestStoreID,
			Bounds:     globalTabletCatalogLabTestBounds,
			Generation: generation, LogicalID: logicalID, PageID: 3,
			Level: GlobalTabletCatalogLabLeaf,
			Kind:  PageIndexDirectory, ChildKind: PageKeyDirectory,
			ChildLength: GlobalTabletCatalogLabTabletBytes,
		},
		entries,
	)
	if err != nil {
		t.Fatal(err)
	}
	ref := globalTabletCatalogLabTestRef(
		128<<10, logicalID, generation,
		GlobalTabletCatalogLabNodeBytes, PageIndexDirectory,
	)
	view, err := OpenGlobalTabletCatalogLabNode(
		image, ref, globalTabletCatalogLabTestBounds,
	)
	if err != nil {
		t.Fatal(err)
	}
	return view, image, ref, entries
}

func TestGlobalTabletCatalogLabExactRouteCursorAndCOW(t *testing.T) {
	view, image, ref, entries := globalTabletCatalogLabTestNode(t)
	for _, test := range []struct {
		key  string
		want int
	}{
		{"", 0},
		{"a", 0},
		{"m", 1},
		{"y", 1},
		{"z", 2},
		{"zz", 2},
	} {
		got := view.Route([]byte(test.key))
		if got.ID != entries[test.want].ID ||
			got.Ref != entries[test.want].Ref ||
			int(got.Ordinal) != test.want {
			t.Fatalf("route %q = %+v, want entry %d", test.key, got, test.want)
		}
	}
	cursor := view.LowerBound([]byte("n"))
	for want := 1; want < len(entries); want++ {
		got, ok := cursor.Route()
		if !ok || got.ID != entries[want].ID {
			t.Fatalf("cursor %d = %+v,%v", want, got, ok)
		}
		if want+1 < len(entries) && !cursor.Next() {
			t.Fatalf("cursor ended at %d", want)
		}
	}
	replacement := entries[1].Ref
	replacement.Offset += 64 << 10
	replacement.Generation++
	before := bytes.Clone(image)
	if _, err := view.RewriteHandle(
		image, replacement.Generation, globalTabletCatalogLabTestBounds,
		entries[1].ID, replacement,
	); err == nil {
		t.Fatal("accepted overlapping COW destination")
	}
	if !bytes.Equal(image, before) {
		t.Fatal("overlap rejection changed admitted source")
	}
	backing := make([]byte, len(image)+1)
	copy(backing[1:], image)
	shiftedView, err := OpenGlobalTabletCatalogLabNode(
		backing[1:], ref, globalTabletCatalogLabTestBounds,
	)
	if err != nil {
		t.Fatal(err)
	}
	shiftedBefore := bytes.Clone(backing[1:])
	if _, err := shiftedView.RewriteHandle(
		backing[:len(image)], replacement.Generation,
		globalTabletCatalogLabTestBounds,
		entries[1].ID, replacement,
	); err == nil {
		t.Fatal("accepted partially overlapping COW destination")
	}
	if !bytes.Equal(backing[1:], shiftedBefore) {
		t.Fatal("partial-overlap rejection changed admitted source")
	}
	next, err := view.RewriteHandle(
		make([]byte, len(image)), replacement.Generation,
		globalTabletCatalogLabTestBounds,
		entries[1].ID, replacement,
	)
	if err != nil {
		t.Fatal(err)
	}
	nextRef := ref
	nextRef.Generation = replacement.Generation
	nextView, err := OpenGlobalTabletCatalogLabNode(
		next, nextRef, globalTabletCatalogLabTestBounds,
	)
	if err != nil {
		t.Fatal(err)
	}
	if nextView.floors.Header().Generation != nextRef.Generation {
		t.Fatalf(
			"embedded floor-map birth = %d, want node birth %d",
			nextView.floors.Header().Generation, nextRef.Generation,
		)
	}
	if got := nextView.Route([]byte("n")); got.Ref != replacement {
		t.Fatalf("COW route = %+v, want %+v", got.Ref, replacement)
	}
	if got := view.Route([]byte("n")); got.Ref != entries[1].Ref {
		t.Fatal("COW changed old snapshot")
	}
}

func TestGlobalTabletCatalogLabCOWIsCanonicalAcrossHistories(t *testing.T) {
	view100, _, ref100, entries := globalTabletCatalogLabTestNode(t)
	replacement := entries[1].Ref
	replacement.Offset += 128 << 10
	replacement.Generation = 102
	from100, err := view100.RewriteHandle(
		make([]byte, len(view100.image)), 102,
		globalTabletCatalogLabTestBounds,
		entries[1].ID, replacement,
	)
	if err != nil {
		t.Fatal(err)
	}

	image101, err := EncodeGlobalTabletCatalogLabNode(
		make([]byte, GlobalTabletCatalogLabNodeBytes),
		GlobalTabletCatalogLabNodeHeader{
			StoreID:    globalTabletCatalogLabTestStoreID,
			Bounds:     globalTabletCatalogLabTestBounds,
			Generation: 101, LogicalID: ref100.LogicalID, PageID: 3,
			Level: GlobalTabletCatalogLabLeaf,
			Kind:  PageIndexDirectory, ChildKind: PageKeyDirectory,
			ChildLength: GlobalTabletCatalogLabTabletBytes,
		},
		entries,
	)
	if err != nil {
		t.Fatal(err)
	}
	ref101 := ref100
	ref101.Generation = 101
	view101, err := OpenGlobalTabletCatalogLabNode(
		image101, ref101, globalTabletCatalogLabTestBounds,
	)
	if err != nil {
		t.Fatal(err)
	}
	from101, err := view101.RewriteHandle(
		make([]byte, len(view101.image)), 102,
		globalTabletCatalogLabTestBounds,
		entries[1].ID, replacement,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(from100, from101) {
		t.Fatal("equivalent catalog COW depends on ancestor generation")
	}

	corrupt := bytes.Clone(from100)
	payload := corrupt[PageHeaderSize:]
	mapStart := GlobalTabletCatalogLabNodePayloadHeaderBytes
	binary.LittleEndian.PutUint64(payload[mapStart+24:], 101)
	tabletAnchorMapLabSeal(
		payload[mapStart : mapStart+
			int(binary.LittleEndian.Uint32(payload[12:16]))],
	)
	if _, err := sealInitializedPage(corrupt); err != nil {
		t.Fatal(err)
	}
	expected := ref100
	expected.Generation = 102
	if _, err := OpenGlobalTabletCatalogLabNode(
		corrupt, expected, globalTabletCatalogLabTestBounds,
	); err == nil {
		t.Fatal("accepted floor map born before its enclosing node")
	}
}

func TestGlobalTabletCatalogLabCOWUsesMonotonicDestinationBounds(t *testing.T) {
	view, _, ref, entries := globalTabletCatalogLabTestNode(t)
	replacement := entries[1].Ref
	replacement.Offset = globalTabletCatalogLabTestBounds.FileEnd
	replacement.Generation = 101
	dst := make([]byte, len(view.image))
	before := bytes.Clone(dst)

	if _, err := view.RewriteHandle(
		dst, 101, globalTabletCatalogLabTestBounds,
		entries[1].ID, replacement,
	); err == nil {
		t.Fatal("accepted appended child under source file bounds")
	}
	if !bytes.Equal(dst, before) {
		t.Fatal("bounds rejection changed destination")
	}

	shrunk := globalTabletCatalogLabTestBounds
	shrunk.FileEnd--
	if _, err := view.RewriteHandle(
		dst, 101, shrunk, entries[1].ID, entries[1].Ref,
	); err == nil {
		t.Fatal("accepted shrinking destination bounds")
	}

	expanded := globalTabletCatalogLabTestBounds
	expanded.FileEnd += GlobalTabletCatalogLabTabletBytes
	next, err := view.RewriteHandle(
		dst, 101, expanded, entries[1].ID, replacement,
	)
	if err != nil {
		t.Fatal(err)
	}
	nextRef := ref
	nextRef.Generation = 101
	nextView, err := OpenGlobalTabletCatalogLabNode(next, nextRef, expanded)
	if err != nil {
		t.Fatal(err)
	}
	if got := nextView.Route([]byte("n")); got.Ref != replacement {
		t.Fatalf("expanded-bounds route = %+v, want %+v", got.Ref, replacement)
	}
}

func TestGlobalTabletCatalogLabNonMonotonicStableIDsAllLevels(t *testing.T) {
	for _, level := range []GlobalTabletCatalogLabNodeLevel{
		GlobalTabletCatalogLabLeaf,
		GlobalTabletCatalogLabBranch,
		GlobalTabletCatalogLabRoot,
	} {
		t.Run(fmt.Sprintf("level-%d", level), func(t *testing.T) {
			ids := []uint32{0, 2, 1}
			floors := [][]byte{nil, []byte("m"), []byte("z")}
			childLevel := GlobalTabletCatalogLabLeaf
			childKind := PageIndexDirectory
			childLength := uint32(GlobalTabletCatalogLabNodeBytes)
			pageID := uint32(3)
			var logicalID uint64
			switch level {
			case GlobalTabletCatalogLabLeaf:
				childKind = PageKeyDirectory
				childLength = GlobalTabletCatalogLabTabletBytes
				logicalID, _ = GlobalTabletCatalogLabCatalogLeafLogicalID(pageID)
			case GlobalTabletCatalogLabBranch:
				logicalID, _ = GlobalTabletCatalogLabCatalogBranchLogicalID(pageID)
			case GlobalTabletCatalogLabRoot:
				pageID = 0
				childLevel = GlobalTabletCatalogLabBranch
				logicalID = GlobalTabletCatalogLabRootLogicalID
			}
			entries := make([]GlobalTabletCatalogLabNodeEntry, len(ids))
			for at, id := range ids {
				childLogical, ok := globalTabletCatalogLabChildLogicalID(
					level, childLevel, id,
				)
				if !ok {
					t.Fatal("derive non-monotonic child")
				}
				entries[at] = GlobalTabletCatalogLabNodeEntry{
					Floor: floors[at], ID: id,
					Ref: globalTabletCatalogLabTestRef(
						uint64(at+1)*8192, childLogical, 50,
						childLength, childKind,
					),
				}
			}
			pageBytes, _ := globalTabletCatalogLabNodePageBytes(level)
			image, err := EncodeGlobalTabletCatalogLabNode(
				make([]byte, pageBytes),
				GlobalTabletCatalogLabNodeHeader{
					StoreID:    globalTabletCatalogLabTestStoreID,
					Bounds:     globalTabletCatalogLabTestBounds,
					Generation: 50, LogicalID: logicalID, PageID: pageID,
					Level: level, RootChildLevel: childLevel,
					Kind: PageFloat64Catalog, ChildKind: childKind,
					ChildLength: childLength,
				},
				entries,
			)
			if err != nil {
				t.Fatal(err)
			}
			ref := globalTabletCatalogLabTestRef(
				1<<30, logicalID, 50, uint32(pageBytes), PageFloat64Catalog,
			)
			view, err := OpenGlobalTabletCatalogLabNode(
				image, ref, globalTabletCatalogLabTestBounds,
			)
			if err != nil {
				t.Fatal(err)
			}
			crossStore := globalTabletCatalogLabTestBounds
			crossStore.StoreID[0] ^= 1
			if _, err := OpenGlobalTabletCatalogLabNode(
				image, ref, crossStore,
			); !errors.Is(err, ErrGlobalTabletCatalogLabCorrupt) {
				t.Fatalf("cross-Store node graft error = %v", err)
			}
			staleSnapshot := globalTabletCatalogLabTestBounds
			staleSnapshot.SelectedRootGeneration = ref.Generation - 1
			if _, err := OpenGlobalTabletCatalogLabNode(
				image, ref, staleSnapshot,
			); !errors.Is(err, ErrGlobalTabletCatalogLabCorrupt) {
				t.Fatalf("stale-generation node graft error = %v", err)
			}
			for at, key := range [][]byte{nil, []byte("n"), []byte("zz")} {
				if got := view.Route(key); got.ID != ids[at] ||
					got.Ref != entries[at].Ref {
					t.Fatalf("route %q = %+v, want %+v", key, got, entries[at])
				}
			}
			cursor := view.LowerBound(nil)
			for at, want := range ids {
				got, ok := cursor.Route()
				if !ok || got.ID != want {
					t.Fatalf("cursor %d = %+v,%v", at, got, ok)
				}
				if at+1 < len(ids) && !cursor.Next() {
					t.Fatal("early cursor end")
				}
			}
			replacement := entries[1].Ref
			replacement.Offset += 64 << 10
			replacement.Generation++
			next, err := view.RewriteHandle(
				make([]byte, pageBytes), replacement.Generation,
				globalTabletCatalogLabTestBounds,
				ids[1], replacement,
			)
			if err != nil {
				t.Fatal(err)
			}
			nextRef := ref
			nextRef.Generation++
			nextView, err := OpenGlobalTabletCatalogLabNode(
				next, nextRef, globalTabletCatalogLabTestBounds,
			)
			if err != nil {
				t.Fatal(err)
			}
			if got := nextView.Route([]byte("n")); got.ID != ids[1] ||
				got.Ref != replacement {
				t.Fatalf("COW route = %+v", got)
			}
		})
	}
}

func TestGlobalTabletCatalogLabAdaptiveDepthAndWorstCaseCapacity(t *testing.T) {
	leaf := GlobalTabletCatalogLabWorstCaseFanout(
		GlobalTabletCatalogLabNodeBytes, 256,
	)
	root := GlobalTabletCatalogLabWorstCaseFanout(
		GlobalTabletCatalogLabRootBytes, 256,
	)
	if leaf < 28 || root < 235 {
		t.Fatalf("worst-case fanout leaf/root = %d/%d, want >=28/>=235", leaf, root)
	}
	if capacity := uint64(leaf) * uint64(leaf) * uint64(root); capacity < 174_000 {
		t.Fatalf("three-level adversarial capacity = %d", capacity)
	}
	if twoLevel := uint64(leaf) * uint64(root); twoLevel >= 174_000 {
		t.Fatalf("test no longer exercises rare third level: %d", twoLevel)
	}
	bounds, ok := GlobalTabletCatalogLabCatalogGeometry(174_000, 256)
	if !ok || bounds.Levels != 3 || bounds.PointPages != 3 ||
		bounds.LeafPages != 6215 || bounds.BranchPages != 222 ||
		bounds.MaximumTablets < 174_000 ||
		bounds.COWBytes != 80<<10 ||
		bounds.ResidentBytes != 64<<10 {
		t.Fatalf("174k adversarial bounds = %+v,%v", bounds, ok)
	}
	if typical, ok := GlobalTabletCatalogLabCatalogGeometry(174_000, 8); !ok ||
		typical.Levels != 2 || typical.PointPages != 2 {
		t.Fatalf("short-fence bounds = %+v,%v", typical, ok)
	}

	// Exercise the actual prefix/restart encoder near the universal leaf
	// bound with valid, strictly ordered 256-byte binary separators.
	count := leaf
	entries := make([]GlobalTabletCatalogLabNodeEntry, count)
	for at := range entries {
		var floor []byte
		if at != 0 {
			floor = make([]byte, 256)
			binary.BigEndian.PutUint32(floor, uint32(at))
			for i := 4; i < len(floor); i++ {
				floor[i] = byte(uint32(at)*0x9e3779b9 + uint32(i)*0x85ebca6b)
			}
		}
		logicalID, _ := GlobalTabletCatalogLabTabletRootLogicalID(uint32(at))
		entries[at] = GlobalTabletCatalogLabNodeEntry{
			Floor: floor, ID: uint32(at),
			Ref: globalTabletCatalogLabTestRef(
				uint64(at+1)*8192, logicalID, 10,
				GlobalTabletCatalogLabTabletBytes, PageKeyDirectory,
			),
		}
	}
	logicalID, _ := GlobalTabletCatalogLabCatalogLeafLogicalID(0)
	if _, err := EncodeGlobalTabletCatalogLabNode(
		make([]byte, GlobalTabletCatalogLabNodeBytes),
		GlobalTabletCatalogLabNodeHeader{
			StoreID:    globalTabletCatalogLabTestStoreID,
			Bounds:     globalTabletCatalogLabTestBounds,
			Generation: 10, LogicalID: logicalID,
			Level: GlobalTabletCatalogLabLeaf,
			Kind:  PageIndexDirectory, ChildKind: PageKeyDirectory,
			ChildLength: GlobalTabletCatalogLabTabletBytes,
		},
		entries,
	); err != nil {
		t.Fatalf("actual worst-case-bound corpus: %v", err)
	}
}

func TestGlobalTabletCatalogLabTwoAndThreeLevelRootTyping(t *testing.T) {
	for _, childLevel := range []GlobalTabletCatalogLabNodeLevel{
		GlobalTabletCatalogLabLeaf,
		GlobalTabletCatalogLabBranch,
	} {
		childID := uint32(5)
		var childLogical uint64
		var ok bool
		if childLevel == GlobalTabletCatalogLabLeaf {
			childLogical, ok = GlobalTabletCatalogLabCatalogLeafLogicalID(childID)
		} else {
			childLogical, ok = GlobalTabletCatalogLabCatalogBranchLogicalID(childID)
		}
		if !ok {
			t.Fatal("child logical ID")
		}
		entry := GlobalTabletCatalogLabNodeEntry{
			ID: childID,
			Ref: globalTabletCatalogLabTestRef(
				8192, childLogical, 20,
				GlobalTabletCatalogLabNodeBytes, PageIndexDirectory,
			),
		}
		image, err := EncodeGlobalTabletCatalogLabNode(
			make([]byte, GlobalTabletCatalogLabRootBytes),
			GlobalTabletCatalogLabNodeHeader{
				StoreID:    globalTabletCatalogLabTestStoreID,
				Bounds:     globalTabletCatalogLabTestBounds,
				Generation: 20, LogicalID: GlobalTabletCatalogLabRootLogicalID,
				Level: GlobalTabletCatalogLabRoot, RootChildLevel: childLevel,
				Kind: PageFloat64Catalog, ChildKind: PageIndexDirectory,
				ChildLength: GlobalTabletCatalogLabNodeBytes,
			},
			[]GlobalTabletCatalogLabNodeEntry{entry},
		)
		if err != nil {
			t.Fatal(err)
		}
		rootRef := globalTabletCatalogLabTestRef(
			256<<10, GlobalTabletCatalogLabRootLogicalID, 20,
			GlobalTabletCatalogLabRootBytes, PageFloat64Catalog,
		)
		view, err := OpenGlobalTabletCatalogLabNode(
			image, rootRef, globalTabletCatalogLabTestBounds,
		)
		if err != nil {
			t.Fatal(err)
		}
		if got := view.Route(nil); got.ID != childID || got.Ref != entry.Ref {
			t.Fatalf("root child level %d = %+v", childLevel, got)
		}
	}
}

func TestGlobalTabletCatalogLabCompactLocator(t *testing.T) {
	const tabletID = uint32(42)
	logicalID, _ := GlobalTabletCatalogLabLocatorLogicalID(tabletID)
	header := PageHeader{
		StoreID:    globalTabletCatalogLabTestStoreID,
		Generation: 77, LogicalID: logicalID,
		PageSize: GlobalTabletCatalogLabLocatorBytes,
		PayloadLength: GlobalTabletCatalogLabLocatorHeader +
			globalTabletCatalogLabPackedBytes,
		Kind: PageFingerprintDirectory,
	}
	entries := []GlobalTabletCatalogLabLocatorEntry{
		{LocalID: 0, PageID: 0, RowSlot: 7, State: GlobalTabletCatalogLabLocatorLive},
		{LocalID: 1, PageID: 15, RowSlot: 255, State: GlobalTabletCatalogLabLocatorRetired},
		{LocalID: 2047, PageID: 8, RowSlot: 9, State: GlobalTabletCatalogLabLocatorLive},
		{LocalID: 4095, PageID: 14, RowSlot: 3, State: GlobalTabletCatalogLabLocatorLive},
	}
	image, err := EncodeGlobalTabletCatalogLabLocator(
		make([]byte, GlobalTabletCatalogLabLocatorBytes),
		header, globalTabletCatalogLabTestBounds, tabletID, 70, entries,
	)
	if err != nil {
		t.Fatal(err)
	}
	ref := globalTabletCatalogLabTestRef(
		64<<10, logicalID, header.Generation,
		GlobalTabletCatalogLabLocatorBytes, header.Kind,
	)
	view, err := OpenGlobalTabletCatalogLabLocator(
		image, ref, globalTabletCatalogLabTestBounds,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		pageID, rowSlot, state := view.Resolve(entry.LocalID)
		if pageID != entry.PageID || rowSlot != entry.RowSlot ||
			state != entry.State {
			t.Fatalf("resolve %d = %d/%d/%d, want %+v",
				entry.LocalID, pageID, rowSlot, state, entry)
		}
	}
	if _, _, state := view.Resolve(99); state != GlobalTabletCatalogLabLocatorEmpty {
		t.Fatalf("empty state = %d", state)
	}
	if len(view.packed) != 7168 {
		t.Fatalf("packed locator bytes = %d", len(view.packed))
	}
}

func TestGlobalTabletCatalogLabCacheableTabletReadPaths(t *testing.T) {
	header, leaves, anchorRefs := segmentedTabletRouterLabTestInputs(t, 1024)
	bounds := globalTabletCatalogLabTestBounds
	bounds.SelectedRootGeneration = header.Generation
	root, rawLocator, anchors, _, err := EncodeSegmentedTabletRouterLab(
		make([]byte, SegmentedTabletRouterLabRootBytes),
		make([]byte, SegmentedTabletRouterLabLocatorBytes),
		make([]byte, 4*SegmentedTabletRouterLabAnchorPageBytes),
		header, anchorRefs, leaves,
	)
	if err != nil {
		t.Fatal(err)
	}
	locatorLogical, _ := GlobalTabletCatalogLabLocatorLogicalID(header.TabletID)
	locatorRef := globalTabletCatalogLabTestRef(
		1<<20, locatorLogical, header.Generation,
		GlobalTabletCatalogLabLocatorBytes, PageFingerprintDirectory,
	)
	locatorEntries := make([]GlobalTabletCatalogLabLocatorEntry, len(leaves))
	for rank, leaf := range leaves {
		code := binary.LittleEndian.Uint16(rawLocator[int(leaf.LocalID)*2:])
		locatorEntries[rank] = GlobalTabletCatalogLabLocatorEntry{
			LocalID: leaf.LocalID, PageID: uint8(code >> 8),
			RowSlot: uint8(code), State: GlobalTabletCatalogLabLocatorLive,
		}
	}
	// Encoder requires LocalID order; physical locator identity is independent
	// of lexical anchor order.
	for i := 1; i < len(locatorEntries); i++ {
		for j := i; j > 0 && locatorEntries[j].LocalID < locatorEntries[j-1].LocalID; j-- {
			locatorEntries[j], locatorEntries[j-1] = locatorEntries[j-1], locatorEntries[j]
		}
	}
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
		t.Fatal(err)
	}
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
		t.Fatal(err)
	}
	tablet, err := OpenGlobalTabletCatalogLabTabletRoot(
		tabletImage, tabletRef, bounds,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := tablet.LocatorRef(); !ok || got != locatorRef {
		t.Fatalf("locator ref = %+v,%v", got, ok)
	}
	locator, err := OpenGlobalTabletCatalogLabLocator(
		locatorImage, locatorRef, bounds,
	)
	if err != nil {
		t.Fatal(err)
	}
	crossStore := bounds
	crossStore.StoreID[0] ^= 1
	if _, err := OpenGlobalTabletCatalogLabTabletRoot(
		tabletImage, tabletRef, crossStore,
	); !errors.Is(err, ErrGlobalTabletCatalogLabCorrupt) {
		t.Fatalf("cross-Store tablet graft error = %v", err)
	}
	if _, err := OpenGlobalTabletCatalogLabLocator(
		locatorImage, locatorRef, crossStore,
	); !errors.Is(err, ErrGlobalTabletCatalogLabCorrupt) {
		t.Fatalf("cross-Store locator graft error = %v", err)
	}
	staleSnapshot := bounds
	staleSnapshot.SelectedRootGeneration = header.Generation - 1
	if _, err := OpenGlobalTabletCatalogLabTabletRoot(
		tabletImage, tabletRef, staleSnapshot,
	); !errors.Is(err, ErrGlobalTabletCatalogLabCorrupt) {
		t.Fatalf("stale-generation tablet graft error = %v", err)
	}
	if _, err := OpenGlobalTabletCatalogLabLocator(
		locatorImage, locatorRef, staleSnapshot,
	); !errors.Is(err, ErrGlobalTabletCatalogLabCorrupt) {
		t.Fatalf("stale-generation locator graft error = %v", err)
	}
	for _, rank := range []int{0, 255, 256, 511, 512, 1023} {
		key := leaves[rank].Fence
		next, ok := tablet.RouteAnchor(key)
		if !ok {
			t.Fatalf("route anchor %d", rank)
		}
		pageImage := anchors[int(next.PageID)*SegmentedTabletRouterLabAnchorPageBytes:]
		anchor, err := OpenGlobalTabletCatalogLabAnchor(
			pageImage[:SegmentedTabletRouterLabAnchorPageBytes],
			&tablet, next.PageID,
		)
		if err != nil {
			t.Fatal(err)
		}
		hash := KeyHashBytes(segmentedTabletRouterLabTestSeed, key)
		point, ok := anchor.RouteHashed(hash, key)
		if !ok || point.Bucket != segmentedTabletRouterLabTestBucket(leaves[rank].Ref) ||
			point.Ref != leaves[rank].Ref {
			t.Fatalf("point %d = %+v,%v", rank, point, ok)
		}
		ref, zone, ok := anchor.ResolveBucket(&locator, point.Bucket)
		if !ok || ref != point.Ref || zone != point.Zone {
			t.Fatalf("posting %d = %+v/%x/%v", rank, ref, zone, ok)
		}
	}
	target := 512
	selected, _ := tablet.RouteAnchor(leaves[target].Fence)
	start := int(selected.PageID) * SegmentedTabletRouterLabAnchorPageBytes
	anchor, err := OpenGlobalTabletCatalogLabAnchor(
		anchors[start:start+SegmentedTabletRouterLabAnchorPageBytes],
		&tablet, selected.PageID,
	)
	if err != nil {
		t.Fatal(err)
	}
	crossStoreRoot := tablet
	crossStoreRoot.bounds.StoreID[0] ^= 1
	if _, err := OpenGlobalTabletCatalogLabAnchor(
		anchors[start:start+SegmentedTabletRouterLabAnchorPageBytes],
		&crossStoreRoot, selected.PageID,
	); !errors.Is(err, ErrGlobalTabletCatalogLabCorrupt) {
		t.Fatalf("cross-Store anchor owner error = %v", err)
	}
	staleRoot := tablet
	staleRoot.bounds.SelectedRootGeneration = tablet.header.Generation - 1
	if _, err := OpenGlobalTabletCatalogLabAnchor(
		anchors[start:start+SegmentedTabletRouterLabAnchorPageBytes],
		&staleRoot, selected.PageID,
	); !errors.Is(err, ErrGlobalTabletCatalogLabCorrupt) {
		t.Fatalf("stale-generation anchor owner error = %v", err)
	}
	bucket := segmentedTabletRouterLabTestBucket(leaves[target].Ref)
	graft := locator
	graft.ref.Offset += 8192
	if _, _, ok := anchor.ResolveBucket(&graft, bucket); ok {
		t.Fatal("accepted same-tablet locator under another PageRef")
	}
	staleRef := locatorRef
	staleRef.Offset += 16 << 10
	staleRef.Generation--
	staleImage, err := EncodeGlobalTabletCatalogLabLocator(
		make([]byte, GlobalTabletCatalogLabLocatorBytes),
		PageHeader{
			StoreID:    globalTabletCatalogLabTestStoreID,
			Generation: staleRef.Generation, LogicalID: locatorLogical,
			PageSize: GlobalTabletCatalogLabLocatorBytes,
			PayloadLength: GlobalTabletCatalogLabLocatorHeader +
				globalTabletCatalogLabPackedBytes,
			Kind: staleRef.Kind,
		},
		bounds,
		header.TabletID, staleRef.Generation, locatorEntries,
	)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := OpenGlobalTabletCatalogLabLocator(
		staleImage, staleRef, bounds,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := anchor.ResolveBucket(&stale, bucket); ok {
		t.Fatal("grafted stale same-tablet locator")
	}
}

func TestGlobalTabletCatalogLabFailClosed(t *testing.T) {
	_, image, ref, _ := globalTabletCatalogLabTestNode(t)
	tests := map[string]func([]byte){
		"child state": func(page []byte) {
			header, payload, err := OpenPage(page)
			if err != nil {
				panic(err)
			}
			mapBytes := int(binary.LittleEndian.Uint32(payload[12:16]))
			handle := PageHeaderSize + GlobalTabletCatalogLabNodePayloadHeaderBytes + mapBytes
			clear(page[handle : handle+GlobalTabletCatalogLabHandleBytes])
			if _, err := SealPage(page[:header.PageSize]); err != nil {
				panic(err)
			}
		},
		"child length": func(page []byte) {
			binary.LittleEndian.PutUint32(
				page[PageHeaderSize+16:PageHeaderSize+20],
				GlobalTabletCatalogLabTabletBytes*2,
			)
			if _, err := SealPage(page); err != nil {
				panic(err)
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			corrupt := bytes.Clone(image)
			mutate(corrupt)
			if _, err := OpenGlobalTabletCatalogLabNode(
				corrupt, ref, globalTabletCatalogLabTestBounds,
			); !errors.Is(
				err, ErrGlobalTabletCatalogLabCorrupt,
			) {
				t.Fatalf("open error = %v", err)
			}
		})
	}
}

func TestGlobalTabletCatalogLabRootAndLocatorFailClosed(t *testing.T) {
	header, leaves, anchorRefs := segmentedTabletRouterLabTestInputs(t, 1)
	bounds := globalTabletCatalogLabTestBounds
	bounds.SelectedRootGeneration = header.Generation
	rawRoot, _, _, _, err := EncodeSegmentedTabletRouterLab(
		make([]byte, SegmentedTabletRouterLabRootBytes),
		make([]byte, SegmentedTabletRouterLabLocatorBytes),
		make([]byte, SegmentedTabletRouterLabAnchorPageBytes),
		header, anchorRefs, leaves,
	)
	if err != nil {
		t.Fatal(err)
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
		locatorRef, rawRoot,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("discoverable locator binding", func(t *testing.T) {
		corrupt := bytes.Clone(tabletImage)
		payload := corrupt[PageHeaderSize:]
		ref := decodePageRef(payload[16 : 16+PageRefSize])
		ref.LogicalID++
		encodePageRef(payload[16:16+PageRefSize], ref)
		if _, err := SealPage(corrupt); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenGlobalTabletCatalogLabTabletRoot(
			corrupt, tabletRef, bounds,
		); !errors.Is(err, ErrGlobalTabletCatalogLabCorrupt) {
			t.Fatalf("open error = %v", err)
		}
	})
	t.Run("embedded root semantics", func(t *testing.T) {
		corrupt := bytes.Clone(tabletImage)
		inner := corrupt[PageHeaderSize+GlobalTabletCatalogLabRootHeader:]
		inner[14] = SegmentedTabletRouterLabMaxPages + 1
		segmentedTabletRouterLabSeal(
			inner[:SegmentedTabletRouterLabRootBytes],
			segmentedTabletRouterLabRootTrailerAt,
		)
		if _, err := SealPage(corrupt); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenGlobalTabletCatalogLabTabletRoot(
			corrupt, tabletRef, bounds,
		); !errors.Is(err, ErrGlobalTabletCatalogLabCorrupt) {
			t.Fatalf("open error = %v", err)
		}
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
		header.TabletID, header.Generation,
		[]GlobalTabletCatalogLabLocatorEntry{{
			LocalID: leaves[0].LocalID, State: GlobalTabletCatalogLabLocatorLive,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint16(locatorImage[PageHeaderSize+8:], 2)
	if _, err := SealPage(locatorImage); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenGlobalTabletCatalogLabLocator(
		locatorImage, locatorRef, bounds,
	); !errors.Is(err, ErrGlobalTabletCatalogLabCorrupt) {
		t.Fatalf("locator open error = %v", err)
	}
}

func TestGlobalTabletCatalogLabNamespaceAndSpace(t *testing.T) {
	ranges := [][2]uint64{
		{GlobalTabletCatalogLabStateRootLogicalID, GlobalTabletCatalogLabStateRootLogicalID + 1},
		{GlobalTabletCatalogLabLeafLogicalIDBase, GlobalTabletCatalogLabLeafLogicalIDLimit},
		{GlobalTabletCatalogLabAnchorLogicalIDBase, GlobalTabletCatalogLabAnchorLogicalIDLimit},
		{GlobalTabletCatalogLabTabletRootLogicalIDBase, GlobalTabletCatalogLabTabletRootLogicalIDLimit},
		{GlobalTabletCatalogLabLocatorLogicalIDBase, GlobalTabletCatalogLabLocatorLogicalIDLimit},
		{GlobalTabletCatalogLabLeafPageLogicalIDBase, GlobalTabletCatalogLabLeafPageLogicalIDLimit},
		{GlobalTabletCatalogLabBranchPageLogicalIDBase, GlobalTabletCatalogLabBranchPageLogicalIDLimit},
		{GlobalTabletCatalogLabRootLogicalID, GlobalTabletCatalogLabRootLogicalID + 1},
		{PrimaryTabletRouteLogicalIDBase, PrimaryTabletRouteLogicalIDLimit},
		{PrimaryTabletDirectoryLogicalIDBase, PrimaryTabletDirectoryLogicalIDLimit},
	}
	for at := 1; at < len(ranges); at++ {
		if ranges[at-1][1] != ranges[at][0] {
			t.Fatalf("namespace gap/collision %d: %v then %v", at, ranges[at-1], ranges[at])
		}
	}
	if GlobalTabletCatalogLabFirstDynamicLogicalID != ranges[len(ranges)-1][1] ||
		!GlobalTabletCatalogLabIsDynamicLogicalID(GlobalTabletCatalogLabFirstDynamicLogicalID) {
		t.Fatal("dynamic namespace boundary")
	}
	view, _, _, entries := globalTabletCatalogLabTestNode(t)
	if _, err := view.RewriteHandle(
		make([]byte, len(view.image)), uint64(1)<<48,
		globalTabletCatalogLabTestBounds,
		entries[0].ID, entries[0].Ref,
	); err == nil {
		t.Fatal("accepted 48-bit generation overflow")
	}
	space, ok := GlobalTabletCatalogLabRoutingSpace(
		100_000_000_000, 195, 4096, 6<<20,
	)
	if !ok {
		t.Fatal("routing space")
	}
	if space.Tablets != 125_201 ||
		space.BytesPerDoc < 0.184 || space.BytesPerDoc > 0.185 {
		t.Fatalf("100B space = %+v", space)
	}
	t.Logf(
		"100B: tablets=%d tablet-routing=%0.3fGiB catalog=%0.3fMiB B/doc=%0.6f",
		space.Tablets, float64(space.TabletBytes)/(1<<30),
		float64(space.CatalogBytes)/(1<<20), space.BytesPerDoc,
	)
}

func TestGlobalTabletCatalogLabReferenceBounds(t *testing.T) {
	_, image, ref, entries := globalTabletCatalogLabTestNode(t)
	outOfFile := entries
	outOfFile[0].Ref.Offset = globalTabletCatalogLabTestBounds.FileEnd - 4096
	logicalID, _ := GlobalTabletCatalogLabCatalogLeafLogicalID(3)
	if _, err := EncodeGlobalTabletCatalogLabNode(
		make([]byte, GlobalTabletCatalogLabNodeBytes),
		GlobalTabletCatalogLabNodeHeader{
			StoreID:    globalTabletCatalogLabTestStoreID,
			Bounds:     globalTabletCatalogLabTestBounds,
			Generation: 100, LogicalID: logicalID, PageID: 3,
			Level: GlobalTabletCatalogLabLeaf,
			Kind:  PageIndexDirectory, ChildKind: PageKeyDirectory,
			ChildLength: GlobalTabletCatalogLabTabletBytes,
		},
		outOfFile,
	); err == nil {
		t.Fatal("accepted child extent crossing FileEnd")
	}
	huge := entries
	huge[0].Ref.Offset = ^uint64(0) &^ 4095
	if _, err := EncodeGlobalTabletCatalogLabNode(
		make([]byte, GlobalTabletCatalogLabNodeBytes),
		GlobalTabletCatalogLabNodeHeader{
			StoreID:    globalTabletCatalogLabTestStoreID,
			Bounds:     globalTabletCatalogLabTestBounds,
			Generation: 100, LogicalID: logicalID, PageID: 3,
			Level: GlobalTabletCatalogLabLeaf,
			Kind:  PageIndexDirectory, ChildKind: PageKeyDirectory,
			ChildLength: GlobalTabletCatalogLabTabletBytes,
		},
		huge,
	); err == nil {
		t.Fatal("accepted huge packed child offset")
	}
	badExpected := ref
	badExpected.Offset = globalTabletCatalogLabTestBounds.FileEnd
	if _, err := OpenGlobalTabletCatalogLabNode(
		image, badExpected, globalTabletCatalogLabTestBounds,
	); !errors.Is(err, ErrGlobalTabletCatalogLabCorrupt) {
		t.Fatalf("out-of-file node error = %v", err)
	}
	tooSmallNamespace := globalTabletCatalogLabTestBounds
	tooSmallNamespace.NextLogicalID = entries[0].Ref.LogicalID
	if _, err := OpenGlobalTabletCatalogLabNode(
		image, ref, tooSmallNamespace,
	); !errors.Is(err, ErrGlobalTabletCatalogLabCorrupt) {
		t.Fatalf("namespace-bound node error = %v", err)
	}
}

func FuzzGlobalTabletCatalogLabNodeAdmission(f *testing.F) {
	_, image, ref, _ := globalTabletCatalogLabTestNode(f)
	f.Add(uint16(0), byte(1))
	f.Add(uint16(len(image)-1), byte(0x80))
	f.Fuzz(func(t *testing.T, offset uint16, value byte) {
		corrupt := bytes.Clone(image)
		at := int(offset) % len(corrupt)
		corrupt[at] ^= value
		view, err := OpenGlobalTabletCatalogLabNode(
			corrupt, ref, globalTabletCatalogLabTestBounds,
		)
		if value == 0 {
			if err != nil || view.Count() != 3 {
				t.Fatalf("unchanged image: %v", err)
			}
			return
		}
		if err == nil {
			t.Fatalf("admitted mutation at %d xor %02x", at, value)
		}
	})
}

func ExampleGlobalTabletCatalogLabRoutingSpace() {
	space, _ := GlobalTabletCatalogLabRoutingSpace(
		100_000_000_000, 195, 4096, 6<<20,
	)
	fmt.Printf("%.4f bytes/document\n", space.BytesPerDoc)
	// Output:
	// 0.1847 bytes/document
}
