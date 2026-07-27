package storeio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// The segmented tablet router is the production-candidate form of the 18/12
// primary routing experiment. One tablet has a 4 KiB root, an exact 8 KiB
// LocalID locator, and at most sixteen independently replaceable 8 KiB anchor
// pages. A locator value is (stable page ID << 8) | stable row slot. Lexical
// ranks are a separate permutation inside each anchor page.
//
// The leaf handle is eighteen bytes:
//
//	u48 absolute offset in 8-byte units
//	u48 generation
//	u16 exact length minus one
//	4-byte zone
//
// Exact length is needed for byte-packed cold leaves. LogicalID is derived from
// BucketID and Kind is fixed by the tablet root, so neither is duplicated.
const (
	SegmentedTabletRouterLabRootBytes       = 4 << 10
	SegmentedTabletRouterLabLocatorBytes    = 8 << 10
	SegmentedTabletRouterLabAnchorPageBytes = 8 << 10
	SegmentedTabletRouterLabMaxPages        = 16
	SegmentedTabletRouterLabRowsPerPage     = 256
	SegmentedTabletRouterLabHandleBytes     = 18

	// Logical IDs form one explicit, disjoint durable namespace. StateRoot owns
	// 1. Every possible 30-bit leaf identity follows, then every possible
	// (18-bit tablet, 4-bit stable anchor-page) identity. Tablet roots,
	// catalogs, indexes, and other dynamically allocated pages start at the
	// first free boundary below.
	SegmentedTabletRouterLabStateRootLogicalID    = uint64(1)
	SegmentedTabletRouterLabLeafLogicalIDBase     = uint64(2)
	SegmentedTabletRouterLabLeafLogicalIDLimit    = SegmentedTabletRouterLabLeafLogicalIDBase + 1<<30
	SegmentedTabletRouterLabAnchorLogicalIDBase   = SegmentedTabletRouterLabLeafLogicalIDLimit
	SegmentedTabletRouterLabAnchorLogicalIDLimit  = SegmentedTabletRouterLabAnchorLogicalIDBase + 1<<18*SegmentedTabletRouterLabMaxPages
	SegmentedTabletRouterLabFirstDynamicLogicalID = SegmentedTabletRouterLabAnchorLogicalIDLimit

	segmentedTabletRouterLabRootHeaderBytes   = 64
	segmentedTabletRouterLabRootRefBytes      = 13
	segmentedTabletRouterLabRootRefsAt        = segmentedTabletRouterLabRootHeaderBytes
	segmentedTabletRouterLabRootRanksAt       = segmentedTabletRouterLabRootRefsAt + SegmentedTabletRouterLabMaxPages*segmentedTabletRouterLabRootRefBytes
	segmentedTabletRouterLabRootOffsetsAt     = segmentedTabletRouterLabRootRanksAt + SegmentedTabletRouterLabMaxPages
	segmentedTabletRouterLabRootKeysAt        = segmentedTabletRouterLabRootOffsetsAt + (SegmentedTabletRouterLabMaxPages+1)*2
	segmentedTabletRouterLabRootTrailerAt     = SegmentedTabletRouterLabRootBytes - 8
	segmentedTabletRouterLabAnchorHeaderBytes = 64
	segmentedTabletRouterLabAnchorRanksAt     = segmentedTabletRouterLabAnchorHeaderBytes
	segmentedTabletRouterLabAnchorLocalIDsAt  = segmentedTabletRouterLabAnchorRanksAt + SegmentedTabletRouterLabRowsPerPage
	segmentedTabletRouterLabAnchorHandlesAt   = segmentedTabletRouterLabAnchorLocalIDsAt + SegmentedTabletRouterLabRowsPerPage*2
	segmentedTabletRouterLabAnchorOffsetsAt   = segmentedTabletRouterLabAnchorHandlesAt + SegmentedTabletRouterLabRowsPerPage*SegmentedTabletRouterLabHandleBytes
	segmentedTabletRouterLabAnchorKeysAt      = segmentedTabletRouterLabAnchorOffsetsAt + (SegmentedTabletRouterLabRowsPerPage+1)*2
	segmentedTabletRouterLabAnchorTrailerAt   = SegmentedTabletRouterLabAnchorPageBytes - 8
	segmentedTabletRouterLabAnchorKeyCapacity = segmentedTabletRouterLabAnchorTrailerAt - segmentedTabletRouterLabAnchorKeysAt

	segmentedTabletRouterLabRestart = 16
	segmentedTabletRouterLabEmpty   = uint16(0xffff)
	// Version 2 changes the derived PageRef LogicalID namespace. Version 1
	// mapped bucket zero and low anchor identities onto StateRoot/each other,
	// so it must fail closed even though LogicalID is not repeated in handles.
	segmentedTabletRouterLabVersion   = uint32(2)
	segmentedTabletRouterLabRootMagic = "STRROOT1"
	segmentedTabletRouterLabPageMagic = "STRPAGE1"
)

var (
	ErrSegmentedTabletRouterLabCorrupt = errors.New(
		"vibejson: corrupt segmented tablet router lab image",
	)
	ErrSegmentedTabletRouterLabNoSpace = errors.New(
		"vibejson: segmented tablet router lab image has no space",
	)
	ErrSegmentedTabletRouterLabNotFound = errors.New(
		"vibejson: segmented tablet router lab bucket not found",
	)
)

// SegmentedTabletRouterLabHeader is the root identity. AnchorKind identifies
// the fixed 8 KiB routing pages; LeafKind identifies the exact-length handles.
type SegmentedTabletRouterLabHeader struct {
	TabletID   uint32
	Generation uint64
	AnchorKind PageKind
	LeafKind   PageKind
}

// SegmentedTabletRouterLabLeaf is canonical encoder input. Leaves must be in
// strict lexical fence order. The first fence must be empty and denotes
// negative infinity. LocalID is stable and unique inside the tablet.
type SegmentedTabletRouterLabLeaf struct {
	LocalID uint16
	Fence   []byte
	Ref     PageRef
	Zone    BucketZone
}

// SegmentedTabletRouterLabRoute is a complete resident route. Hash is carried
// through to the ordered hash leaf, avoiding a second primary-key hash.
type SegmentedTabletRouterLabRoute struct {
	Bucket  BucketID
	Hash    uint64
	PageID  uint8
	RowSlot uint8
	Ref     PageRef
	Zone    BucketZone
}

type segmentedTabletRouterLabAnchorView struct {
	image      []byte
	ranks      []byte
	localIDs   []byte
	handles    []byte
	offsets    []byte
	keys       []byte
	heads      []byte
	headValid  []byte
	tabletID   uint32
	generation uint64
	pageID     uint8
	count      uint16
	common     uint8
	headBytes  uint8
	leafKind   PageKind
}

// SegmentedTabletRouterLabView borrows all checksum-admitted images. The fixed
// array avoids heap allocation and makes posting-driven page selection direct.
type SegmentedTabletRouterLabView struct {
	root        []byte
	locator     []byte
	pages       [SegmentedTabletRouterLabMaxPages]segmentedTabletRouterLabAnchorView
	rootRefs    []byte
	rootRanks   []byte
	rootOffsets []byte
	rootKeys    []byte
	tabletID    uint32
	generation  uint64
	pageCount   uint8
	anchorKind  PageKind
	leafKind    PageKind
}

// SegmentedTabletRouterLabCursor walks current leaves in lexical order without
// following sibling pointers. Returned key-independent routes are exact.
type SegmentedTabletRouterLabCursor struct {
	view     *SegmentedTabletRouterLabView
	pageRank uint8
	rowRank  uint16
	valid    bool
}

// SegmentedTabletRouterLabCOWResult reports the exact immutable rewrite set.
type SegmentedTabletRouterLabCOWResult struct {
	Root       []byte
	AnchorPage []byte
	PageID     uint8
	Bytes      int
}

// SegmentedTabletRouterLabSplitResult reports one structural split. Locator,
// root, and both output anchor pages form one atomic publication unit.
type SegmentedTabletRouterLabSplitResult struct {
	Root        []byte
	Locator     []byte
	LeftPage    []byte
	RightPage   []byte
	LeftPageID  uint8
	RightPageID uint8
	Bytes       int
}

type segmentedTabletRouterLabFence struct {
	a, b, c []byte
}

func (f segmentedTabletRouterLabFence) length() int {
	return len(f.a) + len(f.b) + len(f.c)
}

func (f segmentedTabletRouterLabFence) at(index int) byte {
	if index < len(f.a) {
		return f.a[index]
	}
	index -= len(f.a)
	if index < len(f.b) {
		return f.b[index]
	}
	return f.c[index-len(f.b)]
}

func (f segmentedTabletRouterLabFence) copyTo(dst []byte, from int) int {
	n := 0
	for at := from; at < f.length(); at++ {
		dst[n] = f.at(at)
		n++
	}
	return n
}

func segmentedTabletRouterLabFencePrefix(
	left, right segmentedTabletRouterLabFence,
) int {
	limit := min(left.length(), right.length())
	for at := 0; at < limit; at++ {
		if left.at(at) != right.at(at) {
			return at
		}
	}
	return limit
}

