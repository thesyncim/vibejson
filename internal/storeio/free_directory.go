package storeio

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	FreeDirectoryPayloadHeaderSize = 32
	FreeDirectoryLeafRecordSize    = 24
	FreeDirectoryBranchRecordSize  = 48
	freeDirectoryVersion           = uint32(1)
	freeDirectoryKnownFlags        = uint8(0)
)

// ErrFreeDirectoryCorrupt reports malformed durable extent metadata.
var ErrFreeDirectoryCorrupt = errors.New("simdjson: corrupt Store free directory")

// FreeExtent is one page-aligned physical range retired by a copy-on-write
// publication. RetiredGeneration is the last Store generation that may reach
// it; reuse additionally waits for every read lease and protected recovery
// root to advance beyond that generation.
type FreeExtent struct {
	Offset            uint64
	Length            uint64
	RetiredGeneration uint64
}

// FreeDirectoryChild is one offset lower bound and child page.
type FreeDirectoryChild struct {
	Lower uint64
	Ref   PageRef
}

// FreeDirectoryHeader describes one immutable extent B+tree node.
type FreeDirectoryHeader struct {
	StoreID    [16]byte
	Generation uint64
	LogicalID  uint64
	PageSize   uint32
	Level      uint8
	Flags      uint8
}

// FreeDirectoryView is one checksum-verified borrowed node.
type FreeDirectoryView struct {
	header  FreeDirectoryHeader
	payload []byte
	count   uint16
}

// EncodeFreeDirectoryLeaf writes non-overlapping extents ordered by offset.
func EncodeFreeDirectoryLeaf(dst []byte, header FreeDirectoryHeader, extents []FreeExtent, fileEnd, nextLogicalID uint64) ([]byte, error) {
	if header.Level != 0 {
		return nil, fmt.Errorf("%w: free leaf level", ErrInvalidWrite)
	}
	if err := validateFreeDirectoryHeader(header, len(extents), FreeDirectoryLeafRecordSize, nextLogicalID); err != nil {
		return nil, err
	}
	var previousEnd uint64
	for i, extent := range extents {
		if err := validateFreeExtent(extent, header.PageSize, fileEnd); err != nil || i != 0 && extent.Offset < previousEnd {
			return nil, fmt.Errorf("%w: free extent order or bounds", ErrInvalidWrite)
		}
		previousEnd = extent.Offset + extent.Length
	}
	payloadLength := FreeDirectoryPayloadHeaderSize + len(extents)*FreeDirectoryLeafRecordSize
	payload, err := InitPage(dst, PageHeader{
		StoreID: header.StoreID, Generation: header.Generation, LogicalID: header.LogicalID,
		PageSize: header.PageSize, PayloadLength: uint32(payloadLength), Kind: PageFreeDirectory,
	})
	if err != nil {
		return nil, err
	}
	encodeFreeDirectoryHeader(payload, header, len(extents))
	for i, extent := range extents {
		record := payload[FreeDirectoryPayloadHeaderSize+i*FreeDirectoryLeafRecordSize:]
		binary.LittleEndian.PutUint64(record[0:8], extent.Offset)
		binary.LittleEndian.PutUint64(record[8:16], extent.Length)
		binary.LittleEndian.PutUint64(record[16:24], extent.RetiredGeneration)
	}
	page := dst[:int(header.PageSize)]
	if _, err := sealInitializedPage(page); err != nil {
		return nil, err
	}
	return page, nil
}

// EncodeFreeDirectoryBranch writes an offset-routing branch with at most 64
// children.
func EncodeFreeDirectoryBranch(dst []byte, header FreeDirectoryHeader, children []FreeDirectoryChild, fileEnd, nextLogicalID uint64) ([]byte, error) {
	if header.Level == 0 || len(children) > 64 {
		return nil, fmt.Errorf("%w: free branch level or fanout", ErrInvalidWrite)
	}
	if err := validateFreeDirectoryHeader(header, len(children), FreeDirectoryBranchRecordSize, nextLogicalID); err != nil {
		return nil, err
	}
	var seen chunkDirectoryRefSet
	for i, child := range children {
		if i != 0 && children[i-1].Lower >= child.Lower {
			return nil, fmt.Errorf("%w: free lower-bound order", ErrInvalidWrite)
		}
		if err := validateFreeDirectoryChild(header, child.Ref, fileEnd, nextLogicalID); err != nil || !seen.add(child.Ref) {
			return nil, fmt.Errorf("%w: free branch child", ErrInvalidWrite)
		}
	}
	payloadLength := FreeDirectoryPayloadHeaderSize + len(children)*FreeDirectoryBranchRecordSize
	payload, err := InitPage(dst, PageHeader{
		StoreID: header.StoreID, Generation: header.Generation, LogicalID: header.LogicalID,
		PageSize: header.PageSize, PayloadLength: uint32(payloadLength), Kind: PageFreeDirectory,
	})
	if err != nil {
		return nil, err
	}
	encodeFreeDirectoryHeader(payload, header, len(children))
	for i, child := range children {
		record := payload[FreeDirectoryPayloadHeaderSize+i*FreeDirectoryBranchRecordSize:]
		binary.LittleEndian.PutUint64(record[0:8], child.Lower)
		encodePageRef(record[16:16+PageRefSize], child.Ref)
	}
	page := dst[:int(header.PageSize)]
	if _, err := sealInitializedPage(page); err != nil {
		return nil, err
	}
	return page, nil
}

