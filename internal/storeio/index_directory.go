package storeio

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	IndexDirectoryPayloadHeaderSize = 32
	IndexDirectoryLeafRecordSize    = 56
	IndexDirectoryBranchRecordSize  = 48
	indexDirectoryVersion           = uint32(1)
	indexDirectoryKnownFlags        = uint8(0)
	indexPostingRefKnownFlags       = IndexPostingImmutableBase
)

// ErrIndexDirectoryCorrupt reports malformed durable secondary-index routing
// metadata. Tuple hashes only select candidates; Store query execution still
// performs its ordinary exact scalar recheck.
var ErrIndexDirectoryCorrupt = errors.New("simdjson: corrupt Store index directory")

// IndexDirectoryKey is the canonical routing order for one exact tuple stream.
type IndexDirectoryKey struct {
	IndexID   uint32
	TupleHash uint64
	Chunk     uint32
}

// IndexPostingRef selects one segment in an immutable posting page.
type IndexPostingRef struct {
	Page    PageRef
	Segment uint16
	Flags   uint16
}

const (
	// IndexPostingImmutableBase marks a posting page shared by several compact
	// directory entries. Online mutation redirects the changed entry to an
	// isolated delta page but retains the immutable base extent. Base space is
	// therefore bounded by the last bulk generation instead of becoming
	// untracked or requiring page-level reference counts.
	IndexPostingImmutableBase uint16 = 1 << iota
)

// IndexDirectoryEntry is one leaf mapping.
type IndexDirectoryEntry struct {
	Key     IndexDirectoryKey
	Posting IndexPostingRef
}

// IndexDirectoryChild is one branch lower bound and child page.
type IndexDirectoryChild struct {
	Lower IndexDirectoryKey
	Ref   PageRef
}

// IndexDirectoryHeader describes one immutable B+tree node.
type IndexDirectoryHeader struct {
	StoreID    [16]byte
	Generation uint64
	LogicalID  uint64
	PageSize   uint32
	Level      uint8
	Flags      uint8
}

// IndexDirectoryView is one checksum-verified borrowed node.
type IndexDirectoryView struct {
	header  IndexDirectoryHeader
	payload []byte
	count   uint16
}

// EncodeIndexDirectoryLeaf writes a strictly ordered tuple-routing leaf.
func EncodeIndexDirectoryLeaf(dst []byte, header IndexDirectoryHeader, entries []IndexDirectoryEntry, fileEnd, nextLogicalID uint64, indexHighWater uint32) ([]byte, error) {
	if header.Level != 0 || indexHighWater == 0 {
		return nil, fmt.Errorf("%w: index leaf level or bounds", ErrInvalidWrite)
	}
	if err := validateIndexDirectoryHeader(header, len(entries), IndexDirectoryLeafRecordSize, nextLogicalID); err != nil {
		return nil, err
	}
	for i, entry := range entries {
		if entry.Key.IndexID >= indexHighWater || i != 0 && compareIndexDirectoryKey(entries[i-1].Key, entry.Key) >= 0 ||
			entry.Posting.Flags&^indexPostingRefKnownFlags != 0 {
			return nil, fmt.Errorf("%w: index leaf order, id, or flags", ErrInvalidWrite)
		}
		if err := validateIndexPostingPageRef(header, entry.Posting.Page, fileEnd, nextLogicalID); err != nil {
			return nil, fmt.Errorf("%w: index posting reference", ErrInvalidWrite)
		}
	}
	payloadLength := IndexDirectoryPayloadHeaderSize + len(entries)*IndexDirectoryLeafRecordSize
	payload, err := InitPage(dst, PageHeader{
		StoreID: header.StoreID, Generation: header.Generation, LogicalID: header.LogicalID,
		PageSize: header.PageSize, PayloadLength: uint32(payloadLength), Kind: PageIndexDirectory,
	})
	if err != nil {
		return nil, err
	}
	encodeIndexDirectoryHeader(payload, header, len(entries))
	for i, entry := range entries {
		record := payload[IndexDirectoryPayloadHeaderSize+i*IndexDirectoryLeafRecordSize:]
		encodeIndexDirectoryKey(record, entry.Key)
		encodePageRef(record[16:16+PageRefSize], entry.Posting.Page)
		binary.LittleEndian.PutUint16(record[48:50], entry.Posting.Segment)
		binary.LittleEndian.PutUint16(record[50:52], entry.Posting.Flags)
	}
	page := dst[:int(header.PageSize)]
	if _, err := sealInitializedPage(page); err != nil {
		return nil, err
	}
	return page, nil
}