func segmentedTabletRouterLabCompareFenceKey(
	fence segmentedTabletRouterLabFence, key []byte,
) int {
	keyAt := 0
	for _, part := range [...][]byte{fence.a, fence.b, fence.c} {
		remaining := len(key) - keyAt
		if remaining <= 0 {
			if len(part) != 0 {
				return 1
			}
			continue
		}
		compared := min(len(part), remaining)
		if order := bytes.Compare(part[:compared], key[keyAt:keyAt+compared]); order != 0 {
			return order
		}
		keyAt += compared
		if compared != len(part) {
			return 1
		}
	}
	if keyAt < len(key) {
		return -1
	}
	return 0
}

func segmentedTabletRouterLabCompareFenceSuffixKey(
	restartPrefix, suffix, key []byte,
) int {
	compared := min(len(restartPrefix), len(key))
	if order := bytes.Compare(restartPrefix[:compared], key[:compared]); order != 0 {
		return order
	}
	if compared != len(restartPrefix) {
		return 1
	}
	key = key[compared:]
	compared = min(len(suffix), len(key))
	if order := bytes.Compare(suffix[:compared], key[:compared]); order != 0 {
		return order
	}
	if compared != len(suffix) {
		return 1
	}
	if compared < len(key) {
		return -1
	}
	return 0
}

func segmentedTabletRouterLabCompareFences(
	left, right segmentedTabletRouterLabFence,
) int {
	limit := min(left.length(), right.length())
	for at := 0; at < limit; at++ {
		if left.at(at) < right.at(at) {
			return -1
		}
		if left.at(at) > right.at(at) {
			return 1
		}
	}
	return left.length() - right.length()
}

// EncodeSegmentedTabletRouterLab builds the complete initial tablet. pageDst
// must contain one 8 KiB slice per required page, contiguously. anchorRefs are
// in lexical page order and become stable page IDs 0..pageCount-1.
func EncodeSegmentedTabletRouterLab(
	rootDst, locatorDst, pageDst []byte,
	header SegmentedTabletRouterLabHeader,
	anchorRefs []PageRef,
	leaves []SegmentedTabletRouterLabLeaf,
) (root, locator, pages []byte, pageCount int, err error) {
	if len(rootDst) < SegmentedTabletRouterLabRootBytes ||
		len(locatorDst) < SegmentedTabletRouterLabLocatorBytes ||
		header.TabletID >= TabletLocalIdentityLabTabletCount ||
		header.Generation == 0 || header.Generation >= uint64(1)<<48 ||
		!validPageKind(header.AnchorKind) ||
		!validPageKind(header.LeafKind) ||
		len(leaves) == 0 ||
		len(leaves) > TabletLocalIdentityLabLocalCount ||
		len(leaves) > SegmentedTabletRouterLabMaxPages*SegmentedTabletRouterLabRowsPerPage {
		return nil, nil, nil, 0, fmt.Errorf(
			"%w: segmented router identity or geometry", ErrInvalidWrite,
		)
	}
	pageCount = (len(leaves) + SegmentedTabletRouterLabRowsPerPage - 1) /
		SegmentedTabletRouterLabRowsPerPage
	if len(anchorRefs) != pageCount ||
		len(pageDst) < pageCount*SegmentedTabletRouterLabAnchorPageBytes {
		return nil, nil, nil, 0, fmt.Errorf(
			"%w: segmented router page destinations", ErrInvalidWrite,
		)
	}
	if len(leaves[0].Fence) != 0 {
		return nil, nil, nil, 0, fmt.Errorf(
			"%w: first segmented router fence is not empty", ErrInvalidWrite,
		)
	}
	locator = locatorDst[:SegmentedTabletRouterLabLocatorBytes]
	for at := range locator {
		locator[at] = 0xff
	}
	var previous []byte
	for rank, leaf := range leaves {
		if leaf.LocalID >= TabletLocalIdentityLabLocalCount ||
			rank != 0 && bytes.Compare(previous, leaf.Fence) >= 0 ||
			rank != 0 && len(leaf.Fence) == 0 {
			return nil, nil, nil, 0, fmt.Errorf(
				"%w: non-canonical leaf at rank %d", ErrInvalidWrite, rank,
			)
		}
		bucket, ok := MakeTabletLocalIdentityLabBucket(
			header.TabletID, uint32(leaf.LocalID),
		)
		refErr := segmentedTabletRouterLabValidateLeafRef(
			leaf.Ref, BucketID(bucket), header.LeafKind,
			header.Generation,
		)
		if !ok || refErr != nil {
			return nil, nil, nil, 0, fmt.Errorf(
				"%w: invalid leaf identity at rank %d: %v",
				ErrInvalidWrite, rank, refErr,
			)
		}
		if binary.LittleEndian.Uint16(locator[int(leaf.LocalID)*2:]) !=
			segmentedTabletRouterLabEmpty {
			return nil, nil, nil, 0, fmt.Errorf(
				"%w: duplicate LocalID", ErrInvalidWrite,
			)
		}
		pageID := rank / SegmentedTabletRouterLabRowsPerPage
		rowSlot := rank % SegmentedTabletRouterLabRowsPerPage
		binary.LittleEndian.PutUint16(
			locator[int(leaf.LocalID)*2:],
			uint16(pageID<<8|rowSlot),
		)
		previous = leaf.Fence
	}
	pages = pageDst[:pageCount*SegmentedTabletRouterLabAnchorPageBytes]
	for pageID := 0; pageID < pageCount; pageID++ {
		if anchorRefs[pageID].Generation != header.Generation {
			return nil, nil, nil, 0, fmt.Errorf(
				"%w: initial anchor generation", ErrInvalidWrite,
			)
		}
		if err := segmentedTabletRouterLabValidateAnchorRef(
			anchorRefs[pageID], header, uint8(pageID),
		); err != nil {
			return nil, nil, nil, 0, err
		}
		first := pageID * SegmentedTabletRouterLabRowsPerPage
		last := min(first+SegmentedTabletRouterLabRowsPerPage, len(leaves))
		if _, err := segmentedTabletRouterLabEncodeAnchorFromLeaves(
			pages[pageID*SegmentedTabletRouterLabAnchorPageBytes:],
			header, uint8(pageID), leaves[first:last],
		); err != nil {
			return nil, nil, nil, 0, err
		}
	}
	root = rootDst[:SegmentedTabletRouterLabRootBytes]
	if err := segmentedTabletRouterLabEncodeRootInitial(
		root, locator, header, anchorRefs, leaves,
	); err != nil {
		return nil, nil, nil, 0, err
	}
	return root, locator, pages, pageCount, nil
}