func encodeFreeDirectoryHeader(payload []byte, header FreeDirectoryHeader, count int) {
	binary.LittleEndian.PutUint32(payload[0:4], freeDirectoryVersion)
	payload[4] = header.Level
	payload[5] = header.Flags
	binary.LittleEndian.PutUint16(payload[6:8], uint16(count))
}

// OpenFreeDirectoryPage validates one extent leaf or branch.
func OpenFreeDirectoryPage(src []byte, fileEnd, nextLogicalID uint64) (FreeDirectoryView, error) {
	pageHeader, payload, err := OpenPage(src)
	if err != nil {
		return FreeDirectoryView{}, fmt.Errorf("%w: %w", ErrFreeDirectoryCorrupt, err)
	}
	if pageHeader.Kind != PageFreeDirectory || len(payload) < FreeDirectoryPayloadHeaderSize ||
		binary.LittleEndian.Uint32(payload[0:4]) != freeDirectoryVersion ||
		!allZero(payload[8:FreeDirectoryPayloadHeaderSize]) {
		return FreeDirectoryView{}, fmt.Errorf("%w: header, version, or reserved bytes", ErrFreeDirectoryCorrupt)
	}
	header := FreeDirectoryHeader{
		StoreID: pageHeader.StoreID, Generation: pageHeader.Generation,
		LogicalID: pageHeader.LogicalID, PageSize: pageHeader.PageSize,
		Level: payload[4], Flags: payload[5],
	}
	count := int(binary.LittleEndian.Uint16(payload[6:8]))
	recordSize := FreeDirectoryLeafRecordSize
	if header.Level != 0 {
		recordSize = FreeDirectoryBranchRecordSize
	}
	if err := validateFreeDirectoryHeader(header, count, recordSize, nextLogicalID); err != nil ||
		len(payload) != FreeDirectoryPayloadHeaderSize+count*recordSize {
		return FreeDirectoryView{}, fmt.Errorf("%w: node bounds", ErrFreeDirectoryCorrupt)
	}
	var previousEnd uint64
	var previousLower uint64
	var seen chunkDirectoryRefSet
	for i := 0; i < count; i++ {
		record := payload[FreeDirectoryPayloadHeaderSize+i*recordSize:]
		if header.Level == 0 {
			extent := decodeFreeExtent(record)
			if err := validateFreeExtent(extent, header.PageSize, fileEnd); err != nil || i != 0 && extent.Offset < previousEnd {
				return FreeDirectoryView{}, fmt.Errorf("%w: extent order or bounds", ErrFreeDirectoryCorrupt)
			}
			previousEnd = extent.Offset + extent.Length
		} else {
			lower := binary.LittleEndian.Uint64(record[0:8])
			if !allZero(record[8:16]) || !pageRefReservedZero(record[16:16+PageRefSize]) || i != 0 && lower <= previousLower {
				return FreeDirectoryView{}, fmt.Errorf("%w: branch order or reserved bytes", ErrFreeDirectoryCorrupt)
			}
			ref := decodePageRef(record[16 : 16+PageRefSize])
			if err := validateFreeDirectoryChild(header, ref, fileEnd, nextLogicalID); err != nil || !seen.add(ref) {
				return FreeDirectoryView{}, fmt.Errorf("%w: branch child", ErrFreeDirectoryCorrupt)
			}
			previousLower = lower
		}
	}
	return FreeDirectoryView{header: header, payload: payload, count: uint16(count)}, nil
}

// Header returns value-only node metadata.
func (v FreeDirectoryView) Header() FreeDirectoryHeader { return v.header }

// Len returns the number of extents or children.
func (v FreeDirectoryView) Len() int { return int(v.count) }

// ExtentAt returns one leaf extent at rank.
func (v FreeDirectoryView) ExtentAt(rank int) (FreeExtent, bool) {
	if v.header.Level != 0 || rank < 0 || rank >= int(v.count) {
		return FreeExtent{}, false
	}
	record := v.payload[FreeDirectoryPayloadHeaderSize+rank*FreeDirectoryLeafRecordSize:]
	return decodeFreeExtent(record), true
}

