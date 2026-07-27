package storeio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"unsafe"
)

// The global tablet catalog lab closes the two missing durable routing links:
//
//   - an exact lexical catalog whose leaves name TabletRoot PageRefs; and
//   - independently cacheable tablet roots and 14-bit local-ID locators.
//
// The normal catalog has two levels. A rare third level is available for
// adversarial 256-byte separators. With the conservative no-prefix-sharing
// bound, an 8 KiB node admits at least 28 children and the 64 KiB root admits
// at least 235. Three levels therefore cover 235*28*28 = 184,240 tablets,
// above the 174,000-tablet 100-billion-document design bound. The exact writer
// still measures every real image and fails closed on overflow.
//
// Every view borrows one admitted image. There is no Go object per tablet:
// the resident object is one catalog root, while catalog branches/leaves,
// tablet roots, locators, and anchor pages are ordinary cache frames.
const (
	GlobalTabletCatalogLabRootBytes              = 64 << 10
	GlobalTabletCatalogLabNodeBytes              = 8 << 10
	GlobalTabletCatalogLabTabletBytes            = 8 << 10
	GlobalTabletCatalogLabLocatorBytes           = 8 << 10
	GlobalTabletCatalogLabHandleBytes            = 12
	GlobalTabletCatalogLabNodePayloadHeaderBytes = 32
	GlobalTabletCatalogLabLocatorHeader          = 32
	GlobalTabletCatalogLabRootHeader             = 64

	GlobalTabletCatalogLabMaxLeafPages   = 1 << 13
	GlobalTabletCatalogLabMaxBranchPages = 1 << 9

	GlobalTabletCatalogLabStateRootLogicalID       = StateRootLogicalID
	GlobalTabletCatalogLabLeafLogicalIDBase        = PrimaryLeafLogicalIDBase
	GlobalTabletCatalogLabLeafLogicalIDLimit       = PrimaryLeafLogicalIDLimit
	GlobalTabletCatalogLabAnchorLogicalIDBase      = PrimaryAnchorLogicalIDBase
	GlobalTabletCatalogLabAnchorLogicalIDLimit     = PrimaryAnchorLogicalIDLimit
	GlobalTabletCatalogLabTabletRootLogicalIDBase  = PrimaryTabletRootLogicalIDBase
	GlobalTabletCatalogLabTabletRootLogicalIDLimit = PrimaryTabletRootLogicalIDLimit
	GlobalTabletCatalogLabLocatorLogicalIDBase     = PrimaryLocatorLogicalIDBase
	GlobalTabletCatalogLabLocatorLogicalIDLimit    = PrimaryLocatorLogicalIDLimit
	GlobalTabletCatalogLabLeafPageLogicalIDBase    = PrimaryCatalogLeafLogicalIDBase
	GlobalTabletCatalogLabLeafPageLogicalIDLimit   = PrimaryCatalogLeafLogicalIDLimit
	GlobalTabletCatalogLabBranchPageLogicalIDBase  = PrimaryCatalogBranchLogicalIDBase
	GlobalTabletCatalogLabBranchPageLogicalIDLimit = PrimaryCatalogBranchLogicalIDLimit
	GlobalTabletCatalogLabRootLogicalID            = PrimaryCatalogRootLogicalID
	GlobalTabletCatalogLabFirstDynamicLogicalID    = PrimaryFirstDynamicLogicalID

	globalTabletCatalogLabNodeVersion    = uint32(1)
	globalTabletCatalogLabLocatorVersion = uint32(1)
	globalTabletCatalogLabRootVersion    = uint32(1)
	globalTabletCatalogLabPackedBits     = 14
	globalTabletCatalogLabPackedBytes    = TabletLocalIdentityLabLocalCount * globalTabletCatalogLabPackedBits / 8
)

var (
	ErrGlobalTabletCatalogLabCorrupt = errors.New(
		"vibejson: corrupt global tablet catalog lab page",
	)
	ErrGlobalTabletCatalogLabNoSpace = errors.New(
		"vibejson: global tablet catalog lab page has no space",
	)
)

// GlobalTabletCatalogLabNodeLevel identifies the exact child identity derived
// from a node row. A leaf names tablet roots; a branch names leaf pages; a root
// names either leaves (normal two-level form) or branches (rare three-level
// form).
type GlobalTabletCatalogLabNodeLevel uint8

const (
	GlobalTabletCatalogLabLeaf GlobalTabletCatalogLabNodeLevel = iota
	GlobalTabletCatalogLabBranch
	GlobalTabletCatalogLabRoot
)

// GlobalTabletCatalogLabNodeHeader is construction metadata. Page Kind is the
// outer common-page discriminator. Child Kind and fixed ChildLength make every
// packed child handle recoverable without storing duplicate logical IDs.
type GlobalTabletCatalogLabNodeHeader struct {
	StoreID    [16]byte
	Generation uint64
	LogicalID  uint64
	PageID     uint32
	Level      GlobalTabletCatalogLabNodeLevel
	Bounds     GlobalTabletCatalogLabBounds
	// RootChildLevel is Leaf in the normal two-level tree and Branch in the
	// rare adversarial three-level tree. It is ignored below the root.
	RootChildLevel GlobalTabletCatalogLabNodeLevel
	Kind           PageKind
	ChildKind      PageKind
	ChildLength    uint32
}

// GlobalTabletCatalogLabNodeEntry is one exact lexical floor and child. Floor
// zero must be empty. Subsequent floors are shortest exact separators.
type GlobalTabletCatalogLabNodeEntry struct {
	Floor []byte
	ID    uint32
	Ref   PageRef
}

// GlobalTabletCatalogLabNodeView borrows one common page, its exact front-coded
// floor map, and its packed physical handles.
type GlobalTabletCatalogLabNodeView struct {
	image       []byte
	payload     []byte
	handles     []byte
	heads       []byte
	floors      TabletAnchorMapLabView
	header      PageHeader
	level       GlobalTabletCatalogLabNodeLevel
	childLevel  GlobalTabletCatalogLabNodeLevel
	pageID      uint32
	childKind   PageKind
	childLength uint32
	headBytes   uint8
	bounds      GlobalTabletCatalogLabBounds
}

// GlobalTabletCatalogLabNodeRoute is the next cache acquisition. ID is a
// TabletID at a leaf, a leaf-page ID at a branch, or the selected child ID at
// a root.
type GlobalTabletCatalogLabNodeRoute struct {
	ID      uint32
	Ordinal uint16
	Ref     PageRef
}

type GlobalTabletCatalogLabNodeCursor struct {
	node   *GlobalTabletCatalogLabNodeView
	cursor TabletAnchorMapLabCursor
}

// GlobalTabletCatalogLabLocatorState occupies the high two bits of each packed
// 14-bit locator. The low twelve bits are pageID:4,rowSlot:8. Retired preserves
// the last location for validation/debugging; reuse remains conservatively
// gated by the selecting snapshot generation.
type GlobalTabletCatalogLabLocatorState uint8

const (
	GlobalTabletCatalogLabLocatorEmpty GlobalTabletCatalogLabLocatorState = iota
	GlobalTabletCatalogLabLocatorLive
	GlobalTabletCatalogLabLocatorRetired
	globalTabletCatalogLabLocatorReserved
)

type GlobalTabletCatalogLabLocatorEntry struct {
	LocalID uint16
	PageID  uint8
	RowSlot uint8
	State   GlobalTabletCatalogLabLocatorState
}

type GlobalTabletCatalogLabLocatorView struct {
	image      []byte
	packed     []byte
	header     PageHeader
	ref        PageRef
	tabletID   uint32
	live       uint16
	retired    uint16
	reuseFloor uint64
	bounds     GlobalTabletCatalogLabBounds
}

// GlobalTabletCatalogLabTabletRootView is an independently admitted 8 KiB
// wrapper around the proven segmented-root codec. The wrapper's common-page
// checksum binds a complete, discoverable locator PageRef to that root.
type GlobalTabletCatalogLabTabletRootView struct {
	image   []byte
	payload []byte
	header  PageHeader
	inner   globalTabletCatalogLabSegmentedRootView
	locator PageRef
	bounds  GlobalTabletCatalogLabBounds
}

// This compact root-only view deliberately excludes the segmented lab's
// [16]anchor-view array. A cached tablet root therefore retains borrowed byte
// slices and scalars only; selected anchor views exist only for selected cache
// frames and cannot create a per-tablet heap/object graph.
type globalTabletCatalogLabSegmentedRootView struct {
	root        []byte
	rootRefs    []byte
	rootRanks   []byte
	rootOffsets []byte
	rootKeys    []byte
	storeID     [16]byte
	tabletID    uint32
	generation  uint64
	pageCount   uint8
	anchorKind  PageKind
	leafKind    PageKind
}