// OpenSegmentedTabletRouterLab admits one complete resident tablet. anchorPages
// is the concatenation of every live stable page ID from zero to pageCount-1.
// Unused stable IDs are represented by absent root refs, not dummy images.
func OpenSegmentedTabletRouterLab(
	root, locator, anchorPages []byte,
) (SegmentedTabletRouterLabView, error) {
	var view SegmentedTabletRouterLabView
	if len(root) != SegmentedTabletRouterLabRootBytes ||
		len(locator) != SegmentedTabletRouterLabLocatorBytes ||
		string(root[:8]) != segmentedTabletRouterLabRootMagic ||
		binary.LittleEndian.Uint32(root[8:12]) !=
			segmentedTabletRouterLabVersion ||
		binary.LittleEndian.Uint16(root[12:14]) !=
			segmentedTabletRouterLabRootHeaderBytes ||
		root[14] == 0 || root[14] > SegmentedTabletRouterLabMaxPages ||
		!validPageKind(PageKind(root[15])) ||
		!validPageKind(PageKind(root[16])) ||
		!allZero(root[17:20]) ||
		!allZero(root[44:segmentedTabletRouterLabRootHeaderBytes]) {
		return view, segmentedTabletRouterLabCorrupt("root header")
	}
	pageCount := int(root[14])
	tabletID := binary.LittleEndian.Uint32(root[20:24])
	generation := binary.LittleEndian.Uint64(root[24:32])
	rootKeyBytes := int(binary.LittleEndian.Uint16(root[32:34]))
	if tabletID >= TabletLocalIdentityLabTabletCount || generation == 0 ||
		generation >= uint64(1)<<48 ||
		int(binary.LittleEndian.Uint16(root[34:36])) != pageCount ||
		binary.LittleEndian.Uint32(root[36:40]) != PageChecksum(locator) ||
		int(binary.LittleEndian.Uint32(root[40:44])) !=
			SegmentedTabletRouterLabRootBytes ||
		rootKeyBytes > segmentedTabletRouterLabRootTrailerAt-
			segmentedTabletRouterLabRootKeysAt ||
		len(anchorPages) != pageCount*SegmentedTabletRouterLabAnchorPageBytes ||
		!segmentedTabletRouterLabChecksumOK(
			root, segmentedTabletRouterLabRootTrailerAt,
		) {
		return view, segmentedTabletRouterLabCorrupt(
			"root identity, binding, geometry, or checksum",
		)
	}
	view = SegmentedTabletRouterLabView{
		root:        root,
		locator:     locator,
		rootRefs:    root[segmentedTabletRouterLabRootRefsAt:segmentedTabletRouterLabRootRanksAt],
		rootRanks:   root[segmentedTabletRouterLabRootRanksAt:segmentedTabletRouterLabRootOffsetsAt],
		rootOffsets: root[segmentedTabletRouterLabRootOffsetsAt:segmentedTabletRouterLabRootKeysAt],
		rootKeys:    root[segmentedTabletRouterLabRootKeysAt : segmentedTabletRouterLabRootKeysAt+rootKeyBytes],
		tabletID:    tabletID,
		generation:  generation,
		pageCount:   uint8(pageCount),
		anchorKind:  PageKind(root[15]),
		leafKind:    PageKind(root[16]),
	}
	if binary.LittleEndian.Uint16(view.rootOffsets[pageCount*2:]) !=
		uint16(rootKeyBytes) {
		return SegmentedTabletRouterLabView{},
			segmentedTabletRouterLabCorrupt("root terminal offset")
	}
	for rank := 0; rank < pageCount; rank++ {
		start := int(binary.LittleEndian.Uint16(view.rootOffsets[rank*2:]))
		end := int(binary.LittleEndian.Uint16(view.rootOffsets[(rank+1)*2:]))
		if start > end || end > rootKeyBytes {
			return SegmentedTabletRouterLabView{},
				segmentedTabletRouterLabCorrupt("root fence offsets")
		}
	}
	if !allZero(
		root[segmentedTabletRouterLabRootRanksAt+pageCount:segmentedTabletRouterLabRootOffsetsAt],
	) || !allZero(
		root[segmentedTabletRouterLabRootOffsetsAt+(pageCount+1)*2:segmentedTabletRouterLabRootKeysAt],
	) || !allZero(
		root[segmentedTabletRouterLabRootKeysAt+rootKeyBytes:segmentedTabletRouterLabRootTrailerAt],
	) {
		return SegmentedTabletRouterLabView{},
			segmentedTabletRouterLabCorrupt("root non-canonical padding")
	}
	var seenPages uint16
	for rank := 0; rank < pageCount; rank++ {
		pageID := view.rootRanks[rank]
		if int(pageID) >= pageCount ||
			seenPages&(uint16(1)<<pageID) != 0 {
			return SegmentedTabletRouterLabView{},
				segmentedTabletRouterLabCorrupt("root page permutation")
		}
		seenPages |= uint16(1) << pageID
		ref, ok := view.anchorRef(pageID)
		if !ok {
			return SegmentedTabletRouterLabView{},
				segmentedTabletRouterLabCorrupt("root anchor ref")
		}
		image := anchorPages[int(pageID)*SegmentedTabletRouterLabAnchorPageBytes : (int(pageID)+1)*SegmentedTabletRouterLabAnchorPageBytes]
		page, err := segmentedTabletRouterLabOpenAnchor(
			image, view, pageID, ref,
		)
		if err != nil {
			return SegmentedTabletRouterLabView{}, err
		}
		view.pages[pageID] = page
		if rank == 0 {
			fence := page.fenceAt(0)
			if fence.length() != 0 || view.rootFence(rank).length() != 0 {
				return SegmentedTabletRouterLabView{},
					segmentedTabletRouterLabCorrupt("non-empty first floor")
			}
		} else if segmentedTabletRouterLabCompareFences(
			view.rootFence(rank), page.fenceAt(0),
		) != 0 || segmentedTabletRouterLabCompareFences(
			view.rootFence(rank-1), view.rootFence(rank),
		) >= 0 {
			return SegmentedTabletRouterLabView{},
				segmentedTabletRouterLabCorrupt("root lexical floors")
		}
	}
	for pageID := 0; pageID < SegmentedTabletRouterLabMaxPages; pageID++ {
		start := pageID * segmentedTabletRouterLabRootRefBytes
		if seenPages&(uint16(1)<<pageID) == 0 &&
			!allZero(view.rootRefs[start:start+
				segmentedTabletRouterLabRootRefBytes]) {
			return SegmentedTabletRouterLabView{},
				segmentedTabletRouterLabCorrupt("unranked anchor ref")
		}
	}
	if err := view.validateLocator(); err != nil {
		return SegmentedTabletRouterLabView{}, err
	}
	return view, nil
}

func (v *SegmentedTabletRouterLabView) validateLocator() error {
	live := 0
	for localID := 0; localID < TabletLocalIdentityLabLocalCount; localID++ {
		code := binary.LittleEndian.Uint16(v.locator[localID*2:])
		if code == segmentedTabletRouterLabEmpty {
			continue
		}
		pageID, slot := uint8(code>>8), uint8(code)
		if pageID >= SegmentedTabletRouterLabMaxPages ||
			len(v.pages[pageID].image) == 0 ||
			binary.LittleEndian.Uint16(
				v.pages[pageID].localIDs[int(slot)*2:],
			) != uint16(localID) {
			return segmentedTabletRouterLabCorrupt("locator binding")
		}
		live++
	}
	rows := 0
	for pageID := 0; pageID < SegmentedTabletRouterLabMaxPages; pageID++ {
		page := &v.pages[pageID]
		if len(page.image) == 0 {
			continue
		}
		rows += int(page.count)
		for rank := 0; rank < int(page.count); rank++ {
			slot := page.ranks[rank]
			localID := binary.LittleEndian.Uint16(page.localIDs[int(slot)*2:])
			if localID >= TabletLocalIdentityLabLocalCount ||
				binary.LittleEndian.Uint16(v.locator[int(localID)*2:]) !=
					uint16(pageID<<8|int(slot)) {
				return segmentedTabletRouterLabCorrupt("row locator inverse")
			}
		}
	}
	if rows != live {
		return segmentedTabletRouterLabCorrupt("locator cardinality")
	}
	return nil
}