// EncodeIndexDirectoryBranch writes a branch with at most 64 children.
func EncodeIndexDirectoryBranch(dst []byte, header IndexDirectoryHeader, children []IndexDirectoryChild, fileEnd, nextLogicalID uint64) ([]byte, error) {
	if header.Level == 0 || len(children) > 64 {
		return nil, fmt.Errorf("%w: index branch level or fanout", ErrInvalidWrite)
	}
	if err := validateIndexDirectoryHeader(header, len(children), IndexDirectoryBranchRecordSize, nextLogicalID); err != nil {
		return nil, err
	}
	var seen chunkDirectoryRefSet
	for i, child := range children {
		if i != 0 && compareIndexDirectoryKey(children[i-1].Lower, child.Lower) >= 0 {
			return nil, fmt.Errorf("%w: index lower-bound order", ErrInvalidWrite)
		}
		if err := validateIndexDirectoryChild(header, child.Ref, fileEnd, nextLogicalID); err != nil || !seen.add(child.Ref) {
			return nil, fmt.Errorf("%w: index branch child", ErrInvalidWrite)
		}
	}
	payloadLength := IndexDirectoryPayloadHeaderSize + len(children)*IndexDirectoryBranchRecordSize
	payload, err := InitPage(dst, PageHeader{
		StoreID: header.StoreID, Generation: header.Generation, LogicalID: header.LogicalID,
		PageSize: header.PageSize, PayloadLength: uint32(payloadLength), Kind: PageIndexDirectory,
	})
	if err != nil {
		return nil, err
	}
	encodeIndexDirectoryHeader(payload, header, len(children))
	for i, child := range children {
		record := payload[IndexDirectoryPayloadHeaderSize+i*IndexDirectoryBranchRecordSize:]
		encodeIndexDirectoryKey(record, child.Lower)
		encodePageRef(record[16:16+PageRefSize], child.Ref)
	}
	page := dst[:int(header.PageSize)]
	if _, err := sealInitializedPage(page); err != nil {
		return nil, err
	}
	return page, nil
}

func encodeIndexDirectoryHeader(payload []byte, header IndexDirectoryHeader, count int) {
	binary.LittleEndian.PutUint32(payload[0:4], indexDirectoryVersion)
	payload[4] = header.Level
	payload[5] = header.Flags
	binary.LittleEndian.PutUint16(payload[6:8], uint16(count))
}

func encodeIndexDirectoryKey(dst []byte, key IndexDirectoryKey) {
	binary.LittleEndian.PutUint32(dst[0:4], key.IndexID)
	binary.LittleEndian.PutUint32(dst[4:8], key.Chunk)
	binary.LittleEndian.PutUint64(dst[8:16], key.TupleHash)
}

func decodeIndexDirectoryKey(src []byte) IndexDirectoryKey {
	return IndexDirectoryKey{
		IndexID:   binary.LittleEndian.Uint32(src[0:4]),
		TupleHash: binary.LittleEndian.Uint64(src[8:16]),
		Chunk:     binary.LittleEndian.Uint32(src[4:8]),
	}
}