type GlobalTabletCatalogLabAnchorRoute struct {
	PageID uint8
	Ref    PageRef
}

type GlobalTabletCatalogLabAnchorView struct {
	root *GlobalTabletCatalogLabTabletRootView
	page segmentedTabletRouterLabAnchorView
	ref  PageRef
}

// GlobalTabletCatalogLabSpace is an exact routing-only charge. CatalogBytes is
// supplied from the measured catalog tree because shortest separator lengths
// are workload dependent.
type GlobalTabletCatalogLabSpace struct {
	Tablets      uint64
	Leaves       uint64
	Documents    uint64
	TabletBytes  uint64
	CatalogBytes uint64
	TotalBytes   uint64
	BytesPerLeaf float64
	BytesPerDoc  float64
}

type GlobalTabletCatalogLabCatalogBounds struct {
	Tablets        uint64
	MaxFenceBytes  int
	LeafFanout     int
	RootFanout     int
	LeafPages      uint64
	BranchPages    uint64
	Levels         int
	PointPages     int
	COWBytes       uint64
	DiskBytes      uint64
	ResidentBytes  uint64
	MaximumTablets uint64
}

// GlobalTabletCatalogLabBounds is snapshot-owned admission context. StoreID
// lets common-page admission reject checksum-valid cross-Store grafts,
// SelectedRootGeneration rejects references born after the selecting snapshot,
// FileEnd rejects out-of-file references, and NextLogicalID bounds the
// allocated logical namespace. PageRef itself has no StoreID, so raw
// non-common-page acquisition must retain this Store context until admission.
type GlobalTabletCatalogLabBounds struct {
	StoreID                [16]byte
	SelectedRootGeneration uint64
	FileEnd                uint64
	NextLogicalID          uint64
}

func GlobalTabletCatalogLabLeafLogicalID(bucket BucketID) (uint64, bool) {
	if uint32(bucket) >= PrimaryBucketIDLimit {
		return 0, false
	}
	return GlobalTabletCatalogLabLeafLogicalIDBase + uint64(bucket), true
}

func GlobalTabletCatalogLabAnchorLogicalID(tabletID uint32, pageID uint8) (uint64, bool) {
	if tabletID >= TabletLocalIdentityLabTabletCount ||
		pageID >= SegmentedTabletRouterLabMaxPages {
		return 0, false
	}
	return GlobalTabletCatalogLabAnchorLogicalIDBase +
		uint64(tabletID)*SegmentedTabletRouterLabMaxPages + uint64(pageID), true
}

func GlobalTabletCatalogLabTabletRootLogicalID(tabletID uint32) (uint64, bool) {
	if tabletID >= TabletLocalIdentityLabTabletCount {
		return 0, false
	}
	return GlobalTabletCatalogLabTabletRootLogicalIDBase + uint64(tabletID), true
}

func GlobalTabletCatalogLabLocatorLogicalID(tabletID uint32) (uint64, bool) {
	if tabletID >= TabletLocalIdentityLabTabletCount {
		return 0, false
	}
	return GlobalTabletCatalogLabLocatorLogicalIDBase + uint64(tabletID), true
}

func GlobalTabletCatalogLabCatalogLeafLogicalID(pageID uint32) (uint64, bool) {
	if pageID >= GlobalTabletCatalogLabMaxLeafPages {
		return 0, false
	}
	return GlobalTabletCatalogLabLeafPageLogicalIDBase + uint64(pageID), true
}

func GlobalTabletCatalogLabCatalogBranchLogicalID(pageID uint32) (uint64, bool) {
	if pageID >= GlobalTabletCatalogLabMaxBranchPages {
		return 0, false
	}
	return GlobalTabletCatalogLabBranchPageLogicalIDBase + uint64(pageID), true
}

func GlobalTabletCatalogLabIsDynamicLogicalID(logicalID uint64) bool {
	return logicalID >= GlobalTabletCatalogLabFirstDynamicLogicalID
}

// GlobalTabletCatalogLabWorstCaseFanout returns a universal lower bound for
// valid floors no longer than maxFenceBytes. It charges two front-code bytes,
// the complete fence, offset/bucket metadata, one packed handle, both schema
// envelopes, and checks the real anchor-map geometry at every candidate count.
func GlobalTabletCatalogLabWorstCaseFanout(pageBytes, maxFenceBytes int) int {
	if pageBytes < GlobalTabletCatalogLabNodePayloadHeaderBytes+PageHeaderSize+PageTrailerSize ||
		maxFenceBytes <= 0 || maxFenceBytes > int(^uint16(0)) {
		return 0
	}
	for count := 1; count <= TabletAnchorMapLabMaxFences; count++ {
		fences := count - 1
		// A zero common prefix and no restart sharing is a valid upper bound
		// on the exact codec, independent of the corpus.
		keyBytes := fences * (2 + maxFenceBytes)
		if keyBytes > int(^uint16(0)) {
			return count - 1
		}
		mapBytes := tabletAnchorMapLabImageBytes(fences, 0, keyBytes)
		payload := GlobalTabletCatalogLabNodePayloadHeaderBytes + mapBytes +
			count*GlobalTabletCatalogLabHandleBytes
		if payload > pageBytes-PageHeaderSize-PageTrailerSize {
			return count - 1
		}
	}
	return TabletAnchorMapLabMaxFences
}

// GlobalTabletCatalogLabCatalogGeometry computes the guaranteed catalog shape
// at a fence-length bound. PointPages includes the resident root so callers can
// also read the number of cache misses as PointPages-1.
func GlobalTabletCatalogLabCatalogGeometry(
	tablets uint64, maxFenceBytes int,
) (GlobalTabletCatalogLabCatalogBounds, bool) {
	leafFanout := GlobalTabletCatalogLabWorstCaseFanout(
		GlobalTabletCatalogLabNodeBytes, maxFenceBytes,
	)
	rootFanout := GlobalTabletCatalogLabWorstCaseFanout(
		GlobalTabletCatalogLabRootBytes, maxFenceBytes,
	)
	if tablets == 0 || leafFanout == 0 || rootFanout == 0 {
		return GlobalTabletCatalogLabCatalogBounds{}, false
	}
	leafPages := (tablets + uint64(leafFanout) - 1) / uint64(leafFanout)
	bounds := GlobalTabletCatalogLabCatalogBounds{
		Tablets: tablets, MaxFenceBytes: maxFenceBytes,
		LeafFanout: leafFanout, RootFanout: rootFanout,
		LeafPages: leafPages, Levels: 2, PointPages: 2,
		COWBytes: GlobalTabletCatalogLabRootBytes +
			GlobalTabletCatalogLabNodeBytes,
		ResidentBytes: GlobalTabletCatalogLabRootBytes,
		MaximumTablets: uint64(rootFanout) * uint64(leafFanout) *
			uint64(leafFanout),
	}
	if leafPages > uint64(rootFanout) {
		bounds.BranchPages =
			(leafPages + uint64(leafFanout) - 1) / uint64(leafFanout)
		bounds.Levels = 3
		bounds.PointPages = 3
		bounds.COWBytes += GlobalTabletCatalogLabNodeBytes
		if bounds.BranchPages > uint64(rootFanout) {
			return GlobalTabletCatalogLabCatalogBounds{}, false
		}
	}
	bounds.DiskBytes = GlobalTabletCatalogLabRootBytes +
		(bounds.LeafPages+bounds.BranchPages)*GlobalTabletCatalogLabNodeBytes
	return bounds, true
}