func segmentedTabletRouterLabOpenAnchor(
	image []byte,
	root SegmentedTabletRouterLabView,
	pageID uint8,
	ref PageRef,
) (segmentedTabletRouterLabAnchorView, error) {
	var view segmentedTabletRouterLabAnchorView
	if len(image) != SegmentedTabletRouterLabAnchorPageBytes ||
		string(image[:8]) != segmentedTabletRouterLabPageMagic ||
		binary.LittleEndian.Uint32(image[8:12]) !=
			segmentedTabletRouterLabVersion ||
		binary.LittleEndian.Uint16(image[12:14]) !=
			segmentedTabletRouterLabAnchorHeaderBytes ||
		image[14] != pageID || image[15] != segmentedTabletRouterLabRestart ||
		binary.LittleEndian.Uint32(image[16:20]) != root.tabletID ||
		binary.LittleEndian.Uint64(image[24:32]) != ref.Generation ||
		image[40] != byte(root.leafKind) ||
		!allZero(image[41:segmentedTabletRouterLabAnchorHeaderBytes]) ||
		!segmentedTabletRouterLabChecksumOK(
			image, segmentedTabletRouterLabAnchorTrailerAt,
		) {
		return view, segmentedTabletRouterLabCorrupt(
			"anchor header, identity, or checksum",
		)
	}
	count := int(binary.LittleEndian.Uint16(image[20:22]))
	keyBytes := int(binary.LittleEndian.Uint16(image[22:24]))
	common := int(image[32])
	headBytes := int(image[33])
	if count == 0 || count > SegmentedTabletRouterLabRowsPerPage ||
		keyBytes < common || keyBytes > segmentedTabletRouterLabAnchorKeyCapacity ||
		headBytes != 0 && headBytes != 1 &&
			headBytes != 2 && headBytes != 4 ||
		int(binary.LittleEndian.Uint16(image[34:36])) != count ||
		int(binary.LittleEndian.Uint32(image[36:40])) !=
			SegmentedTabletRouterLabAnchorPageBytes {
		return view, segmentedTabletRouterLabCorrupt("anchor geometry")
	}
	view = segmentedTabletRouterLabAnchorView{
		image:      image,
		ranks:      image[segmentedTabletRouterLabAnchorRanksAt:segmentedTabletRouterLabAnchorLocalIDsAt],
		localIDs:   image[segmentedTabletRouterLabAnchorLocalIDsAt:segmentedTabletRouterLabAnchorHandlesAt],
		handles:    image[segmentedTabletRouterLabAnchorHandlesAt:segmentedTabletRouterLabAnchorOffsetsAt],
		offsets:    image[segmentedTabletRouterLabAnchorOffsetsAt:segmentedTabletRouterLabAnchorKeysAt],
		keys:       image[segmentedTabletRouterLabAnchorKeysAt : segmentedTabletRouterLabAnchorKeysAt+keyBytes],
		tabletID:   root.tabletID,
		generation: ref.Generation,
		pageID:     pageID,
		count:      uint16(count),
		common:     uint8(common),
		headBytes:  uint8(headBytes),
		leafKind:   root.leafKind,
	}
	dataBytes := int(binary.LittleEndian.Uint16(view.offsets[count*2:]))
	headExtra := 0
	if headBytes != 0 {
		headExtra = count*headBytes + SegmentedTabletRouterLabRowsPerPage/8
	}
	if dataBytes < common || dataBytes+headExtra != keyBytes {
		return segmentedTabletRouterLabAnchorView{},
			segmentedTabletRouterLabCorrupt("anchor terminal offset")
	}
	keyRegion := view.keys
	if headBytes != 0 {
		view.heads = keyRegion[dataBytes : dataBytes+count*headBytes]
		view.headValid = keyRegion[dataBytes+count*headBytes:]
	}
	view.keys = keyRegion[:dataBytes]
	if !allZero(view.ranks[count:]) ||
		!allZero(view.offsets[(count+1)*2:]) ||
		!allZero(image[segmentedTabletRouterLabAnchorKeysAt+keyBytes:segmentedTabletRouterLabAnchorTrailerAt]) {
		return segmentedTabletRouterLabAnchorView{},
			segmentedTabletRouterLabCorrupt("anchor non-canonical padding")
	}
	var seen [4]uint64
	var previous segmentedTabletRouterLabFence
	var restart segmentedTabletRouterLabFence
	expectedCommon := -1
	for rank := 0; rank < count; rank++ {
		slot := view.ranks[rank]
		word, bit := slot>>6, uint64(1)<<(slot&63)
		if seen[word]&bit != 0 {
			return segmentedTabletRouterLabAnchorView{},
				segmentedTabletRouterLabCorrupt("duplicate row slot")
		}
		seen[word] |= bit
		localID := binary.LittleEndian.Uint16(view.localIDs[int(slot)*2:])
		if localID >= TabletLocalIdentityLabLocalCount {
			return segmentedTabletRouterLabAnchorView{},
				segmentedTabletRouterLabCorrupt("row LocalID")
		}
		bucket, _ := MakeTabletLocalIdentityLabBucket(
			root.tabletID, uint32(localID),
		)
		if _, _, ok := view.handleAt(slot, BucketID(bucket)); !ok {
			return segmentedTabletRouterLabAnchorView{},
				segmentedTabletRouterLabCorrupt("leaf handle")
		}
		fence, ok := view.fenceAtChecked(rank)
		if !ok || rank == 0 && pageID == root.rootRanks[0] &&
			fence.length() != 0 ||
			rank != 0 && segmentedTabletRouterLabCompareFences(
				previous, fence,
			) >= 0 {
			return segmentedTabletRouterLabAnchorView{},
				segmentedTabletRouterLabCorrupt("anchor fences")
		}
		if rank == 0 {
			expectedCommon = fence.length()
		} else {
			expectedCommon = min(
				expectedCommon,
				segmentedTabletRouterLabFencePrefix(
					view.fenceAt(0), fence,
				),
			)
		}
		start := int(binary.LittleEndian.Uint16(view.offsets[rank*2:]))
		if rank%segmentedTabletRouterLabRestart == 0 {
			restart = fence
			if view.keys[start] != 0 {
				return segmentedTabletRouterLabAnchorView{},
					segmentedTabletRouterLabCorrupt("restart sharing")
			}
		} else {
			wantShared := segmentedTabletRouterLabFencePrefix(
				restart, fence,
			) - common
			if wantShared < 0 || int(view.keys[start]) != wantShared {
				return segmentedTabletRouterLabAnchorView{},
					segmentedTabletRouterLabCorrupt("non-canonical sharing")
			}
		}
		previous = fence
	}
	if expectedCommon != common {
		return segmentedTabletRouterLabAnchorView{},
			segmentedTabletRouterLabCorrupt("non-canonical common prefix")
	}
	expectedHeadBytes := 0
	for _, candidate := range [...]int{4, 2, 1} {
		if dataBytes+count*candidate+
			SegmentedTabletRouterLabRowsPerPage/8 <=
			segmentedTabletRouterLabAnchorKeyCapacity {
			expectedHeadBytes = candidate
			break
		}
	}
	if expectedHeadBytes != headBytes {
		return segmentedTabletRouterLabAnchorView{},
			segmentedTabletRouterLabCorrupt("non-canonical head accelerator")
	}
	for rank := 0; rank < count && headBytes != 0; rank++ {
		fence := view.fenceAt(rank)
		valid := fence.length()-common >= headBytes
		bit := view.headValid[rank>>3]&(byte(1)<<uint(rank&7)) != 0
		if valid != bit {
			return segmentedTabletRouterLabAnchorView{},
				segmentedTabletRouterLabCorrupt("anchor head validity")
		}
		if valid {
			for at := 0; at < headBytes; at++ {
				if view.heads[rank*headBytes+at] != fence.at(common+at) {
					return segmentedTabletRouterLabAnchorView{},
						segmentedTabletRouterLabCorrupt("anchor head value")
				}
			}
		} else if !allZero(
			view.heads[rank*headBytes : (rank+1)*headBytes],
		) {
			return segmentedTabletRouterLabAnchorView{},
				segmentedTabletRouterLabCorrupt("anchor short head")
		}
	}
	for slot := 0; slot < SegmentedTabletRouterLabRowsPerPage; slot++ {
		word, bit := uint8(slot)>>6, uint64(1)<<(uint8(slot)&63)
		localID := binary.LittleEndian.Uint16(view.localIDs[slot*2:])
		handle := view.handles[slot*SegmentedTabletRouterLabHandleBytes : (slot+1)*SegmentedTabletRouterLabHandleBytes]
		if seen[word]&bit == 0 &&
			(localID != segmentedTabletRouterLabEmpty || !allZero(handle)) {
			return segmentedTabletRouterLabAnchorView{},
				segmentedTabletRouterLabCorrupt("unused row state")
		}
	}
	return view, nil
}

// Route hashes once and completes the resident root + anchor-page route.
func (v *SegmentedTabletRouterLabView) Route(
	seed [16]byte, key []byte,
) SegmentedTabletRouterLabRoute {
	return v.RouteHashed(KeyHashBytes(seed, key), key)
}

// RouteHashed reuses a caller-owned hash. The route is allocation-free.
func (v *SegmentedTabletRouterLabView) RouteHashed(
	hash uint64, key []byte,
) SegmentedTabletRouterLabRoute {
	if v == nil || len(v.root) == 0 {
		return SegmentedTabletRouterLabRoute{Hash: hash}
	}
	pageRank := v.rootUpperBound(key) - 1
	pageID := v.rootRanks[pageRank]
	page := &v.pages[pageID]
	rowRank := page.upperBound(key) - 1
	return page.routeAt(rowRank, hash)
}

// ResolveBucketID is the posting-driven route through the exact 8 KiB dense
// locator. It performs no lexical comparisons.
func (v *SegmentedTabletRouterLabView) ResolveBucketID(
	bucket BucketID,
) (PageRef, BucketZone, bool) {
	if v == nil || uint32(bucket) >= PrimaryBucketIDLimit ||
		uint32(bucket)>>TabletLocalIdentityLabLocalBits != v.tabletID {
		return PageRef{}, BucketZone{}, false
	}
	localID := uint16(uint32(bucket) & (TabletLocalIdentityLabLocalCount - 1))
	code := binary.LittleEndian.Uint16(v.locator[int(localID)*2:])
	if code == segmentedTabletRouterLabEmpty {
		return PageRef{}, BucketZone{}, false
	}
	pageID, slot := uint8(code>>8), uint8(code)
	if pageID >= SegmentedTabletRouterLabMaxPages ||
		len(v.pages[pageID].image) == 0 ||
		binary.LittleEndian.Uint16(
			v.pages[pageID].localIDs[int(slot)*2:],
		) != localID {
		return PageRef{}, BucketZone{}, false
	}
	return v.pages[pageID].handleAt(slot, bucket)
}

// LowerBound returns the leaf interval that can contain target. Next walks
// anchor lexical ranks and then the root's page-rank permutation.
func (v *SegmentedTabletRouterLabView) LowerBound(
	target []byte,
) SegmentedTabletRouterLabCursor {
	if v == nil || len(v.root) == 0 {
		return SegmentedTabletRouterLabCursor{}
	}
	pageRank := v.rootUpperBound(target) - 1
	pageID := v.rootRanks[pageRank]
	rowRank := v.pages[pageID].upperBound(target) - 1
	return SegmentedTabletRouterLabCursor{
		view: v, pageRank: uint8(pageRank), rowRank: uint16(rowRank), valid: true,
	}
}

func (c *SegmentedTabletRouterLabCursor) Route(
	hash uint64,
) (SegmentedTabletRouterLabRoute, bool) {
	if c == nil || !c.valid {
		return SegmentedTabletRouterLabRoute{}, false
	}
	pageID := c.view.rootRanks[c.pageRank]
	return c.view.pages[pageID].routeAt(int(c.rowRank), hash), true
}

func (c *SegmentedTabletRouterLabCursor) Next() bool {
	if c == nil || !c.valid {
		return false
	}
	pageID := c.view.rootRanks[c.pageRank]
	if int(c.rowRank)+1 < int(c.view.pages[pageID].count) {
		c.rowRank++
		return true
	}
	if int(c.pageRank)+1 >= int(c.view.pageCount) {
		c.valid = false
		return false
	}
	c.pageRank++
	c.rowRank = 0
	return true
}