// LowerBound returns the first leaf extent whose end is greater than offset.
// It is the candidate containing offset or the first extent after it.
func (v FreeDirectoryView) LowerBound(offset uint64) int {
	if v.header.Level != 0 {
		return 0
	}
	low, high := 0, int(v.count)
	for low < high {
		middle := int(uint(low+high) >> 1)
		extent, _ := v.ExtentAt(middle)
		if extent.Offset+extent.Length <= offset {
			low = middle + 1
		} else {
			high = middle
		}
	}
	return low
}

// Child selects the branch with the greatest lower offset not exceeding key.
func (v FreeDirectoryView) Child(offset uint64) (PageRef, bool) {
	if v.header.Level == 0 || v.count == 0 {
		return PageRef{}, false
	}
	low, high := 0, int(v.count)
	for low < high {
		middle := int(uint(low+high) >> 1)
		record := v.payload[FreeDirectoryPayloadHeaderSize+middle*FreeDirectoryBranchRecordSize:]
		if binary.LittleEndian.Uint64(record[0:8]) <= offset {
			low = middle + 1
		} else {
			high = middle
		}
	}
	if low == 0 {
		return PageRef{}, false
	}
	record := v.payload[FreeDirectoryPayloadHeaderSize+(low-1)*FreeDirectoryBranchRecordSize:]
	return decodePageRef(record[16 : 16+PageRefSize]), true
}

// ChildAt returns one branch lower bound and child at rank.
func (v FreeDirectoryView) ChildAt(rank int) (FreeDirectoryChild, bool) {
	if v.header.Level == 0 || rank < 0 || rank >= int(v.count) {
		return FreeDirectoryChild{}, false
	}
	record := v.payload[FreeDirectoryPayloadHeaderSize+rank*FreeDirectoryBranchRecordSize:]
	return FreeDirectoryChild{
		Lower: binary.LittleEndian.Uint64(record[0:8]),
		Ref:   decodePageRef(record[16 : 16+PageRefSize]),
	}, true
}

func decodeFreeExtent(src []byte) FreeExtent {
	return FreeExtent{
		Offset:            binary.LittleEndian.Uint64(src[0:8]),
		Length:            binary.LittleEndian.Uint64(src[8:16]),
		RetiredGeneration: binary.LittleEndian.Uint64(src[16:24]),
	}
}

func validateFreeExtent(extent FreeExtent, pageSize uint32, fileEnd uint64) error {
	quantum := uint64(pageSize)
	if extent.Length == 0 || extent.Offset < uint64(superblockCopies)*quantum ||
		extent.Offset%quantum != 0 || extent.Length%quantum != 0 ||
		extent.Offset > maxSuperblockFileOffset || extent.Length > fileEnd || extent.Offset > fileEnd-extent.Length ||
		extent.RetiredGeneration == 0 {
		return fmt.Errorf("%w: invalid free extent", ErrInvalidWrite)
	}
	return nil
}

func validateFreeDirectoryHeader(header FreeDirectoryHeader, count, recordSize int, nextLogicalID uint64) error {
	if header.StoreID == ([16]byte{}) || header.Generation == 0 ||
		header.LogicalID <= StateRootLogicalID || header.LogicalID >= nextLogicalID ||
		!validPhysicalPageSize(header.PageSize) || header.Flags&^freeDirectoryKnownFlags != 0 ||
		count <= 0 || count > int(^uint16(0)) ||
		recordSize == FreeDirectoryBranchRecordSize && count > 64 {
		return fmt.Errorf("%w: free node identity, count, or flags", ErrInvalidWrite)
	}
	payloadLength := uint64(FreeDirectoryPayloadHeaderSize) + uint64(count)*uint64(recordSize)
	if payloadLength > uint64(header.PageSize)-PageHeaderSize-PageTrailerSize {
		return fmt.Errorf("%w: free-directory payload does not fit", ErrInvalidWrite)
	}
	return nil
}

func validateFreeDirectoryChild(header FreeDirectoryHeader, ref PageRef, fileEnd, nextLogicalID uint64) error {
	pageSize := uint64(header.PageSize)
	if fileEnd < uint64(superblockCopies)*pageSize || fileEnd > maxSuperblockFileOffset || fileEnd%pageSize != 0 ||
		ref.Kind != PageFreeDirectory || ref.Flags != 0 || ref.Aux != 0 || ref.Length != header.PageSize ||
		ref.Generation == 0 || ref.Generation > header.Generation ||
		ref.LogicalID <= StateRootLogicalID || ref.LogicalID >= nextLogicalID || ref.LogicalID == header.LogicalID ||
		ref.Offset < uint64(superblockCopies)*pageSize || ref.Offset%pageSize != 0 ||
		ref.Offset > maxSuperblockFileOffset || ref.Offset > fileEnd-pageSize {
		return fmt.Errorf("%w: invalid free-directory child", ErrInvalidWrite)
	}
	return nil
}