func EncodeGlobalTabletCatalogLabNode(
	dst []byte,
	header GlobalTabletCatalogLabNodeHeader,
	entries []GlobalTabletCatalogLabNodeEntry,
) ([]byte, error) {
	pageBytes, err := globalTabletCatalogLabNodePageBytes(header.Level)
	if err != nil || len(dst) < pageBytes || len(entries) == 0 ||
		len(entries[0].Floor) != 0 ||
		header.StoreID == ([16]byte{}) ||
		header.Generation == 0 || header.Generation >= uint64(1)<<48 ||
		header.StoreID != header.Bounds.StoreID ||
		header.Generation > header.Bounds.SelectedRootGeneration ||
		!validPageKind(header.Kind) || !validPageKind(header.ChildKind) ||
		header.Kind == header.ChildKind ||
		header.LogicalID == 0 || !header.Bounds.valid() {
		return nil, fmt.Errorf("%w: catalog node identity or geometry", ErrInvalidWrite)
	}
	childLevel, childLevelOK := globalTabletCatalogLabChildLevel(
		header.Level, header.RootChildLevel,
	)
	wantLogicalID, wantChildLength, ok := globalTabletCatalogLabNodeIdentity(
		header.Level, header.PageID,
	)
	if !ok || !childLevelOK || header.LogicalID != wantLogicalID ||
		header.ChildLength != wantChildLength {
		return nil, fmt.Errorf("%w: catalog node namespace", ErrInvalidWrite)
	}
	anchors := make([]TabletAnchorMapLabAnchor, len(entries)-1)
	for at, entry := range entries {
		bucket, ok := globalTabletCatalogLabNodeBucket(
			header.Level, childLevel, entry.ID,
		)
		if !ok || at != 0 && len(entry.Floor) == 0 ||
			at != 0 && bytes.Compare(entries[at-1].Floor, entry.Floor) >= 0 {
			return nil, fmt.Errorf("%w: catalog node floor or child ID", ErrInvalidWrite)
		}
		for prior := 0; prior < at; prior++ {
			if entries[prior].ID == entry.ID {
				return nil, fmt.Errorf("%w: duplicate catalog child ID", ErrInvalidWrite)
			}
		}
		wantChild, ok := globalTabletCatalogLabChildLogicalID(
			header.Level, childLevel, entry.ID,
		)
		if !ok || globalTabletCatalogLabValidatePackedRef(
			entry.Ref, wantChild, header.ChildKind, header.ChildLength,
			header.Generation, header.Bounds,
		) != nil {
			return nil, fmt.Errorf("%w: catalog child reference", ErrInvalidWrite)
		}
		if at != 0 {
			anchors[at-1] = TabletAnchorMapLabAnchor{
				Fence: entry.Floor, Bucket: bucket,
			}
		}
	}
	common, _, keyBytes, err := tabletAnchorMapLabMeasure(anchors)
	if err != nil {
		return nil, err
	}
	mapBytes := tabletAnchorMapLabImageBytes(len(anchors), common, keyBytes)
	basePayloadBytes := GlobalTabletCatalogLabNodePayloadHeaderBytes + mapBytes +
		len(entries)*GlobalTabletCatalogLabHandleBytes
	headBytes := globalTabletCatalogLabChooseHeadBytes(
		entries, pageBytes-PageHeaderSize-PageTrailerSize-basePayloadBytes,
	)
	payloadBytes := basePayloadBytes + len(entries)*headBytes
	if payloadBytes > pageBytes-PageHeaderSize-PageTrailerSize {
		return nil, ErrGlobalTabletCatalogLabNoSpace
	}
	payload, err := InitPage(dst[:pageBytes], PageHeader{
		StoreID: header.StoreID, Generation: header.Generation,
		LogicalID: header.LogicalID, PageSize: uint32(pageBytes),
		PayloadLength: uint32(payloadBytes), Kind: header.Kind,
	})
	if err != nil {
		return nil, err
	}
	binary.LittleEndian.PutUint32(payload[0:4], globalTabletCatalogLabNodeVersion)
	payload[4] = byte(header.Level)
	payload[5] = byte(header.ChildKind)
	payload[6] = byte(childLevel)
	binary.LittleEndian.PutUint32(payload[8:12], header.PageID)
	binary.LittleEndian.PutUint32(payload[12:16], uint32(mapBytes))
	binary.LittleEndian.PutUint32(payload[16:20], header.ChildLength)
	binary.LittleEndian.PutUint16(payload[20:22], GlobalTabletCatalogLabHandleBytes)
	binary.LittleEndian.PutUint16(payload[22:24], uint16(len(entries)))
	payload[24] = byte(headBytes)
	firstBucket, _ := globalTabletCatalogLabNodeBucket(
		header.Level, childLevel, entries[0].ID,
	)
	if _, err := EncodeTabletAnchorMapLab(
		payload[GlobalTabletCatalogLabNodePayloadHeaderBytes:GlobalTabletCatalogLabNodePayloadHeaderBytes+mapBytes],
		TabletAnchorMapLabHeader{
			TabletID: header.LogicalID, Generation: header.Generation,
		},
		firstBucket, anchors,
	); err != nil {
		return nil, err
	}
	handles := payload[GlobalTabletCatalogLabNodePayloadHeaderBytes+mapBytes : basePayloadBytes]
	for at, entry := range entries {
		globalTabletCatalogLabEncodePackedRef(
			handles[at*GlobalTabletCatalogLabHandleBytes:], entry.Ref,
		)
	}
	heads := payload[basePayloadBytes:]
	for at := 1; at < len(entries) && headBytes != 0; at++ {
		copy(heads[at*headBytes:], entries[at].Floor[:headBytes])
	}
	if _, err := sealInitializedPage(dst[:pageBytes]); err != nil {
		return nil, err
	}
	return dst[:pageBytes], nil
}

func OpenGlobalTabletCatalogLabNode(
	src []byte, expected PageRef, bounds GlobalTabletCatalogLabBounds,
) (GlobalTabletCatalogLabNodeView, error) {
	var view GlobalTabletCatalogLabNodeView
	header, payload, err := OpenPage(src)
	if err != nil || !bounds.valid() ||
		!globalTabletCatalogLabHeaderMatchesRef(header, expected, bounds) ||
		len(payload) < GlobalTabletCatalogLabNodePayloadHeaderBytes ||
		binary.LittleEndian.Uint32(payload[0:4]) != globalTabletCatalogLabNodeVersion ||
		payload[4] > byte(GlobalTabletCatalogLabRoot) ||
		payload[6] > byte(GlobalTabletCatalogLabBranch) ||
		!validPageKind(PageKind(payload[5])) ||
		binary.LittleEndian.Uint16(payload[20:22]) != GlobalTabletCatalogLabHandleBytes ||
		payload[7] != 0 ||
		payload[24] != 0 && payload[24] != 1 &&
			payload[24] != 2 && payload[24] != 4 ||
		!allZero(payload[25:GlobalTabletCatalogLabNodePayloadHeaderBytes]) {
		return view, globalTabletCatalogLabCorrupt("node header")
	}
	level := GlobalTabletCatalogLabNodeLevel(payload[4])
	childLevel, childLevelOK := globalTabletCatalogLabChildLevel(
		level, GlobalTabletCatalogLabNodeLevel(payload[6]),
	)
	pageID := binary.LittleEndian.Uint32(payload[8:12])
	wantLogicalID, childLength, ok := globalTabletCatalogLabNodeIdentity(level, pageID)
	count := int(binary.LittleEndian.Uint16(payload[22:24]))
	headBytes := int(payload[24])
	mapBytes := int(binary.LittleEndian.Uint32(payload[12:16]))
	if !ok || !childLevelOK || header.LogicalID != wantLogicalID || count == 0 ||
		childLength != binary.LittleEndian.Uint32(payload[16:20]) ||
		mapBytes < TabletAnchorMapLabHeaderSize+TabletAnchorMapLabTrailerSize ||
		GlobalTabletCatalogLabNodePayloadHeaderBytes+mapBytes+
			count*(GlobalTabletCatalogLabHandleBytes+headBytes) != len(payload) {
		return GlobalTabletCatalogLabNodeView{},
			globalTabletCatalogLabCorrupt("node identity or sections")
	}
	floors, err := OpenTabletAnchorMapLab(
		payload[GlobalTabletCatalogLabNodePayloadHeaderBytes : GlobalTabletCatalogLabNodePayloadHeaderBytes+mapBytes],
	)
	if err != nil || floors.Header().TabletID != header.LogicalID ||
		floors.Header().Generation != header.Generation ||
		floors.BucketCount() != count {
		return GlobalTabletCatalogLabNodeView{},
			globalTabletCatalogLabCorrupt("node floor map")
	}
	handleAt := GlobalTabletCatalogLabNodePayloadHeaderBytes + mapBytes
	headAt := handleAt + count*GlobalTabletCatalogLabHandleBytes
	view = GlobalTabletCatalogLabNodeView{
		image: src[:header.PageSize], payload: payload,
		handles: payload[handleAt:headAt],
		heads:   payload[headAt:],
		floors:  floors, header: header, level: level, pageID: pageID,
		childLevel: childLevel, childKind: PageKind(payload[5]),
		childLength: childLength, headBytes: uint8(headBytes), bounds: bounds,
	}
	if headBytes != 0 && !allZero(view.heads[:headBytes]) {
		return GlobalTabletCatalogLabNodeView{},
			globalTabletCatalogLabCorrupt("node first head")
	}
	for ordinal := 0; ordinal < count; ordinal++ {
		bucket := floors.bucketAt(ordinal)
		id, ok := globalTabletCatalogLabNodeID(level, childLevel, bucket)
		wantChild, childOK := globalTabletCatalogLabChildLogicalID(
			level, childLevel, id,
		)
		ref, refOK := view.refAt(ordinal, id)
		if !ok || !childOK || !refOK ||
			ref.LogicalID != wantChild {
			return GlobalTabletCatalogLabNodeView{},
				globalTabletCatalogLabCorrupt("node child binding")
		}
		for prior := 0; prior < ordinal; prior++ {
			if floors.bucketAt(prior) == bucket {
				return GlobalTabletCatalogLabNodeView{},
					globalTabletCatalogLabCorrupt("duplicate node child ID")
			}
		}
		if ordinal != 0 && headBytes != 0 {
			common, restart, suffix, floorOK := floors.FenceAt(ordinal - 1)
			if !floorOK || len(common)+len(restart)+len(suffix) < headBytes ||
				!globalTabletCatalogLabHeadMatches(
					view.heads[ordinal*headBytes:], headBytes,
					common, restart, suffix,
				) {
				return GlobalTabletCatalogLabNodeView{},
					globalTabletCatalogLabCorrupt("node head accelerator")
			}
		}
	}
	return view, nil
}