func (v *SegmentedTabletRouterLabView) rootUpperBound(key []byte) int {
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

func (v *SegmentedTabletRouterLabView) rootFence(rank int) segmentedTabletRouterLabFence {
	start := int(binary.LittleEndian.Uint16(v.rootOffsets[rank*2:]))
	end := int(binary.LittleEndian.Uint16(v.rootOffsets[(rank+1)*2:]))
	return segmentedTabletRouterLabFence{a: v.rootKeys[start:end]}
}

func (p *segmentedTabletRouterLabAnchorView) upperBound(key []byte) int {
	common := p.keys[:p.common]
	compared := min(len(common), len(key))
	if order := bytes.Compare(common[:compared], key[:compared]); order != 0 {
		if order > 0 {
			return 1
		}
		return int(p.count)
	}
	if compared != len(common) {
		return 1
	}
	key = key[compared:]
	switch p.headBytes {
	case 4:
		if len(key) >= 4 {
			want := binary.BigEndian.Uint32(key)
			low, high := 1, int(p.count)
			for low < high {
				mid := int(uint(low+high) >> 1)
				if p.headValid[mid>>3]&(byte(1)<<uint(mid&7)) != 0 {
					got := binary.BigEndian.Uint32(p.heads[mid*4:])
					if got < want {
						low = mid + 1
						continue
					}
					if got > want {
						high = mid
						continue
					}
				}
				fence := p.fenceAt(mid)
				if segmentedTabletRouterLabCompareFenceSuffixKey(
					fence.b, fence.c, key,
				) <= 0 {
					low = mid + 1
				} else {
					high = mid
				}
			}
			return low
		}
	case 2:
		if len(key) >= 2 {
			want := binary.BigEndian.Uint16(key)
			low, high := 1, int(p.count)
			for low < high {
				mid := int(uint(low+high) >> 1)
				if p.headValid[mid>>3]&(byte(1)<<uint(mid&7)) != 0 {
					got := binary.BigEndian.Uint16(p.heads[mid*2:])
					if got < want {
						low = mid + 1
						continue
					}
					if got > want {
						high = mid
						continue
					}
				}
				fence := p.fenceAt(mid)
				if segmentedTabletRouterLabCompareFenceSuffixKey(
					fence.b, fence.c, key,
				) <= 0 {
					low = mid + 1
				} else {
					high = mid
				}
			}
			return low
		}
	case 1:
		if len(key) >= 1 {
			want := key[0]
			low, high := 1, int(p.count)
			for low < high {
				mid := int(uint(low+high) >> 1)
				if p.headValid[mid>>3]&(byte(1)<<uint(mid&7)) != 0 {
					got := p.heads[mid]
					if got < want {
						low = mid + 1
						continue
					}
					if got > want {
						high = mid
						continue
					}
				}
				fence := p.fenceAt(mid)
				if segmentedTabletRouterLabCompareFenceSuffixKey(
					fence.b, fence.c, key,
				) <= 0 {
					low = mid + 1
				} else {
					high = mid
				}
			}
			return low
		}
	}
	low, high := 1, int(p.count)
	for low < high {
		mid := int(uint(low+high) >> 1)
		fence := p.fenceAt(mid)
		if segmentedTabletRouterLabCompareFenceSuffixKey(
			fence.b, fence.c, key,
		) <= 0 {
			low = mid + 1
		} else {
			high = mid
		}
	}
	return low
}

func (p *segmentedTabletRouterLabAnchorView) fenceAt(
	rank int,
) segmentedTabletRouterLabFence {
	fence, _ := p.fenceAtChecked(rank)
	return fence
}

func (p *segmentedTabletRouterLabAnchorView) fenceAtChecked(
	rank int,
) (segmentedTabletRouterLabFence, bool) {
	if rank < 0 || rank >= int(p.count) || len(p.keys) < int(p.common) {
		return segmentedTabletRouterLabFence{}, false
	}
	start := int(binary.LittleEndian.Uint16(p.offsets[rank*2:]))
	end := int(binary.LittleEndian.Uint16(p.offsets[(rank+1)*2:]))
	if start < int(p.common) || start > end || end > len(p.keys) ||
		end-start < 1 {
		return segmentedTabletRouterLabFence{}, false
	}
	shared := int(p.keys[start])
	var restart []byte
	if rank%segmentedTabletRouterLabRestart != 0 {
		restartRank := rank - rank%segmentedTabletRouterLabRestart
		restartStart := int(binary.LittleEndian.Uint16(
			p.offsets[restartRank*2:],
		))
		restartEnd := int(binary.LittleEndian.Uint16(
			p.offsets[(restartRank+1)*2:],
		))
		if restartStart < int(p.common) || restartEnd-restartStart < 1 ||
			restartEnd > len(p.keys) || p.keys[restartStart] != 0 ||
			restartStart > restartEnd {
			return segmentedTabletRouterLabFence{}, false
		}
		restart = p.keys[restartStart+1 : restartEnd]
		if shared > len(restart) {
			return segmentedTabletRouterLabFence{}, false
		}
	} else if shared != 0 {
		return segmentedTabletRouterLabFence{}, false
	}
	return segmentedTabletRouterLabFence{
		a: p.keys[:p.common],
		b: restart[:shared],
		c: p.keys[start+1 : end],
	}, true
}

func (p *segmentedTabletRouterLabAnchorView) routeAt(
	rank int, hash uint64,
) SegmentedTabletRouterLabRoute {
	slot := p.ranks[rank]
	localID := binary.LittleEndian.Uint16(p.localIDs[int(slot)*2:])
	bucket, _ := MakeTabletLocalIdentityLabBucket(
		p.tabletID, uint32(localID),
	)
	ref, zone, _ := p.handleAt(slot, BucketID(bucket))
	return SegmentedTabletRouterLabRoute{
		Bucket: BucketID(bucket), Hash: hash, PageID: p.pageID,
		RowSlot: slot, Ref: ref, Zone: zone,
	}
}

func (p *segmentedTabletRouterLabAnchorView) handleAt(
	slot uint8, bucket BucketID,
) (PageRef, BucketZone, bool) {
	start := int(slot) * SegmentedTabletRouterLabHandleBytes
	src := p.handles[start : start+SegmentedTabletRouterLabHandleBytes]
	offsetUnits := segmentedTabletRouterLabGetUint48(src)
	generation := segmentedTabletRouterLabGetUint48(src[6:])
	length := uint32(binary.LittleEndian.Uint16(src[12:14])) + 1
	var zone BucketZone
	copy(zone[:], src[14:18])
	logicalID, ok := SegmentedTabletRouterLabLeafLogicalID(bucket)
	if !ok {
		return PageRef{}, BucketZone{}, false
	}
	ref := PageRef{
		Offset: offsetUnits << 3, LogicalID: logicalID,
		Generation: generation, Length: length, Kind: p.leafKind,
	}
	if segmentedTabletRouterLabValidateLeafRef(
		ref, bucket, p.leafKind, p.generation,
	) != nil {
		return PageRef{}, BucketZone{}, false
	}
	return ref, zone, true
}

func (v *SegmentedTabletRouterLabView) anchorRef(
	pageID uint8,
) (PageRef, bool) {
	start := int(pageID) * segmentedTabletRouterLabRootRefBytes
	src := v.rootRefs[start : start+segmentedTabletRouterLabRootRefBytes]
	if allZero(src) {
		return PageRef{}, false
	}
	logicalID, ok := SegmentedTabletRouterLabAnchorLogicalID(
		v.tabletID, pageID,
	)
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
			TabletID: v.tabletID, Generation: v.generation,
			AnchorKind: v.anchorKind, LeafKind: v.leafKind,
		},
		pageID,
	) == nil
}