// OpenIndexDirectoryPage validates one tuple-routing leaf or branch.
func OpenIndexDirectoryPage(src []byte, fileEnd, nextLogicalID uint64, indexHighWater uint32) (IndexDirectoryView, error) {
	if indexHighWater == 0 {
		return IndexDirectoryView{}, fmt.Errorf("%w: index bounds", ErrIndexDirectoryCorrupt)
	}
	pageHeader, payload, err := OpenPage(src)
	if err != nil {
		return IndexDirectoryView{}, fmt.Errorf("%w: %w", ErrIndexDirectoryCorrupt, err)
	}
	if pageHeader.Kind != PageIndexDirectory || len(payload) < IndexDirectoryPayloadHeaderSize ||
		binary.LittleEndian.Uint32(payload[0:4]) != indexDirectoryVersion ||
		!allZero(payload[8:IndexDirectoryPayloadHeaderSize]) {
		return IndexDirectoryView{}, fmt.Errorf("%w: header, version, or reserved bytes", ErrIndexDirectoryCorrupt)
	}
	header := IndexDirectoryHeader{
		StoreID: pageHeader.StoreID, Generation: pageHeader.Generation,
		LogicalID: pageHeader.LogicalID, PageSize: pageHeader.PageSize,
		Level: payload[4], Flags: payload[5],
	}
	count := int(binary.LittleEndian.Uint16(payload[6:8]))
	recordSize := IndexDirectoryLeafRecordSize
	if header.Level != 0 {
		recordSize = IndexDirectoryBranchRecordSize
	}
	if err := validateIndexDirectoryHeader(header, count, recordSize, nextLogicalID); err != nil ||
		len(payload) != IndexDirectoryPayloadHeaderSize+count*recordSize {
		return IndexDirectoryView{}, fmt.Errorf("%w: node bounds", ErrIndexDirectoryCorrupt)
	}
	var previous IndexDirectoryKey
	var seen chunkDirectoryRefSet
	for i := 0; i < count; i++ {
		record := payload[IndexDirectoryPayloadHeaderSize+i*recordSize:]
		key := decodeIndexDirectoryKey(record)
		if key.IndexID >= indexHighWater ||
			i != 0 && compareIndexDirectoryKey(previous, key) >= 0 {
			return IndexDirectoryView{}, fmt.Errorf("%w: key order, id, or reserved bytes", ErrIndexDirectoryCorrupt)
		}
		if header.Level == 0 {
			if !pageRefReservedZero(record[16:16+PageRefSize]) || !allZero(record[52:56]) ||
				binary.LittleEndian.Uint16(record[50:52])&^indexPostingRefKnownFlags != 0 {
				return IndexDirectoryView{}, fmt.Errorf("%w: posting reserved bytes or flags", ErrIndexDirectoryCorrupt)
			}
			ref := decodePageRef(record[16 : 16+PageRefSize])
			if err := validateIndexPostingPageRef(header, ref, fileEnd, nextLogicalID); err != nil {
				return IndexDirectoryView{}, fmt.Errorf("%w: posting page reference", ErrIndexDirectoryCorrupt)
			}
		} else {
			if !pageRefReservedZero(record[16 : 16+PageRefSize]) {
				return IndexDirectoryView{}, fmt.Errorf("%w: child reserved bytes", ErrIndexDirectoryCorrupt)
			}
			ref := decodePageRef(record[16 : 16+PageRefSize])
			if err := validateIndexDirectoryChild(header, ref, fileEnd, nextLogicalID); err != nil || !seen.add(ref) {
				return IndexDirectoryView{}, fmt.Errorf("%w: branch child", ErrIndexDirectoryCorrupt)
			}
		}
		previous = key
	}
	return IndexDirectoryView{header: header, payload: payload, count: uint16(count)}, nil
}

// Header returns value-only node metadata.
func (v IndexDirectoryView) Header() IndexDirectoryHeader { return v.header }

// Len returns the number of entries or children.
func (v IndexDirectoryView) Len() int { return int(v.count) }

// Lookup resolves one exact routing key in a leaf.
func (v IndexDirectoryView) Lookup(key IndexDirectoryKey) (IndexPostingRef, bool) {
	if v.header.Level != 0 {
		return IndexPostingRef{}, false
	}
	low, high := 0, int(v.count)
	for low < high {
		middle := int(uint(low+high) >> 1)
		record := v.payload[IndexDirectoryPayloadHeaderSize+middle*IndexDirectoryLeafRecordSize:]
		if compareIndexDirectoryKey(decodeIndexDirectoryKey(record), key) < 0 {
			low = middle + 1
		} else {
			high = middle
		}
	}
	if low >= int(v.count) {
		return IndexPostingRef{}, false
	}
	record := v.payload[IndexDirectoryPayloadHeaderSize+low*IndexDirectoryLeafRecordSize:]
	if compareIndexDirectoryKey(decodeIndexDirectoryKey(record), key) != 0 {
		return IndexPostingRef{}, false
	}
	return IndexPostingRef{
		Page:    decodePageRef(record[16 : 16+PageRefSize]),
		Segment: binary.LittleEndian.Uint16(record[48:50]),
		Flags:   binary.LittleEndian.Uint16(record[50:52]),
	}, true
}

// EntryAt returns one leaf mapping at rank.
func (v IndexDirectoryView) EntryAt(rank int) (IndexDirectoryEntry, bool) {
	if v.header.Level != 0 || rank < 0 || rank >= int(v.count) {
		return IndexDirectoryEntry{}, false
	}
	record := v.payload[IndexDirectoryPayloadHeaderSize+rank*IndexDirectoryLeafRecordSize:]
	return IndexDirectoryEntry{
		Key: decodeIndexDirectoryKey(record),
		Posting: IndexPostingRef{
			Page:    decodePageRef(record[16 : 16+PageRefSize]),
			Segment: binary.LittleEndian.Uint16(record[48:50]),
			Flags:   binary.LittleEndian.Uint16(record[50:52]),
		},
	}, true
}