func (v *GlobalTabletCatalogLabNodeView) Level() GlobalTabletCatalogLabNodeLevel {
	if v == nil {
		return GlobalTabletCatalogLabLeaf
	}
	return v.level
}

func (v *GlobalTabletCatalogLabNodeView) Count() int {
	if v == nil {
		return 0
	}
	return v.floors.BucketCount()
}

func (v *GlobalTabletCatalogLabNodeView) upperBound(key []byte) int {
	if v == nil {
		return 0
	}
	if v.headBytes == 0 || len(key) < int(v.headBytes) {
		return v.floors.upperBound(key)
	}
	headBytes := int(v.headBytes)
	low, high := 1, v.Count()
	for low < high {
		mid := int(uint(low+high) >> 1)
		order := bytes.Compare(
			v.heads[mid*headBytes:(mid+1)*headBytes],
			key[:headBytes],
		)
		if order == 0 {
			// Equal shortened heads are not identities. The exact admitted
			// floor map has a first-byte accelerator and is faster for the
			// remaining collision class than reconstructing every midpoint.
			return v.floors.upperBound(key)
		}
		if order < 0 {
			low = mid + 1
		} else {
			high = mid
		}
	}
	return low - 1
}

func (v *GlobalTabletCatalogLabNodeView) Route(key []byte) GlobalTabletCatalogLabNodeRoute {
	if v == nil || len(v.image) == 0 {
		return GlobalTabletCatalogLabNodeRoute{}
	}
	ordinal := v.upperBound(key)
	bucket := v.floors.bucketAt(ordinal)
	id, ok := globalTabletCatalogLabNodeID(v.level, v.childLevel, bucket)
	ref, refOK := v.refAt(ordinal, id)
	if !ok || !refOK {
		return GlobalTabletCatalogLabNodeRoute{}
	}
	return GlobalTabletCatalogLabNodeRoute{ID: id, Ordinal: uint16(ordinal), Ref: ref}
}

func (v *GlobalTabletCatalogLabNodeView) LowerBound(
	key []byte,
) GlobalTabletCatalogLabNodeCursor {
	if v == nil {
		return GlobalTabletCatalogLabNodeCursor{}
	}
	return GlobalTabletCatalogLabNodeCursor{
		node: v, cursor: v.floors.LowerBound(key),
	}
}

func (c *GlobalTabletCatalogLabNodeCursor) Route() (GlobalTabletCatalogLabNodeRoute, bool) {
	if c == nil {
		return GlobalTabletCatalogLabNodeRoute{}, false
	}
	bucket, ok := c.cursor.Bucket()
	ordinal, ordinalOK := c.cursor.Ordinal()
	if c.node == nil {
		return GlobalTabletCatalogLabNodeRoute{}, false
	}
	id, idOK := globalTabletCatalogLabNodeID(
		c.node.level, c.node.childLevel, bucket,
	)
	ref, refOK := c.node.refAt(ordinal, id)
	if !ok || !ordinalOK || !idOK || !refOK {
		return GlobalTabletCatalogLabNodeRoute{}, false
	}
	return GlobalTabletCatalogLabNodeRoute{
		ID: id, Ordinal: uint16(ordinal), Ref: ref,
	}, true
}

func (c *GlobalTabletCatalogLabNodeCursor) Next() bool {
	return c != nil && c.cursor.Next()
}

// RewriteHandle performs an immutable non-structural catalog update. Bounds
// are the destination snapshot's allocation bounds and must monotonically
// extend the source view. A tablet root replacement rewrites its 8 KiB catalog
// leaf, every selected 8 KiB branch (if present), and the 64 KiB catalog root.
// Floors are unchanged.
func (v *GlobalTabletCatalogLabNodeView) RewriteHandle(
	dst []byte, generation uint64, bounds GlobalTabletCatalogLabBounds,
	id uint32, replacement PageRef,
) ([]byte, error) {
	if v == nil || len(v.image) == 0 || generation <= v.header.Generation ||
		generation >= uint64(1)<<48 || len(dst) < len(v.image) ||
		!bounds.extends(v.bounds) {
		return nil, fmt.Errorf("%w: catalog COW generation or destination", ErrInvalidWrite)
	}
	if globalTabletCatalogLabSlicesOverlap(dst[:len(v.image)], v.image) {
		return nil, fmt.Errorf("%w: catalog COW source/destination overlap", ErrInvalidWrite)
	}
	wantChild, ok := globalTabletCatalogLabChildLogicalID(
		v.level, v.childLevel, id,
	)
	if !ok || globalTabletCatalogLabValidatePackedRef(
		replacement, wantChild, v.childKind, v.childLength, generation, bounds,
	) != nil {
		return nil, fmt.Errorf("%w: catalog COW child", ErrInvalidWrite)
	}
	ordinal := -1
	for at := 0; at < v.Count(); at++ {
		candidate, valid := globalTabletCatalogLabNodeID(
			v.level, v.childLevel, v.floors.bucketAt(at),
		)
		if valid && candidate == id {
			ordinal = at
			break
		}
	}
	if ordinal < 0 {
		return nil, fmt.Errorf("%w: catalog COW child not found", ErrInvalidWrite)
	}
	payload, err := InitPage(dst[:len(v.image)], PageHeader{
		StoreID: v.header.StoreID, Generation: generation,
		LogicalID: v.header.LogicalID, PageSize: v.header.PageSize,
		PayloadLength: v.header.PayloadLength, Kind: v.header.Kind,
	})
	if err != nil {
		return nil, err
	}
	copy(payload, v.payload)
	mapBytes := int(binary.LittleEndian.Uint32(payload[12:16]))
	mapStart := GlobalTabletCatalogLabNodePayloadHeaderBytes
	mapEnd := mapStart + mapBytes
	if mapBytes < TabletAnchorMapLabHeaderSize+TabletAnchorMapLabTrailerSize ||
		mapEnd > len(payload) {
		return nil, globalTabletCatalogLabCorrupt("COW floor-map geometry")
	}
	// The floor map is embedded, so its identity is the enclosing node's
	// physical birth. Refreshing it makes equal COW nodes byte-identical even
	// when their unchanged floors came from different ancestors.
	binary.LittleEndian.PutUint64(
		payload[mapStart+24:mapStart+32], generation,
	)
	tabletAnchorMapLabSeal(payload[mapStart:mapEnd])
	globalTabletCatalogLabEncodePackedRef(
		payload[GlobalTabletCatalogLabNodePayloadHeaderBytes+
			len(v.floors.image)+
			ordinal*GlobalTabletCatalogLabHandleBytes:],
		replacement,
	)
	if _, err := sealInitializedPage(dst[:len(v.image)]); err != nil {
		return nil, err
	}
	return dst[:len(v.image)], nil
}

func (v *GlobalTabletCatalogLabNodeView) refAt(
	ordinal int, id uint32,
) (PageRef, bool) {
	if v == nil || ordinal < 0 || ordinal >= v.Count() {
		return PageRef{}, false
	}
	logicalID, ok := globalTabletCatalogLabChildLogicalID(
		v.level, v.childLevel, id,
	)
	if !ok {
		return PageRef{}, false
	}
	src := v.handles[ordinal*GlobalTabletCatalogLabHandleBytes:]
	ref := PageRef{
		Offset:     segmentedTabletRouterLabGetUint48(src) << 3,
		LogicalID:  logicalID,
		Generation: segmentedTabletRouterLabGetUint48(src[6:]),
		Length:     v.childLength, Kind: v.childKind,
	}
	return ref, globalTabletCatalogLabValidatePackedRef(
		ref, logicalID, v.childKind, v.childLength,
		v.bounds.SelectedRootGeneration, v.bounds,
	) == nil
}