// RewriteHandle performs the ordinary COW path. It never reads or writes the
// locator. Exactly one 8 KiB anchor page and the 4 KiB root are produced.
func (v *SegmentedTabletRouterLabView) RewriteHandle(
	rootDst, pageDst []byte,
	generation uint64,
	bucket BucketID,
	leafRef PageRef,
	zone BucketZone,
	anchorRef PageRef,
) (SegmentedTabletRouterLabCOWResult, error) {
	var result SegmentedTabletRouterLabCOWResult
	if v == nil || len(rootDst) < SegmentedTabletRouterLabRootBytes ||
		len(pageDst) < SegmentedTabletRouterLabAnchorPageBytes ||
		generation <= v.generation || generation >= uint64(1)<<48 {
		return result, fmt.Errorf("%w: ordinary COW geometry", ErrInvalidWrite)
	}
	tabletID, localID, ok := SplitTabletLocalIdentityLabBucket(uint32(bucket))
	if !ok || tabletID != v.tabletID {
		return result, ErrSegmentedTabletRouterLabNotFound
	}
	code := binary.LittleEndian.Uint16(v.locator[int(localID)*2:])
	if code == segmentedTabletRouterLabEmpty {
		return result, ErrSegmentedTabletRouterLabNotFound
	}
	pageID, slot := uint8(code>>8), uint8(code)
	page := &v.pages[pageID]
	if len(page.image) == 0 ||
		binary.LittleEndian.Uint16(page.localIDs[int(slot)*2:]) != localID ||
		leafRef.Generation != generation ||
		segmentedTabletRouterLabValidateLeafRef(
			leafRef, bucket, v.leafKind, generation,
		) != nil {
		return result, fmt.Errorf("%w: ordinary COW leaf", ErrInvalidWrite)
	}
	nextHeader := SegmentedTabletRouterLabHeader{
		TabletID: v.tabletID, Generation: generation,
		AnchorKind: v.anchorKind, LeafKind: v.leafKind,
	}
	if anchorRef.Generation != generation ||
		segmentedTabletRouterLabValidateAnchorRef(
			anchorRef, nextHeader, pageID,
		) != nil {
		return result, fmt.Errorf("%w: ordinary COW anchor ref", ErrInvalidWrite)
	}
	nextPage := pageDst[:SegmentedTabletRouterLabAnchorPageBytes]
	copy(nextPage, page.image)
	binary.LittleEndian.PutUint64(nextPage[24:32], generation)
	segmentedTabletRouterLabEncodeLeafHandle(
		nextPage[segmentedTabletRouterLabAnchorHandlesAt+
			int(slot)*SegmentedTabletRouterLabHandleBytes:],
		leafRef, zone,
	)
	segmentedTabletRouterLabSeal(
		nextPage, segmentedTabletRouterLabAnchorTrailerAt,
	)
	nextRoot := rootDst[:SegmentedTabletRouterLabRootBytes]
	copy(nextRoot, v.root)
	binary.LittleEndian.PutUint64(nextRoot[24:32], generation)
	segmentedTabletRouterLabEncodeAnchorRef(
		nextRoot[segmentedTabletRouterLabRootRefsAt+
			int(pageID)*segmentedTabletRouterLabRootRefBytes:],
		anchorRef,
	)
	segmentedTabletRouterLabSeal(
		nextRoot, segmentedTabletRouterLabRootTrailerAt,
	)
	return SegmentedTabletRouterLabCOWResult{
		Root: nextRoot, AnchorPage: nextPage, PageID: pageID,
		Bytes: SegmentedTabletRouterLabRootBytes +
			SegmentedTabletRouterLabAnchorPageBytes,
	}, nil
}

// SplitAnchorPage splits one lexical page at splitRank. Stable row slots are
// preserved; only moved LocalIDs change their high locator byte.
func (v *SegmentedTabletRouterLabView) SplitAnchorPage(
	rootDst, locatorDst, leftDst, rightDst []byte,
	generation uint64,
	pageID, newPageID uint8,
	splitRank int,
	leftRef, rightRef PageRef,
) (SegmentedTabletRouterLabSplitResult, error) {
	var result SegmentedTabletRouterLabSplitResult
	if v == nil || len(rootDst) < SegmentedTabletRouterLabRootBytes ||
		len(locatorDst) < SegmentedTabletRouterLabLocatorBytes ||
		len(leftDst) < SegmentedTabletRouterLabAnchorPageBytes ||
		len(rightDst) < SegmentedTabletRouterLabAnchorPageBytes ||
		generation <= v.generation || generation >= uint64(1)<<48 ||
		pageID >= SegmentedTabletRouterLabMaxPages ||
		newPageID >= SegmentedTabletRouterLabMaxPages ||
		pageID == newPageID || len(v.pages[pageID].image) == 0 ||
		len(v.pages[newPageID].image) != 0 ||
		newPageID != v.pageCount ||
		int(v.pageCount) >= SegmentedTabletRouterLabMaxPages {
		return result, fmt.Errorf("%w: structural split geometry", ErrInvalidWrite)
	}
	source := &v.pages[pageID]
	if splitRank <= 0 || splitRank >= int(source.count) {
		return result, fmt.Errorf("%w: structural split rank", ErrInvalidWrite)
	}
	nextHeader := SegmentedTabletRouterLabHeader{
		TabletID: v.tabletID, Generation: generation,
		AnchorKind: v.anchorKind, LeafKind: v.leafKind,
	}
	if leftRef.Generation != generation || rightRef.Generation != generation ||
		segmentedTabletRouterLabValidateAnchorRef(
			leftRef, nextHeader, pageID,
		) != nil ||
		segmentedTabletRouterLabValidateAnchorRef(
			rightRef, nextHeader, newPageID,
		) != nil {
		return result, fmt.Errorf("%w: structural split refs", ErrInvalidWrite)
	}
	left := leftDst[:SegmentedTabletRouterLabAnchorPageBytes]
	right := rightDst[:SegmentedTabletRouterLabAnchorPageBytes]
	if _, err := segmentedTabletRouterLabEncodeAnchorSubset(
		left, nextHeader, pageID, source, 0, splitRank,
	); err != nil {
		return result, err
	}
	if _, err := segmentedTabletRouterLabEncodeAnchorSubset(
		right, nextHeader, newPageID, source, splitRank, int(source.count),
	); err != nil {
		return result, err
	}
	locator := locatorDst[:SegmentedTabletRouterLabLocatorBytes]
	copy(locator, v.locator)
	for rank := splitRank; rank < int(source.count); rank++ {
		slot := source.ranks[rank]
		localID := binary.LittleEndian.Uint16(source.localIDs[int(slot)*2:])
		binary.LittleEndian.PutUint16(
			locator[int(localID)*2:],
			uint16(newPageID)<<8|uint16(slot),
		)
	}
	root := rootDst[:SegmentedTabletRouterLabRootBytes]
	if err := v.encodeSplitRoot(
		root, locator, nextHeader, pageID, newPageID,
		source.fenceAt(splitRank), leftRef, rightRef,
	); err != nil {
		return result, err
	}
	return SegmentedTabletRouterLabSplitResult{
		Root: root, Locator: locator, LeftPage: left, RightPage: right,
		LeftPageID: pageID, RightPageID: newPageID,
		Bytes: SegmentedTabletRouterLabRootBytes +
			SegmentedTabletRouterLabLocatorBytes +
			2*SegmentedTabletRouterLabAnchorPageBytes,
	}, nil
}

// RoutingBytesPerDocument reports the exact fixed routing footprint at a
// specified leaf count and average leaf occupancy.
func SegmentedTabletRouterLabRoutingBytesPerDocument(
	leafCount, rowsPerLeaf int,
) float64 {
	if leafCount <= 0 || leafCount > TabletLocalIdentityLabLocalCount ||
		rowsPerLeaf <= 0 {
		return 0
	}
	pages := (leafCount + SegmentedTabletRouterLabRowsPerPage - 1) /
		SegmentedTabletRouterLabRowsPerPage
	bytes := SegmentedTabletRouterLabRootBytes +
		SegmentedTabletRouterLabLocatorBytes +
		pages*SegmentedTabletRouterLabAnchorPageBytes
	return float64(bytes) / float64(leafCount*rowsPerLeaf)
}

// SegmentedTabletRouterLabLeafLogicalID derives the collision-free durable
// logical ID for one posting-stable 30-bit BucketID.
func SegmentedTabletRouterLabLeafLogicalID(
	bucket BucketID,
) (uint64, bool) {
	if uint32(bucket) >= PrimaryBucketIDLimit {
		return 0, false
	}
	return SegmentedTabletRouterLabLeafLogicalIDBase + uint64(bucket), true
}

// SegmentedTabletRouterLabAnchorLogicalID derives the collision-free durable
// logical ID for one stable anchor-page identity.
func SegmentedTabletRouterLabAnchorLogicalID(
	tabletID uint32, pageID uint8,
) (uint64, bool) {
	if tabletID >= TabletLocalIdentityLabTabletCount ||
		pageID >= SegmentedTabletRouterLabMaxPages {
		return 0, false
	}
	ordinal := uint64(tabletID)*SegmentedTabletRouterLabMaxPages +
		uint64(pageID)
	return SegmentedTabletRouterLabAnchorLogicalIDBase + ordinal, true
}

// segmentedTabletRouterLabAnchorLogicalID is the terse internal form used only
// after geometry has already been validated.
func segmentedTabletRouterLabAnchorLogicalID(
	tabletID uint32, pageID uint8,
) uint64 {
	logicalID, _ := SegmentedTabletRouterLabAnchorLogicalID(tabletID, pageID)
	return logicalID
}

// SegmentedTabletRouterLabIsDynamicLogicalID reports whether id is available
// to tablet roots, catalogs, indexes, and other non-derived logical pages.
func SegmentedTabletRouterLabIsDynamicLogicalID(id uint64) bool {
	return id >= SegmentedTabletRouterLabFirstDynamicLogicalID
}