// Child selects the branch with the greatest lower bound not exceeding key.
func (v IndexDirectoryView) Child(key IndexDirectoryKey) (PageRef, bool) {
	if v.header.Level == 0 || v.count == 0 {
		return PageRef{}, false
	}
	low, high := 0, int(v.count)
	for low < high {
		middle := int(uint(low+high) >> 1)
		record := v.payload[IndexDirectoryPayloadHeaderSize+middle*IndexDirectoryBranchRecordSize:]
		if compareIndexDirectoryKey(decodeIndexDirectoryKey(record), key) <= 0 {
			low = middle + 1
		} else {
			high = middle
		}
	}
	if low == 0 {
		return PageRef{}, false
	}
	record := v.payload[IndexDirectoryPayloadHeaderSize+(low-1)*IndexDirectoryBranchRecordSize:]
	return decodePageRef(record[16 : 16+PageRefSize]), true
}

// ChildAt returns one branch lower bound and child at rank.
func (v IndexDirectoryView) ChildAt(rank int) (IndexDirectoryChild, bool) {
	if v.header.Level == 0 || rank < 0 || rank >= int(v.count) {
		return IndexDirectoryChild{}, false
	}
	record := v.payload[IndexDirectoryPayloadHeaderSize+rank*IndexDirectoryBranchRecordSize:]
	return IndexDirectoryChild{Lower: decodeIndexDirectoryKey(record), Ref: decodePageRef(record[16 : 16+PageRefSize])}, true
}

func compareIndexDirectoryKey(a, b IndexDirectoryKey) int {
	if a.IndexID < b.IndexID {
		return -1
	}
	if a.IndexID > b.IndexID {
		return 1
	}
	if a.TupleHash < b.TupleHash {
		return -1
	}
	if a.TupleHash > b.TupleHash {
		return 1
	}
	if a.Chunk < b.Chunk {
		return -1
	}
	if a.Chunk > b.Chunk {
		return 1
	}
	return 0
}

func validateIndexDirectoryHeader(header IndexDirectoryHeader, count, recordSize int, nextLogicalID uint64) error {
	if header.StoreID == ([16]byte{}) || header.Generation == 0 ||
		header.LogicalID <= StateRootLogicalID || header.LogicalID >= nextLogicalID ||
		!validPhysicalPageSize(header.PageSize) || header.Flags&^indexDirectoryKnownFlags != 0 ||
		count <= 0 || count > int(^uint16(0)) ||
		recordSize == IndexDirectoryBranchRecordSize && count > 64 {
		return fmt.Errorf("%w: index node identity, count, or flags", ErrInvalidWrite)
	}
	payloadLength := uint64(IndexDirectoryPayloadHeaderSize) + uint64(count)*uint64(recordSize)
	if payloadLength > uint64(header.PageSize)-PageHeaderSize-PageTrailerSize {
		return fmt.Errorf("%w: index-directory payload does not fit", ErrInvalidWrite)
	}
	return nil
}

func validateIndexDirectoryChild(header IndexDirectoryHeader, ref PageRef, fileEnd, nextLogicalID uint64) error {
	return validateIndexPageRef(header, ref, PageIndexDirectory, fileEnd, nextLogicalID)
}

func validateIndexPostingPageRef(header IndexDirectoryHeader, ref PageRef, fileEnd, nextLogicalID uint64) error {
	return validateIndexPageRef(header, ref, PageIndexPosting, fileEnd, nextLogicalID)
}

func validateIndexPageRef(header IndexDirectoryHeader, ref PageRef, kind PageKind, fileEnd, nextLogicalID uint64) error {
	pageSize := uint64(header.PageSize)
	if fileEnd < uint64(superblockCopies)*pageSize || fileEnd > maxSuperblockFileOffset || fileEnd%pageSize != 0 ||
		ref.Kind != kind || ref.Flags != 0 || ref.Aux != 0 || ref.Length != header.PageSize ||
		ref.Generation == 0 || ref.Generation > header.Generation ||
		ref.LogicalID <= StateRootLogicalID || ref.LogicalID >= nextLogicalID || ref.LogicalID == header.LogicalID ||
		ref.Offset < uint64(superblockCopies)*pageSize || ref.Offset%pageSize != 0 ||
		ref.Offset > maxSuperblockFileOffset || ref.Offset > fileEnd-pageSize {
		return fmt.Errorf("%w: invalid index page reference", ErrInvalidWrite)
	}
	return nil
}