func EncodeGlobalTabletCatalogLabLocator(
	dst []byte, header PageHeader, bounds GlobalTabletCatalogLabBounds,
	tabletID uint32, reuseFloor uint64,
	entries []GlobalTabletCatalogLabLocatorEntry,
) ([]byte, error) {
	logicalID, ok := GlobalTabletCatalogLabLocatorLogicalID(tabletID)
	payloadLength := GlobalTabletCatalogLabLocatorHeader + globalTabletCatalogLabPackedBytes
	if !ok || len(dst) < GlobalTabletCatalogLabLocatorBytes ||
		header.LogicalID != logicalID ||
		header.PageSize != GlobalTabletCatalogLabLocatorBytes ||
		header.PayloadLength != uint32(payloadLength) ||
		header.Generation == 0 || header.Generation >= uint64(1)<<48 ||
		header.StoreID != bounds.StoreID ||
		header.Generation > bounds.SelectedRootGeneration ||
		reuseFloor > header.Generation || !bounds.valid() ||
		header.LogicalID >= bounds.NextLogicalID {
		return nil, fmt.Errorf("%w: compact locator identity", ErrInvalidWrite)
	}
	payload, err := InitPage(dst[:GlobalTabletCatalogLabLocatorBytes], header)
	if err != nil {
		return nil, err
	}
	binary.LittleEndian.PutUint32(payload[0:4], globalTabletCatalogLabLocatorVersion)
	binary.LittleEndian.PutUint32(payload[4:8], tabletID)
	binary.LittleEndian.PutUint64(payload[16:24], reuseFloor)
	packed := payload[GlobalTabletCatalogLabLocatorHeader:]
	var live, retired uint16
	var previous uint16
	for at, entry := range entries {
		if entry.LocalID >= TabletLocalIdentityLabLocalCount ||
			at != 0 && entry.LocalID <= previous ||
			entry.PageID >= SegmentedTabletRouterLabMaxPages ||
			entry.State != GlobalTabletCatalogLabLocatorLive &&
				entry.State != GlobalTabletCatalogLabLocatorRetired {
			return nil, fmt.Errorf("%w: compact locator entry", ErrInvalidWrite)
		}
		code := uint16(entry.State)<<12 |
			uint16(entry.PageID)<<8 | uint16(entry.RowSlot)
		globalTabletCatalogLabPut14(packed, entry.LocalID, code)
		if entry.State == GlobalTabletCatalogLabLocatorLive {
			live++
		} else {
			retired++
		}
		previous = entry.LocalID
	}
	binary.LittleEndian.PutUint16(payload[8:10], live)
	binary.LittleEndian.PutUint16(payload[10:12], retired)
	if _, err := sealInitializedPage(dst[:GlobalTabletCatalogLabLocatorBytes]); err != nil {
		return nil, err
	}
	return dst[:GlobalTabletCatalogLabLocatorBytes], nil
}

func OpenGlobalTabletCatalogLabLocator(
	src []byte, expected PageRef, bounds GlobalTabletCatalogLabBounds,
) (GlobalTabletCatalogLabLocatorView, error) {
	var view GlobalTabletCatalogLabLocatorView
	header, payload, err := OpenPage(src)
	if err != nil ||
		!globalTabletCatalogLabHeaderMatchesRef(header, expected, bounds) ||
		len(payload) != GlobalTabletCatalogLabLocatorHeader+
			globalTabletCatalogLabPackedBytes ||
		binary.LittleEndian.Uint32(payload[0:4]) != globalTabletCatalogLabLocatorVersion ||
		!allZero(payload[12:16]) || !allZero(payload[24:GlobalTabletCatalogLabLocatorHeader]) {
		return view, globalTabletCatalogLabCorrupt("locator header")
	}
	tabletID := binary.LittleEndian.Uint32(payload[4:8])
	logicalID, ok := GlobalTabletCatalogLabLocatorLogicalID(tabletID)
	reuseFloor := binary.LittleEndian.Uint64(payload[16:24])
	if !ok || header.LogicalID != logicalID || reuseFloor > header.Generation {
		return GlobalTabletCatalogLabLocatorView{},
			globalTabletCatalogLabCorrupt("locator identity")
	}
	view = GlobalTabletCatalogLabLocatorView{
		image: src[:header.PageSize], packed: payload[GlobalTabletCatalogLabLocatorHeader:],
		header: header, ref: expected, tabletID: tabletID,
		live:       binary.LittleEndian.Uint16(payload[8:10]),
		retired:    binary.LittleEndian.Uint16(payload[10:12]),
		reuseFloor: reuseFloor,
		bounds:     bounds,
	}
	var live, retired uint16
	for localID := uint16(0); localID < TabletLocalIdentityLabLocalCount; localID++ {
		code := globalTabletCatalogLabGet14(view.packed, localID)
		state := GlobalTabletCatalogLabLocatorState(code >> 12)
		switch state {
		case GlobalTabletCatalogLabLocatorEmpty:
			if code&0x0fff != 0 {
				return GlobalTabletCatalogLabLocatorView{},
					globalTabletCatalogLabCorrupt("non-canonical empty locator")
			}
		case GlobalTabletCatalogLabLocatorLive:
			live++
		case GlobalTabletCatalogLabLocatorRetired:
			retired++
		default:
			return GlobalTabletCatalogLabLocatorView{},
				globalTabletCatalogLabCorrupt("reserved locator state")
		}
	}
	if live != view.live || retired != view.retired {
		return GlobalTabletCatalogLabLocatorView{},
			globalTabletCatalogLabCorrupt("locator cardinality")
	}
	return view, nil
}

func (v *GlobalTabletCatalogLabLocatorView) Resolve(
	localID uint16,
) (pageID, rowSlot uint8, state GlobalTabletCatalogLabLocatorState) {
	if v == nil || len(v.image) == 0 || localID >= TabletLocalIdentityLabLocalCount {
		return 0, 0, GlobalTabletCatalogLabLocatorEmpty
	}
	code := globalTabletCatalogLabGet14(v.packed, localID)
	return uint8(code >> 8 & 0x0f), uint8(code), GlobalTabletCatalogLabLocatorState(code >> 12)
}

// EncodeGlobalTabletCatalogLabTabletRoot wraps one validated segmented root.
// The complete locator reference is encoded, checksum-bound, and recoverable.
func EncodeGlobalTabletCatalogLabTabletRoot(
	dst []byte, header PageHeader, bounds GlobalTabletCatalogLabBounds,
	locator PageRef, segmentedRoot []byte,
) ([]byte, error) {
	inner, err := globalTabletCatalogLabOpenSegmentedRootOnly(segmentedRoot)
	logicalID, ok := GlobalTabletCatalogLabTabletRootLogicalID(inner.tabletID)
	locatorLogical, locatorOK := GlobalTabletCatalogLabLocatorLogicalID(inner.tabletID)
	payloadLength := GlobalTabletCatalogLabRootHeader + SegmentedTabletRouterLabRootBytes
	if err != nil || !ok || !locatorOK ||
		len(dst) < GlobalTabletCatalogLabTabletBytes ||
		header.LogicalID != logicalID ||
		header.Generation != inner.generation ||
		header.PageSize != GlobalTabletCatalogLabTabletBytes ||
		header.PayloadLength != uint32(payloadLength) ||
		header.StoreID != bounds.StoreID ||
		inner.storeID != header.StoreID ||
		header.Generation > bounds.SelectedRootGeneration ||
		globalTabletCatalogLabValidateFullRef(
			locator, locatorLogical, locator.Kind,
			GlobalTabletCatalogLabLocatorBytes, header.Generation, bounds,
		) != nil ||
		locator.Kind == header.Kind || !bounds.valid() ||
		header.LogicalID >= bounds.NextLogicalID ||
		globalTabletCatalogLabRootRefsWithinBounds(inner, bounds) != nil {
		return nil, fmt.Errorf("%w: cacheable tablet-root identity", ErrInvalidWrite)
	}
	payload, err := InitPage(dst[:GlobalTabletCatalogLabTabletBytes], header)
	if err != nil {
		return nil, err
	}
	binary.LittleEndian.PutUint32(payload[0:4], globalTabletCatalogLabRootVersion)
	binary.LittleEndian.PutUint32(payload[4:8], inner.tabletID)
	binary.LittleEndian.PutUint32(payload[8:12], SegmentedTabletRouterLabRootBytes)
	encodePageRef(payload[16:16+PageRefSize], locator)
	copy(payload[GlobalTabletCatalogLabRootHeader:], segmentedRoot)
	if _, err := sealInitializedPage(dst[:GlobalTabletCatalogLabTabletBytes]); err != nil {
		return nil, err
	}
	return dst[:GlobalTabletCatalogLabTabletBytes], nil
}