func segmentedTabletRouterLabEncodeAnchorFromLeaves(
	dst []byte,
	header SegmentedTabletRouterLabHeader,
	pageID uint8,
	leaves []SegmentedTabletRouterLabLeaf,
) ([]byte, error) {
	return segmentedTabletRouterLabEncodeAnchor(
		dst, header, pageID, len(leaves),
		func(rank int) segmentedTabletRouterLabFence {
			return segmentedTabletRouterLabFence{a: leaves[rank].Fence}
		},
		func(rank int) (uint8, uint16, PageRef, BucketZone) {
			return uint8(rank), leaves[rank].LocalID,
				leaves[rank].Ref, leaves[rank].Zone
		},
	)
}

func segmentedTabletRouterLabEncodeAnchorSubset(
	dst []byte,
	header SegmentedTabletRouterLabHeader,
	pageID uint8,
	source *segmentedTabletRouterLabAnchorView,
	first, last int,
) ([]byte, error) {
	return segmentedTabletRouterLabEncodeAnchor(
		dst, header, pageID, last-first,
		func(rank int) segmentedTabletRouterLabFence {
			return source.fenceAt(first + rank)
		},
		func(rank int) (uint8, uint16, PageRef, BucketZone) {
			slot := source.ranks[first+rank]
			localID := binary.LittleEndian.Uint16(
				source.localIDs[int(slot)*2:],
			)
			bucket, _ := MakeTabletLocalIdentityLabBucket(
				header.TabletID, uint32(localID),
			)
			ref, zone, _ := source.handleAt(slot, BucketID(bucket))
			return slot, localID, ref, zone
		},
	)
}

func segmentedTabletRouterLabEncodeAnchor(
	dst []byte,
	header SegmentedTabletRouterLabHeader,
	pageID uint8,
	count int,
	fenceAt func(int) segmentedTabletRouterLabFence,
	rowAt func(int) (uint8, uint16, PageRef, BucketZone),
) ([]byte, error) {
	if len(dst) < SegmentedTabletRouterLabAnchorPageBytes ||
		count <= 0 || count > SegmentedTabletRouterLabRowsPerPage {
		return nil, fmt.Errorf("%w: anchor encode geometry", ErrInvalidWrite)
	}
	image := dst[:SegmentedTabletRouterLabAnchorPageBytes]
	clear(image)
	copy(image[:8], segmentedTabletRouterLabPageMagic)
	binary.LittleEndian.PutUint32(image[8:12], segmentedTabletRouterLabVersion)
	binary.LittleEndian.PutUint16(
		image[12:14], segmentedTabletRouterLabAnchorHeaderBytes,
	)
	image[14] = pageID
	image[15] = segmentedTabletRouterLabRestart
	binary.LittleEndian.PutUint32(image[16:20], header.TabletID)
	binary.LittleEndian.PutUint16(image[20:22], uint16(count))
	binary.LittleEndian.PutUint64(image[24:32], header.Generation)
	image[40] = byte(header.LeafKind)
	for at := segmentedTabletRouterLabAnchorLocalIDsAt; at < segmentedTabletRouterLabAnchorHandlesAt; at++ {
		image[at] = 0xff
	}
	firstFence := fenceAt(0)
	common := firstFence.length()
	for rank := 1; rank < count; rank++ {
		common = min(
			common,
			segmentedTabletRouterLabFencePrefix(firstFence, fenceAt(rank)),
		)
	}
	if common > 255 {
		return nil, fmt.Errorf("%w: anchor common prefix", ErrInvalidWrite)
	}
	image[32] = byte(common)
	keys := image[segmentedTabletRouterLabAnchorKeysAt:segmentedTabletRouterLabAnchorTrailerAt]
	firstFence.copyTo(keys, 0)
	keyAt := common
	var seen [4]uint64
	var restart segmentedTabletRouterLabFence
	for rank := 0; rank < count; rank++ {
		fence := fenceAt(rank)
		slot, localID, ref, zone := rowAt(rank)
		word, bit := slot>>6, uint64(1)<<(slot&63)
		bucket, ok := MakeTabletLocalIdentityLabBucket(
			header.TabletID, uint32(localID),
		)
		if !ok || seen[word]&bit != 0 ||
			segmentedTabletRouterLabValidateLeafRef(
				ref, BucketID(bucket), header.LeafKind,
				header.Generation,
			) != nil {
			return nil, fmt.Errorf("%w: anchor row", ErrInvalidWrite)
		}
		seen[word] |= bit
		image[segmentedTabletRouterLabAnchorRanksAt+rank] = slot
		binary.LittleEndian.PutUint16(
			image[segmentedTabletRouterLabAnchorLocalIDsAt+int(slot)*2:],
			localID,
		)
		segmentedTabletRouterLabEncodeLeafHandle(
			image[segmentedTabletRouterLabAnchorHandlesAt+
				int(slot)*SegmentedTabletRouterLabHandleBytes:],
			ref, zone,
		)
		binary.LittleEndian.PutUint16(
			image[segmentedTabletRouterLabAnchorOffsetsAt+rank*2:],
			uint16(keyAt),
		)
		shared := 0
		if rank%segmentedTabletRouterLabRestart == 0 {
			restart = fence
		} else {
			shared = segmentedTabletRouterLabFencePrefix(restart, fence)
			shared = max(0, shared-common)
		}
		suffixAt := common + shared
		suffix := fence.length() - suffixAt
		if shared > 255 || suffix < 0 || suffix > 255 ||
			keyAt+1+suffix > len(keys) {
			return nil, fmt.Errorf(
				"%w: compressed anchor fence arena",
				ErrSegmentedTabletRouterLabNoSpace,
			)
		}
		keys[keyAt] = byte(shared)
		keyAt++
		keyAt += fence.copyTo(keys[keyAt:keyAt+suffix], suffixAt)
	}
	binary.LittleEndian.PutUint16(
		image[segmentedTabletRouterLabAnchorOffsetsAt+count*2:],
		uint16(keyAt),
	)
	headBytes := 0
	validBytes := SegmentedTabletRouterLabRowsPerPage / 8
	for _, candidate := range [...]int{4, 2, 1} {
		if keyAt+count*candidate+validBytes <= len(keys) {
			headBytes = candidate
			break
		}
	}
	if headBytes != 0 {
		heads := keys[keyAt : keyAt+count*headBytes]
		valid := keys[keyAt+count*headBytes : keyAt+count*headBytes+validBytes]
		for rank := 0; rank < count; rank++ {
			fence := fenceAt(rank)
			if fence.length()-common < headBytes {
				continue
			}
			valid[rank>>3] |= byte(1) << uint(rank&7)
			for at := 0; at < headBytes; at++ {
				heads[rank*headBytes+at] = fence.at(common + at)
			}
		}
		keyAt += count*headBytes + validBytes
	}
	image[33] = byte(headBytes)
	binary.LittleEndian.PutUint16(image[22:24], uint16(keyAt))
	binary.LittleEndian.PutUint16(image[34:36], uint16(count))
	binary.LittleEndian.PutUint32(
		image[36:40], SegmentedTabletRouterLabAnchorPageBytes,
	)
	segmentedTabletRouterLabSeal(
		image, segmentedTabletRouterLabAnchorTrailerAt,
	)
	return image, nil
}

func segmentedTabletRouterLabEncodeRootInitial(
	root, locator []byte,
	header SegmentedTabletRouterLabHeader,
	anchorRefs []PageRef,
	leaves []SegmentedTabletRouterLabLeaf,
) error {
	clear(root)
	pageCount := len(anchorRefs)
	copy(root[:8], segmentedTabletRouterLabRootMagic)
	binary.LittleEndian.PutUint32(root[8:12], segmentedTabletRouterLabVersion)
	binary.LittleEndian.PutUint16(
		root[12:14], segmentedTabletRouterLabRootHeaderBytes,
	)
	root[14] = byte(pageCount)
	root[15] = byte(header.AnchorKind)
	root[16] = byte(header.LeafKind)
	binary.LittleEndian.PutUint32(root[20:24], header.TabletID)
	binary.LittleEndian.PutUint64(root[24:32], header.Generation)
	binary.LittleEndian.PutUint16(root[34:36], uint16(pageCount))
	binary.LittleEndian.PutUint32(root[36:40], PageChecksum(locator))
	binary.LittleEndian.PutUint32(
		root[40:44], SegmentedTabletRouterLabRootBytes,
	)
	keyAt := 0
	for rank := 0; rank < pageCount; rank++ {
		root[segmentedTabletRouterLabRootRanksAt+rank] = byte(rank)
		segmentedTabletRouterLabEncodeAnchorRef(
			root[segmentedTabletRouterLabRootRefsAt+
				rank*segmentedTabletRouterLabRootRefBytes:],
			anchorRefs[rank],
		)
		binary.LittleEndian.PutUint16(
			root[segmentedTabletRouterLabRootOffsetsAt+rank*2:],
			uint16(keyAt),
		)
		fence := leaves[rank*SegmentedTabletRouterLabRowsPerPage].Fence
		if keyAt+len(fence) >
			segmentedTabletRouterLabRootTrailerAt-
				segmentedTabletRouterLabRootKeysAt {
			return fmt.Errorf("%w: root fence arena", ErrSegmentedTabletRouterLabNoSpace)
		}
		copy(root[segmentedTabletRouterLabRootKeysAt+keyAt:], fence)
		keyAt += len(fence)
	}
	binary.LittleEndian.PutUint16(
		root[segmentedTabletRouterLabRootOffsetsAt+pageCount*2:],
		uint16(keyAt),
	)
	binary.LittleEndian.PutUint16(root[32:34], uint16(keyAt))
	segmentedTabletRouterLabSeal(root, segmentedTabletRouterLabRootTrailerAt)
	return nil
}