func OpenGlobalTabletCatalogLabTabletRoot(
	src []byte, expected PageRef, bounds GlobalTabletCatalogLabBounds,
) (GlobalTabletCatalogLabTabletRootView, error) {
	var view GlobalTabletCatalogLabTabletRootView
	header, payload, err := OpenPage(src)
	if err != nil ||
		!globalTabletCatalogLabHeaderMatchesRef(header, expected, bounds) ||
		len(payload) != GlobalTabletCatalogLabRootHeader+
			SegmentedTabletRouterLabRootBytes ||
		binary.LittleEndian.Uint32(payload[0:4]) != globalTabletCatalogLabRootVersion ||
		binary.LittleEndian.Uint32(payload[8:12]) != SegmentedTabletRouterLabRootBytes ||
		!allZero(payload[12:16]) ||
		!pageRefReservedZero(payload[16:16+PageRefSize]) ||
		!allZero(payload[16+PageRefSize:GlobalTabletCatalogLabRootHeader]) {
		return view, globalTabletCatalogLabCorrupt("tablet root wrapper")
	}
	inner, err := globalTabletCatalogLabOpenSegmentedRootOnly(
		payload[GlobalTabletCatalogLabRootHeader:],
	)
	tabletID := binary.LittleEndian.Uint32(payload[4:8])
	logicalID, ok := GlobalTabletCatalogLabTabletRootLogicalID(tabletID)
	locatorLogical, locatorOK := GlobalTabletCatalogLabLocatorLogicalID(tabletID)
	locator := decodePageRef(payload[16 : 16+PageRefSize])
	if err != nil || tabletID != inner.tabletID || !ok || !locatorOK ||
		inner.storeID != header.StoreID ||
		header.LogicalID != logicalID || header.Generation != inner.generation ||
		globalTabletCatalogLabValidateFullRef(
			locator, locatorLogical, locator.Kind,
			GlobalTabletCatalogLabLocatorBytes, header.Generation, bounds,
		) != nil ||
		locator.Kind == header.Kind ||
		globalTabletCatalogLabRootRefsWithinBounds(inner, bounds) != nil {
		return GlobalTabletCatalogLabTabletRootView{},
			globalTabletCatalogLabCorrupt("tablet root binding")
	}
	return GlobalTabletCatalogLabTabletRootView{
		image: src[:header.PageSize], payload: payload, header: header,
		inner: inner, locator: locator, bounds: bounds,
	}, nil
}

func (v *GlobalTabletCatalogLabTabletRootView) LocatorRef() (PageRef, bool) {
	if v == nil {
		return PageRef{}, false
	}
	return v.locator, len(v.image) != 0
}

// RouteAnchor completes the tablet-root half of a point lookup. The caller
// acquires exactly the returned anchor page; no locator is touched.
func (v *GlobalTabletCatalogLabTabletRootView) RouteAnchor(
	key []byte,
) (GlobalTabletCatalogLabAnchorRoute, bool) {
	if v == nil || len(v.image) == 0 {
		return GlobalTabletCatalogLabAnchorRoute{}, false
	}
	rank := v.inner.rootUpperBound(key) - 1
	pageID := v.inner.rootRanks[rank]
	ref, ok := v.inner.anchorRef(pageID)
	return GlobalTabletCatalogLabAnchorRoute{PageID: pageID, Ref: ref}, ok
}

func OpenGlobalTabletCatalogLabAnchor(
	src []byte, root *GlobalTabletCatalogLabTabletRootView, pageID uint8,
) (GlobalTabletCatalogLabAnchorView, error) {
	// PageRef does not carry StoreID. The independently admitted tablet root
	// supplies the Store fence and the common anchor envelope must match it.
	if root == nil || len(root.image) == 0 || !root.bounds.valid() ||
		root.header.StoreID != root.bounds.StoreID ||
		root.header.Generation > root.bounds.SelectedRootGeneration {
		return GlobalTabletCatalogLabAnchorView{},
			globalTabletCatalogLabCorrupt("anchor without root")
	}
	ref, ok := root.inner.anchorRef(pageID)
	if !ok || globalTabletCatalogLabValidateFullRef(
		ref, ref.LogicalID, ref.Kind, SegmentedTabletRouterLabAnchorPageBytes,
		root.header.Generation, root.bounds,
	) != nil {
		return GlobalTabletCatalogLabAnchorView{},
			globalTabletCatalogLabCorrupt("anchor reference")
	}
	compat := root.inner.segmentedView()
	page, err := segmentedTabletRouterLabOpenAnchor(src, compat, pageID, ref)
	if err != nil {
		return GlobalTabletCatalogLabAnchorView{}, err
	}
	for rank := 0; rank < int(page.count); rank++ {
		slot := page.ranks[rank]
		localID := binary.LittleEndian.Uint16(page.localIDs[int(slot)*2:])
		bucket, bucketOK := MakeTabletLocalIdentityLabBucket(
			root.inner.tabletID, uint32(localID),
		)
		leaf, _, leafOK := page.handleAt(slot, BucketID(bucket))
		if !bucketOK || !leafOK || !root.bounds.contains(leaf) {
			return GlobalTabletCatalogLabAnchorView{},
				globalTabletCatalogLabCorrupt("anchor leaf bounds")
		}
	}
	return GlobalTabletCatalogLabAnchorView{root: root, page: page, ref: ref}, nil
}

func (v *GlobalTabletCatalogLabAnchorView) RouteHashed(
	hash uint64, key []byte,
) (SegmentedTabletRouterLabRoute, bool) {
	if v == nil || len(v.page.image) == 0 {
		return SegmentedTabletRouterLabRoute{}, false
	}
	return v.page.routeAt(v.page.upperBound(key)-1, hash), true
}

// ResolveBucket is the posting path. It verifies tablet identity, live locator
// state, selected anchor PageRef, and the inverse LocalID row binding.
func (v *GlobalTabletCatalogLabAnchorView) ResolveBucket(
	locator *GlobalTabletCatalogLabLocatorView, bucket BucketID,
) (PageRef, BucketZone, bool) {
	if v == nil || v.root == nil || locator == nil {
		return PageRef{}, BucketZone{}, false
	}
	tabletID, localID, ok := SplitTabletLocalIdentityLabBucket(uint32(bucket))
	if !ok || tabletID != v.root.inner.tabletID || locator.tabletID != tabletID {
		return PageRef{}, BucketZone{}, false
	}
	if locator.ref != v.root.locator {
		return PageRef{}, BucketZone{}, false
	}
	pageID, rowSlot, state := locator.Resolve(localID)
	if state != GlobalTabletCatalogLabLocatorLive || pageID != v.page.pageID ||
		binary.LittleEndian.Uint16(v.page.localIDs[int(rowSlot)*2:]) != localID {
		return PageRef{}, BucketZone{}, false
	}
	return v.page.handleAt(rowSlot, bucket)
}

func GlobalTabletCatalogLabRoutingSpace(
	documents, rowsPerLeaf, leavesPerTablet, catalogBytes uint64,
) (GlobalTabletCatalogLabSpace, bool) {
	if documents == 0 || rowsPerLeaf == 0 || leavesPerTablet == 0 {
		return GlobalTabletCatalogLabSpace{}, false
	}
	leaves := (documents + rowsPerLeaf - 1) / rowsPerLeaf
	tablets := (leaves + leavesPerTablet - 1) / leavesPerTablet
	tabletBytes := tablets * (GlobalTabletCatalogLabTabletBytes +
		GlobalTabletCatalogLabLocatorBytes +
		SegmentedTabletRouterLabMaxPages*SegmentedTabletRouterLabAnchorPageBytes)
	total := tabletBytes + catalogBytes
	return GlobalTabletCatalogLabSpace{
		Tablets: tablets, Leaves: leaves, Documents: documents,
		TabletBytes: tabletBytes, CatalogBytes: catalogBytes, TotalBytes: total,
		BytesPerLeaf: float64(total) / float64(leaves),
		BytesPerDoc:  float64(total) / float64(documents),
	}, true
}

func globalTabletCatalogLabNodePageBytes(
	level GlobalTabletCatalogLabNodeLevel,
) (int, error) {
	switch level {
	case GlobalTabletCatalogLabLeaf, GlobalTabletCatalogLabBranch:
		return GlobalTabletCatalogLabNodeBytes, nil
	case GlobalTabletCatalogLabRoot:
		return GlobalTabletCatalogLabRootBytes, nil
	default:
		return 0, fmt.Errorf("%w: catalog node level", ErrInvalidWrite)
	}
}

func globalTabletCatalogLabChooseHeadBytes(
	entries []GlobalTabletCatalogLabNodeEntry, spare int,
) int {
	for _, width := range [...]int{4, 2, 1} {
		if spare < len(entries)*width {
			continue
		}
		valid := true
		for at := 1; at < len(entries); at++ {
			if len(entries[at].Floor) < width {
				valid = false
				break
			}
		}
		if valid {
			return width
		}
	}
	return 0
}

func globalTabletCatalogLabHeadMatches(
	head []byte, width int, parts ...[]byte,
) bool {
	at := 0
	for _, part := range parts {
		for _, value := range part {
			if at == width {
				return true
			}
			if head[at] != value {
				return false
			}
			at++
		}
	}
	return at == width
}