func (v *SegmentedTabletRouterLabView) encodeSplitRoot(
	dst, locator []byte,
	header SegmentedTabletRouterLabHeader,
	pageID, newPageID uint8,
	rightFloor segmentedTabletRouterLabFence,
	leftRef, rightRef PageRef,
) error {
	clear(dst[:SegmentedTabletRouterLabRootBytes])
	root := dst[:SegmentedTabletRouterLabRootBytes]
	copy(root[:segmentedTabletRouterLabRootHeaderBytes],
		v.root[:segmentedTabletRouterLabRootHeaderBytes])
	root[14] = v.pageCount + 1
	binary.LittleEndian.PutUint64(root[24:32], header.Generation)
	binary.LittleEndian.PutUint16(root[34:36], uint16(v.pageCount)+1)
	binary.LittleEndian.PutUint32(root[36:40], PageChecksum(locator))
	for stableID := uint8(0); stableID < SegmentedTabletRouterLabMaxPages; stableID++ {
		if stableID == pageID {
			segmentedTabletRouterLabEncodeAnchorRef(
				root[segmentedTabletRouterLabRootRefsAt+
					int(stableID)*segmentedTabletRouterLabRootRefBytes:],
				leftRef,
			)
		} else if stableID == newPageID {
			segmentedTabletRouterLabEncodeAnchorRef(
				root[segmentedTabletRouterLabRootRefsAt+
					int(stableID)*segmentedTabletRouterLabRootRefBytes:],
				rightRef,
			)
		} else {
			start := segmentedTabletRouterLabRootRefsAt +
				int(stableID)*segmentedTabletRouterLabRootRefBytes
			copy(root[start:start+segmentedTabletRouterLabRootRefBytes],
				v.root[start:start+segmentedTabletRouterLabRootRefBytes])
		}
	}
	insertRank := -1
	for rank := 0; rank < int(v.pageCount); rank++ {
		if v.rootRanks[rank] == pageID {
			insertRank = rank + 1
			break
		}
	}
	if insertRank < 1 {
		return fmt.Errorf("%w: split source rank", ErrInvalidWrite)
	}
	keyAt := 0
	outRank := 0
	for oldRank := 0; oldRank <= int(v.pageCount); oldRank++ {
		if outRank == insertRank {
			root[segmentedTabletRouterLabRootRanksAt+outRank] = newPageID
			binary.LittleEndian.PutUint16(
				root[segmentedTabletRouterLabRootOffsetsAt+outRank*2:],
				uint16(keyAt),
			)
			if keyAt+rightFloor.length() >
				segmentedTabletRouterLabRootTrailerAt-
					segmentedTabletRouterLabRootKeysAt {
				return ErrSegmentedTabletRouterLabNoSpace
			}
			keyAt += rightFloor.copyTo(
				root[segmentedTabletRouterLabRootKeysAt+keyAt:], 0,
			)
			outRank++
		}
		if oldRank == int(v.pageCount) {
			break
		}
		root[segmentedTabletRouterLabRootRanksAt+outRank] =
			v.rootRanks[oldRank]
		binary.LittleEndian.PutUint16(
			root[segmentedTabletRouterLabRootOffsetsAt+outRank*2:],
			uint16(keyAt),
		)
		oldFence := v.rootFence(oldRank)
		if keyAt+oldFence.length() >
			segmentedTabletRouterLabRootTrailerAt-
				segmentedTabletRouterLabRootKeysAt {
			return ErrSegmentedTabletRouterLabNoSpace
		}
		keyAt += oldFence.copyTo(
			root[segmentedTabletRouterLabRootKeysAt+keyAt:], 0,
		)
		outRank++
	}
	binary.LittleEndian.PutUint16(
		root[segmentedTabletRouterLabRootOffsetsAt+outRank*2:],
		uint16(keyAt),
	)
	binary.LittleEndian.PutUint16(root[32:34], uint16(keyAt))
	segmentedTabletRouterLabSeal(root, segmentedTabletRouterLabRootTrailerAt)
	return nil
}

func segmentedTabletRouterLabValidateLeafRef(
	ref PageRef, bucket BucketID, kind PageKind,
	selectingGeneration uint64,
) error {
	logicalID, ok := SegmentedTabletRouterLabLeafLogicalID(bucket)
	if !ok {
		return fmt.Errorf("%w: exact leaf BucketID", ErrInvalidWrite)
	}
	switch {
	case ref.Offset == 0 || ref.Offset&7 != 0 ||
		ref.Offset>>3 >= uint64(1)<<48:
		return fmt.Errorf("%w: exact leaf offset", ErrInvalidWrite)
	case ref.Generation == 0 || ref.Generation >= uint64(1)<<48 ||
		ref.Generation > selectingGeneration:
		return fmt.Errorf("%w: exact leaf generation", ErrInvalidWrite)
	case ref.Length == 0 || ref.Length > 1<<16:
		return fmt.Errorf("%w: exact leaf length", ErrInvalidWrite)
	case ref.LogicalID != logicalID:
		return fmt.Errorf("%w: exact leaf logical ID", ErrInvalidWrite)
	case ref.Kind != kind || ref.Flags != 0 || ref.Aux != 0:
		return fmt.Errorf("%w: exact leaf kind or flags", ErrInvalidWrite)
	}
	return nil
}

func segmentedTabletRouterLabValidateAnchorRef(
	ref PageRef,
	header SegmentedTabletRouterLabHeader,
	pageID uint8,
) error {
	if header.Generation == 0 || header.Generation >= uint64(1)<<48 {
		return fmt.Errorf("%w: tablet-root generation", ErrInvalidWrite)
	}
	logicalID, ok := SegmentedTabletRouterLabAnchorLogicalID(
		header.TabletID, pageID,
	)
	if !ok {
		return fmt.Errorf("%w: anchor identity", ErrInvalidWrite)
	}
	if ref.Offset == 0 || ref.Offset&4095 != 0 ||
		ref.Offset>>12 >= uint64(1)<<48 ||
		ref.Generation == 0 || ref.Generation >= uint64(1)<<48 ||
		ref.Generation > header.Generation ||
		ref.LogicalID != logicalID ||
		ref.Kind != header.AnchorKind || ref.Flags != 0 || ref.Aux != 0 ||
		ref.Length != SegmentedTabletRouterLabAnchorPageBytes {
		return fmt.Errorf("%w: non-canonical anchor ref", ErrInvalidWrite)
	}
	return nil
}

func segmentedTabletRouterLabEncodeLeafHandle(
	dst []byte, ref PageRef, zone BucketZone,
) {
	segmentedTabletRouterLabPutUint48(dst, ref.Offset>>3)
	segmentedTabletRouterLabPutUint48(dst[6:], ref.Generation)
	binary.LittleEndian.PutUint16(dst[12:14], uint16(ref.Length-1))
	copy(dst[14:18], zone[:])
}

func segmentedTabletRouterLabEncodeAnchorRef(dst []byte, ref PageRef) {
	segmentedTabletRouterLabPutUint48(dst, ref.Offset>>12)
	segmentedTabletRouterLabPutUint48(dst[6:], ref.Generation)
	dst[12] = byte(tabletAnchorHandleLabExtentClass(ref.Length))
}

func segmentedTabletRouterLabGetUint48(src []byte) uint64 {
	return uint64(src[0]) |
		uint64(src[1])<<8 |
		uint64(src[2])<<16 |
		uint64(src[3])<<24 |
		uint64(src[4])<<32 |
		uint64(src[5])<<40
}

func segmentedTabletRouterLabPutUint48(dst []byte, value uint64) {
	dst[0] = byte(value)
	dst[1] = byte(value >> 8)
	dst[2] = byte(value >> 16)
	dst[3] = byte(value >> 24)
	dst[4] = byte(value >> 32)
	dst[5] = byte(value >> 40)
}

func segmentedTabletRouterLabSeal(image []byte, trailer int) {
	checksum := PageChecksum(image[:trailer])
	binary.LittleEndian.PutUint32(image[trailer:trailer+4], checksum)
	binary.LittleEndian.PutUint32(image[trailer+4:trailer+8], ^checksum)
}

func segmentedTabletRouterLabChecksumOK(image []byte, trailer int) bool {
	checksum := binary.LittleEndian.Uint32(image[trailer : trailer+4])
	return binary.LittleEndian.Uint32(image[trailer+4:trailer+8]) ==
		^checksum && PageChecksum(image[:trailer]) == checksum
}

func segmentedTabletRouterLabCorrupt(reason string) error {
	return fmt.Errorf("%w: %s", ErrSegmentedTabletRouterLabCorrupt, reason)
}