func globalTabletCatalogLabNodeIdentity(
	level GlobalTabletCatalogLabNodeLevel, pageID uint32,
) (uint64, uint32, bool) {
	switch level {
	case GlobalTabletCatalogLabLeaf:
		if pageID >= GlobalTabletCatalogLabMaxLeafPages {
			return 0, 0, false
		}
		id, _ := GlobalTabletCatalogLabCatalogLeafLogicalID(pageID)
		return id, GlobalTabletCatalogLabTabletBytes, true
	case GlobalTabletCatalogLabBranch:
		if pageID >= GlobalTabletCatalogLabMaxBranchPages {
			return 0, 0, false
		}
		id, _ := GlobalTabletCatalogLabCatalogBranchLogicalID(pageID)
		return id, GlobalTabletCatalogLabNodeBytes, true
	case GlobalTabletCatalogLabRoot:
		if pageID != 0 {
			return 0, 0, false
		}
		// The root may point directly to leaves or to rare branches. The
		// length is identical; child logical derivation uses the encoded level.
		return GlobalTabletCatalogLabRootLogicalID, GlobalTabletCatalogLabNodeBytes, true
	default:
		return 0, 0, false
	}
}

func globalTabletCatalogLabChildLevel(
	level, requested GlobalTabletCatalogLabNodeLevel,
) (GlobalTabletCatalogLabNodeLevel, bool) {
	switch level {
	case GlobalTabletCatalogLabLeaf, GlobalTabletCatalogLabBranch:
		return GlobalTabletCatalogLabLeaf, true
	case GlobalTabletCatalogLabRoot:
		if requested == GlobalTabletCatalogLabLeaf ||
			requested == GlobalTabletCatalogLabBranch {
			return requested, true
		}
		return 0, false
	default:
		return 0, false
	}
}

func globalTabletCatalogLabNodeBucket(
	level, childLevel GlobalTabletCatalogLabNodeLevel, id uint32,
) (BucketID, bool) {
	switch level {
	case GlobalTabletCatalogLabLeaf:
		bucket, ok := MakeTabletLocalIdentityLabBucket(id, 0)
		return BucketID(bucket), ok
	case GlobalTabletCatalogLabBranch:
		return BucketID(id), id < GlobalTabletCatalogLabMaxLeafPages
	case GlobalTabletCatalogLabRoot:
		if childLevel == GlobalTabletCatalogLabLeaf {
			return BucketID(id), id < GlobalTabletCatalogLabMaxLeafPages
		}
		return BucketID(id), childLevel == GlobalTabletCatalogLabBranch &&
			id < GlobalTabletCatalogLabMaxBranchPages
	default:
		return 0, false
	}
}

func globalTabletCatalogLabNodeID(
	level, childLevel GlobalTabletCatalogLabNodeLevel, bucket BucketID,
) (uint32, bool) {
	switch level {
	case GlobalTabletCatalogLabLeaf:
		tabletID, localID, ok := SplitTabletLocalIdentityLabBucket(uint32(bucket))
		return tabletID, ok && localID == 0
	case GlobalTabletCatalogLabBranch:
		id := uint32(bucket)
		return id, id < GlobalTabletCatalogLabMaxLeafPages
	case GlobalTabletCatalogLabRoot:
		id := uint32(bucket)
		if childLevel == GlobalTabletCatalogLabLeaf {
			return id, id < GlobalTabletCatalogLabMaxLeafPages
		}
		return id, childLevel == GlobalTabletCatalogLabBranch &&
			id < GlobalTabletCatalogLabMaxBranchPages
	default:
		return 0, false
	}
}

func globalTabletCatalogLabChildLogicalID(
	level, childLevel GlobalTabletCatalogLabNodeLevel, id uint32,
) (uint64, bool) {
	switch level {
	case GlobalTabletCatalogLabLeaf:
		return GlobalTabletCatalogLabTabletRootLogicalID(id)
	case GlobalTabletCatalogLabBranch:
		return GlobalTabletCatalogLabCatalogLeafLogicalID(id)
	case GlobalTabletCatalogLabRoot:
		if childLevel == GlobalTabletCatalogLabLeaf {
			return GlobalTabletCatalogLabCatalogLeafLogicalID(id)
		}
		if childLevel == GlobalTabletCatalogLabBranch {
			return GlobalTabletCatalogLabCatalogBranchLogicalID(id)
		}
		return 0, false
	default:
		return 0, false
	}
}

func globalTabletCatalogLabValidatePackedRef(
	ref PageRef, logicalID uint64, kind PageKind, length uint32,
	selectingGeneration uint64, bounds GlobalTabletCatalogLabBounds,
) error {
	if ref.Offset == 0 || ref.Offset&4095 != 0 ||
		ref.Offset>>3 >= uint64(1)<<48 ||
		ref.LogicalID != logicalID || ref.Generation == 0 ||
		ref.Generation >= uint64(1)<<48 ||
		ref.Generation > selectingGeneration || ref.Length != length ||
		ref.Kind != kind || ref.Flags != 0 || ref.Aux != 0 ||
		!bounds.contains(ref) {
		return fmt.Errorf("%w: packed catalog reference", ErrInvalidWrite)
	}
	return nil
}

func globalTabletCatalogLabValidateFullRef(
	ref PageRef, logicalID uint64, kind PageKind, length int,
	selectingGeneration uint64, bounds GlobalTabletCatalogLabBounds,
) error {
	if ref.Offset == 0 || ref.Offset&4095 != 0 ||
		ref.LogicalID != logicalID || ref.Generation == 0 ||
		ref.Generation >= uint64(1)<<48 ||
		ref.Generation > selectingGeneration ||
		ref.Length != uint32(length) || ref.Kind != kind ||
		ref.Flags != 0 || ref.Aux != 0 || !bounds.contains(ref) {
		return fmt.Errorf("%w: full tablet reference", ErrInvalidWrite)
	}
	return nil
}

func globalTabletCatalogLabEncodePackedRef(dst []byte, ref PageRef) {
	segmentedTabletRouterLabPutUint48(dst, ref.Offset>>3)
	segmentedTabletRouterLabPutUint48(dst[6:], ref.Generation)
}

func globalTabletCatalogLabHeaderMatchesRef(
	header PageHeader, ref PageRef, bounds GlobalTabletCatalogLabBounds,
) bool {
	return ref.Offset != 0 && ref.Offset&4095 == 0 &&
		header.StoreID == bounds.StoreID &&
		header.LogicalID == ref.LogicalID &&
		header.Generation == ref.Generation &&
		header.PageSize == ref.Length && header.Kind == ref.Kind &&
		ref.Flags == 0 && ref.Aux == 0 && bounds.contains(ref)
}

func (b GlobalTabletCatalogLabBounds) valid() bool {
	return b.StoreID != ([16]byte{}) &&
		b.SelectedRootGeneration != 0 &&
		b.SelectedRootGeneration < uint64(1)<<48 &&
		b.FileEnd >= GlobalTabletCatalogLabRootBytes &&
		b.NextLogicalID >= GlobalTabletCatalogLabFirstDynamicLogicalID
}

func (b GlobalTabletCatalogLabBounds) contains(ref PageRef) bool {
	length := uint64(ref.Length)
	return b.valid() && ref.LogicalID != 0 &&
		ref.LogicalID < b.NextLogicalID &&
		ref.Generation != 0 &&
		ref.Generation <= b.SelectedRootGeneration &&
		length != 0 && length <= b.FileEnd &&
		ref.Offset <= b.FileEnd-length
}

func (b GlobalTabletCatalogLabBounds) extends(previous GlobalTabletCatalogLabBounds) bool {
	return b.valid() && previous.valid() &&
		b.StoreID == previous.StoreID &&
		b.SelectedRootGeneration >= previous.SelectedRootGeneration &&
		b.FileEnd >= previous.FileEnd &&
		b.NextLogicalID >= previous.NextLogicalID
}

func globalTabletCatalogLabPut14(dst []byte, localID uint16, code uint16) {
	bit := int(localID) * globalTabletCatalogLabPackedBits
	at, shift := bit>>3, uint(bit&7)
	word := uint32(code&0x3fff) << shift
	dst[at] |= byte(word)
	dst[at+1] |= byte(word >> 8)
	if at+2 < len(dst) {
		dst[at+2] |= byte(word >> 16)
	}
}

func globalTabletCatalogLabGet14(src []byte, localID uint16) uint16 {
	bit := int(localID) * globalTabletCatalogLabPackedBits
	at, shift := bit>>3, uint(bit&7)
	word := uint32(src[at]) | uint32(src[at+1])<<8
	if at+2 < len(src) {
		word |= uint32(src[at+2]) << 16
	}
	return uint16(word>>shift) & 0x3fff
}

func (v *globalTabletCatalogLabSegmentedRootView) rootUpperBound(key []byte) int {
	low, high := 1, int(v.pageCount)
	for low < high {
		mid := int(uint(low+high) >> 1)
		fence := v.rootFence(mid).a
		if bytes.Compare(fence, key) <= 0 {
			low = mid + 1
		} else {
			high = mid
		}
	}
	return low
}

func (v *globalTabletCatalogLabSegmentedRootView) rootFence(
	rank int,
) segmentedTabletRouterLabFence {
	start := int(binary.LittleEndian.Uint16(v.rootOffsets[rank*2:]))
	end := int(binary.LittleEndian.Uint16(v.rootOffsets[(rank+1)*2:]))
	return segmentedTabletRouterLabFence{a: v.rootKeys[start:end]}
}

func (v *globalTabletCatalogLabSegmentedRootView) anchorRef(
	pageID uint8,
) (PageRef, bool) {
	if pageID >= SegmentedTabletRouterLabMaxPages {
		return PageRef{}, false
	}
	start := int(pageID) * segmentedTabletRouterLabRootRefBytes
	src := v.rootRefs[start : start+segmentedTabletRouterLabRootRefBytes]
	if allZero(src) {
		return PageRef{}, false
	}
	logicalID, ok := GlobalTabletCatalogLabAnchorLogicalID(v.tabletID, pageID)
	if !ok {
		return PageRef{}, false
	}
	ref := PageRef{
		Offset:     segmentedTabletRouterLabGetUint48(src) << 12,
		LogicalID:  logicalID,
		Generation: segmentedTabletRouterLabGetUint48(src[6:]),
		Length:     uint32(4096) << src[12],
		Kind:       v.anchorKind,
	}
	return ref, segmentedTabletRouterLabValidateAnchorRef(
		ref,
		SegmentedTabletRouterLabHeader{
			StoreID: v.storeID, TabletID: v.tabletID,
			Generation: v.generation,
			AnchorKind: v.anchorKind, LeafKind: v.leafKind,
		},
		pageID,
	) == nil
}

func (v globalTabletCatalogLabSegmentedRootView) segmentedView() SegmentedTabletRouterLabView {
	return SegmentedTabletRouterLabView{
		root: v.root, rootRefs: v.rootRefs, rootRanks: v.rootRanks,
		rootOffsets: v.rootOffsets, rootKeys: v.rootKeys,
		storeID: v.storeID, tabletID: v.tabletID, generation: v.generation,
		pageCount: v.pageCount, anchorKind: v.anchorKind, leafKind: v.leafKind,
	}
}

func globalTabletCatalogLabRootRefsWithinBounds(
	root globalTabletCatalogLabSegmentedRootView,
	bounds GlobalTabletCatalogLabBounds,
) error {
	for rank := 0; rank < int(root.pageCount); rank++ {
		pageID := root.rootRanks[rank]
		ref, ok := root.anchorRef(pageID)
		if !ok || !bounds.contains(ref) {
			return fmt.Errorf("%w: tablet anchor bounds", ErrInvalidWrite)
		}
	}
	return nil
}

func globalTabletCatalogLabOpenSegmentedRootOnly(
	root []byte,
) (globalTabletCatalogLabSegmentedRootView, error) {
	var view globalTabletCatalogLabSegmentedRootView
	if len(root) != SegmentedTabletRouterLabRootBytes ||
		string(root[:8]) != segmentedTabletRouterLabRootMagic ||
		binary.LittleEndian.Uint32(root[8:12]) != segmentedTabletRouterLabVersion ||
		binary.LittleEndian.Uint16(root[12:14]) != segmentedTabletRouterLabRootHeaderBytes ||
		root[14] == 0 || root[14] > SegmentedTabletRouterLabMaxPages ||
		PageKind(root[15]) != PagePrimaryAnchor ||
		PageKind(root[16]) != PagePrimaryLeaf ||
		!allZero(root[17:20]) || allZero(root[44:60]) ||
		!allZero(root[60:segmentedTabletRouterLabRootHeaderBytes]) ||
		!segmentedTabletRouterLabChecksumOK(root, segmentedTabletRouterLabRootTrailerAt) {
		return view, globalTabletCatalogLabCorrupt("segmented root envelope")
	}
	pageCount := int(root[14])
	tabletID := binary.LittleEndian.Uint32(root[20:24])
	generation := binary.LittleEndian.Uint64(root[24:32])
	keyBytes := int(binary.LittleEndian.Uint16(root[32:34]))
	if tabletID >= TabletLocalIdentityLabTabletCount ||
		generation == 0 || generation >= uint64(1)<<48 ||
		int(binary.LittleEndian.Uint16(root[34:36])) != pageCount ||
		binary.LittleEndian.Uint32(root[40:44]) != SegmentedTabletRouterLabRootBytes ||
		keyBytes > segmentedTabletRouterLabRootTrailerAt-segmentedTabletRouterLabRootKeysAt {
		return globalTabletCatalogLabSegmentedRootView{},
			globalTabletCatalogLabCorrupt("segmented root identity")
	}
	view = globalTabletCatalogLabSegmentedRootView{
		root:        root,
		rootRefs:    root[segmentedTabletRouterLabRootRefsAt:segmentedTabletRouterLabRootRanksAt],
		rootRanks:   root[segmentedTabletRouterLabRootRanksAt:segmentedTabletRouterLabRootOffsetsAt],
		rootOffsets: root[segmentedTabletRouterLabRootOffsetsAt:segmentedTabletRouterLabRootKeysAt],
		rootKeys:    root[segmentedTabletRouterLabRootKeysAt : segmentedTabletRouterLabRootKeysAt+keyBytes],
		storeID:     [16]byte(root[44:60]),
		tabletID:    tabletID, generation: generation, pageCount: uint8(pageCount),
		anchorKind: PageKind(root[15]), leafKind: PageKind(root[16]),
	}
	if binary.LittleEndian.Uint16(view.rootOffsets[pageCount*2:]) != uint16(keyBytes) ||
		!allZero(root[segmentedTabletRouterLabRootRanksAt+pageCount:segmentedTabletRouterLabRootOffsetsAt]) ||
		!allZero(root[segmentedTabletRouterLabRootOffsetsAt+(pageCount+1)*2:segmentedTabletRouterLabRootKeysAt]) ||
		!allZero(root[segmentedTabletRouterLabRootKeysAt+keyBytes:segmentedTabletRouterLabRootTrailerAt]) {
		return globalTabletCatalogLabSegmentedRootView{},
			globalTabletCatalogLabCorrupt("segmented root sections")
	}
	var seen uint16
	var previous segmentedTabletRouterLabFence
	for rank := 0; rank < pageCount; rank++ {
		start := int(binary.LittleEndian.Uint16(view.rootOffsets[rank*2:]))
		end := int(binary.LittleEndian.Uint16(view.rootOffsets[(rank+1)*2:]))
		pageID := view.rootRanks[rank]
		if start > end || end > keyBytes ||
			pageID >= SegmentedTabletRouterLabMaxPages ||
			seen&(uint16(1)<<pageID) != 0 {
			return globalTabletCatalogLabSegmentedRootView{},
				globalTabletCatalogLabCorrupt("segmented root order")
		}
		seen |= uint16(1) << pageID
		fence := view.rootFence(rank)
		if rank == 0 && fence.length() != 0 ||
			rank != 0 && segmentedTabletRouterLabCompareFences(previous, fence) >= 0 {
			return globalTabletCatalogLabSegmentedRootView{},
				globalTabletCatalogLabCorrupt("segmented root floors")
		}
		if _, ok := view.anchorRef(pageID); !ok {
			return globalTabletCatalogLabSegmentedRootView{},
				globalTabletCatalogLabCorrupt("segmented anchor reference")
		}
		previous = fence
	}
	for pageID := 0; pageID < SegmentedTabletRouterLabMaxPages; pageID++ {
		start := pageID * segmentedTabletRouterLabRootRefBytes
		if seen&(uint16(1)<<pageID) == 0 &&
			!allZero(view.rootRefs[start:start+segmentedTabletRouterLabRootRefBytes]) {
			return globalTabletCatalogLabSegmentedRootView{},
				globalTabletCatalogLabCorrupt("segmented unused reference")
		}
	}
	return view, nil
}

func globalTabletCatalogLabCorrupt(detail string) error {
	return fmt.Errorf("%w: %s", ErrGlobalTabletCatalogLabCorrupt, detail)
}

func globalTabletCatalogLabSlicesOverlap(left, right []byte) bool {
	if len(left) == 0 || len(right) == 0 {
		return false
	}
	leftStart := uintptr(unsafe.Pointer(unsafe.SliceData(left)))
	rightStart := uintptr(unsafe.Pointer(unsafe.SliceData(right)))
	return leftStart < rightStart+uintptr(len(right)) &&
		rightStart < leftStart+uintptr(len(left))
}
