package storeio

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	TTLDirectoryPayloadHeaderSize = 32
	TTLDirectoryLeafRecordSize    = 16
	TTLDirectoryBranchRecordSize  = 48
	ttlDirectoryVersion           = uint32(1)
	ttlDirectoryKnownFlags        = uint8(0)
)

// ErrTTLDirectoryCorrupt reports malformed durable expiry metadata.
var ErrTTLDirectoryCorrupt = errors.New("slopjson: corrupt Store TTL directory")

// TTLKey is the canonical total order of an expiry record. Deadline is Unix
// nanoseconds; chunk and slot disambiguate equal deadlines.
type TTLKey struct {
	Deadline int64
	Chunk    uint32
	Slot     uint8
}

// TTLDirectoryChild is one branch lower bound and child page.
type TTLDirectoryChild struct {
	Lower TTLKey
	Ref   PageRef
}

// TTLDirectoryHeader describes one immutable expiry B+tree node. Level zero
// contains TTLKey records; higher levels contain lower-bound/child pairs.
type TTLDirectoryHeader struct {
	StoreID    [16]byte
	Generation uint64
	LogicalID  uint64
	PageSize   uint32
	Level      uint8
	Flags      uint8
}

// TTLDirectoryView is one admitted, borrowed expiry node.
type TTLDirectoryView struct {
	header  TTLDirectoryHeader
	payload []byte
	count   uint16
}

// EncodeTTLDirectoryLeaf writes a strictly ordered expiry leaf.
func EncodeTTLDirectoryLeaf(dst []byte, header TTLDirectoryHeader, entries []TTLKey, nextLogicalID uint64, chunkHighWater uint32, chunkDocuments uint8) ([]byte, error) {
	if header.Level != 0 || chunkHighWater == 0 || chunkDocuments == 0 || chunkDocuments > 64 {
		return nil, fmt.Errorf("%w: TTL leaf level or bounds", ErrInvalidWrite)
	}
	for i, entry := range entries {
		if entry.Chunk >= chunkHighWater || entry.Slot >= chunkDocuments ||
			i != 0 && compareTTLKey(entries[i-1], entry) >= 0 {
			return nil, fmt.Errorf("%w: TTL order or location", ErrInvalidWrite)
		}
	}
	if err := validateTTLDirectoryHeader(header, len(entries), TTLDirectoryLeafRecordSize, nextLogicalID); err != nil {
		return nil, err
	}
	payloadLength := TTLDirectoryPayloadHeaderSize + len(entries)*TTLDirectoryLeafRecordSize
	payload, err := InitPage(dst, PageHeader{
		StoreID: header.StoreID, Generation: header.Generation, LogicalID: header.LogicalID,
		PageSize: header.PageSize, PayloadLength: uint32(payloadLength), Kind: PageTTLDirectory,
	})
	if err != nil {
		return nil, err
	}
	encodeTTLDirectoryHeader(payload, header, len(entries))
	for i, entry := range entries {
		encodeTTLKey(payload[TTLDirectoryPayloadHeaderSize+i*TTLDirectoryLeafRecordSize:], entry)
	}
	page := dst[:int(header.PageSize)]
	if _, err := sealInitializedPage(page); err != nil {
		return nil, err
	}
	return page, nil
}

// EncodeTTLDirectoryBranch writes a branch with at most 64 children.
func EncodeTTLDirectoryBranch(dst []byte, header TTLDirectoryHeader, children []TTLDirectoryChild, fileEnd, nextLogicalID uint64) ([]byte, error) {
	if header.Level == 0 || len(children) > 64 {
		return nil, fmt.Errorf("%w: TTL branch level or fanout", ErrInvalidWrite)
	}
	if err := validateTTLDirectoryHeader(header, len(children), TTLDirectoryBranchRecordSize, nextLogicalID); err != nil {
		return nil, err
	}
	var seen chunkDirectoryRefSet
	for i, child := range children {
		if i != 0 && compareTTLKey(children[i-1].Lower, child.Lower) >= 0 {
			return nil, fmt.Errorf("%w: TTL lower-bound order", ErrInvalidWrite)
		}
		if err := validateTTLDirectoryChild(header, child.Ref, fileEnd, nextLogicalID); err != nil || !seen.add(child.Ref) {
			return nil, fmt.Errorf("%w: TTL branch child", ErrInvalidWrite)
		}
	}
	payloadLength := TTLDirectoryPayloadHeaderSize + len(children)*TTLDirectoryBranchRecordSize
	payload, err := InitPage(dst, PageHeader{
		StoreID: header.StoreID, Generation: header.Generation, LogicalID: header.LogicalID,
		PageSize: header.PageSize, PayloadLength: uint32(payloadLength), Kind: PageTTLDirectory,
	})
	if err != nil {
		return nil, err
	}
	encodeTTLDirectoryHeader(payload, header, len(children))
	for i, child := range children {
		record := payload[TTLDirectoryPayloadHeaderSize+i*TTLDirectoryBranchRecordSize:]
		encodeTTLKey(record, child.Lower)
		encodePageRef(record[16:16+PageRefSize], child.Ref)
	}
	page := dst[:int(header.PageSize)]
	if _, err := sealInitializedPage(page); err != nil {
		return nil, err
	}
	return page, nil
}

func encodeTTLDirectoryHeader(payload []byte, header TTLDirectoryHeader, count int) {
	binary.LittleEndian.PutUint32(payload[0:4], ttlDirectoryVersion)
	payload[4] = header.Level
	payload[5] = header.Flags
	binary.LittleEndian.PutUint16(payload[6:8], uint16(count))
}

func encodeTTLKey(dst []byte, key TTLKey) {
	binary.LittleEndian.PutUint64(dst[0:8], uint64(key.Deadline))
	binary.LittleEndian.PutUint32(dst[8:12], key.Chunk)
	dst[12] = key.Slot
}

func decodeTTLKey(src []byte) TTLKey {
	return TTLKey{
		Deadline: int64(binary.LittleEndian.Uint64(src[0:8])),
		Chunk:    binary.LittleEndian.Uint32(src[8:12]), Slot: src[12],
	}
}

// OpenTTLDirectoryPage validates an expiry leaf or branch once.
func OpenTTLDirectoryPage(src []byte, fileEnd, nextLogicalID uint64, chunkHighWater uint32, chunkDocuments uint8) (TTLDirectoryView, error) {
	if chunkHighWater == 0 || chunkDocuments == 0 || chunkDocuments > 64 {
		return TTLDirectoryView{}, fmt.Errorf("%w: location bounds", ErrTTLDirectoryCorrupt)
	}
	pageHeader, payload, err := OpenPage(src)
	if err != nil {
		return TTLDirectoryView{}, fmt.Errorf("%w: %w", ErrTTLDirectoryCorrupt, err)
	}
	if pageHeader.Kind != PageTTLDirectory || len(payload) < TTLDirectoryPayloadHeaderSize ||
		binary.LittleEndian.Uint32(payload[0:4]) != ttlDirectoryVersion ||
		!allZero(payload[8:TTLDirectoryPayloadHeaderSize]) {
		return TTLDirectoryView{}, fmt.Errorf("%w: header, version, or reserved bytes", ErrTTLDirectoryCorrupt)
	}
	header := TTLDirectoryHeader{
		StoreID: pageHeader.StoreID, Generation: pageHeader.Generation,
		LogicalID: pageHeader.LogicalID, PageSize: pageHeader.PageSize,
		Level: payload[4], Flags: payload[5],
	}
	count := int(binary.LittleEndian.Uint16(payload[6:8]))
	recordSize := TTLDirectoryLeafRecordSize
	if header.Level != 0 {
		recordSize = TTLDirectoryBranchRecordSize
	}
	if err := validateTTLDirectoryHeader(header, count, recordSize, nextLogicalID); err != nil ||
		len(payload) != TTLDirectoryPayloadHeaderSize+count*recordSize {
		return TTLDirectoryView{}, fmt.Errorf("%w: node bounds", ErrTTLDirectoryCorrupt)
	}
	var previous TTLKey
	var seen chunkDirectoryRefSet
	for i := 0; i < count; i++ {
		record := payload[TTLDirectoryPayloadHeaderSize+i*recordSize:]
		entry := decodeTTLKey(record)
		if !allZero(record[13:16]) || i != 0 && compareTTLKey(previous, entry) >= 0 {
			return TTLDirectoryView{}, fmt.Errorf("%w: key order or reserved bytes", ErrTTLDirectoryCorrupt)
		}
		if header.Level == 0 {
			if entry.Chunk >= chunkHighWater || entry.Slot >= chunkDocuments {
				return TTLDirectoryView{}, fmt.Errorf("%w: leaf location", ErrTTLDirectoryCorrupt)
			}
		} else {
			if !pageRefReservedZero(record[16 : 16+PageRefSize]) {
				return TTLDirectoryView{}, fmt.Errorf("%w: child reserved bytes", ErrTTLDirectoryCorrupt)
			}
			ref := decodePageRef(record[16 : 16+PageRefSize])
			if err := validateTTLDirectoryChild(header, ref, fileEnd, nextLogicalID); err != nil || !seen.add(ref) {
				return TTLDirectoryView{}, fmt.Errorf("%w: branch child", ErrTTLDirectoryCorrupt)
			}
		}
		previous = entry
	}
	return TTLDirectoryView{header: header, payload: payload, count: uint16(count)}, nil
}

// Header returns value-only node metadata.
func (v TTLDirectoryView) Header() TTLDirectoryHeader { return v.header }

// Len returns the number of records or children.
func (v TTLDirectoryView) Len() int { return int(v.count) }

// EntryAt returns a leaf expiry record at rank.
func (v TTLDirectoryView) EntryAt(rank int) (TTLKey, bool) {
	if v.header.Level != 0 || rank < 0 || rank >= int(v.count) {
		return TTLKey{}, false
	}
	return decodeTTLKey(v.payload[TTLDirectoryPayloadHeaderSize+rank*TTLDirectoryLeafRecordSize:]), true
}

// LowerBound returns the first leaf rank whose key is not less than target.
func (v TTLDirectoryView) LowerBound(target TTLKey) int {
	if v.header.Level != 0 {
		return 0
	}
	low, high := 0, int(v.count)
	for low < high {
		middle := int(uint(low+high) >> 1)
		entry, _ := v.EntryAt(middle)
		if compareTTLKey(entry, target) < 0 {
			low = middle + 1
		} else {
			high = middle
		}
	}
	return low
}

// Child selects the branch with the greatest lower bound not exceeding key.
func (v TTLDirectoryView) Child(key TTLKey) (PageRef, bool) {
	if v.header.Level == 0 || v.count == 0 {
		return PageRef{}, false
	}
	low, high := 0, int(v.count)
	for low < high {
		middle := int(uint(low+high) >> 1)
		record := v.payload[TTLDirectoryPayloadHeaderSize+middle*TTLDirectoryBranchRecordSize:]
		if compareTTLKey(decodeTTLKey(record), key) <= 0 {
			low = middle + 1
		} else {
			high = middle
		}
	}
	if low == 0 {
		return PageRef{}, false
	}
	record := v.payload[TTLDirectoryPayloadHeaderSize+(low-1)*TTLDirectoryBranchRecordSize:]
	return decodePageRef(record[16 : 16+PageRefSize]), true
}

// ChildAt returns one branch lower bound and child at rank.
func (v TTLDirectoryView) ChildAt(rank int) (TTLDirectoryChild, bool) {
	if v.header.Level == 0 || rank < 0 || rank >= int(v.count) {
		return TTLDirectoryChild{}, false
	}
	record := v.payload[TTLDirectoryPayloadHeaderSize+rank*TTLDirectoryBranchRecordSize:]
	return TTLDirectoryChild{Lower: decodeTTLKey(record), Ref: decodePageRef(record[16 : 16+PageRefSize])}, true
}

func compareTTLKey(a, b TTLKey) int {
	if a.Deadline < b.Deadline {
		return -1
	}
	if a.Deadline > b.Deadline {
		return 1
	}
	if a.Chunk < b.Chunk {
		return -1
	}
	if a.Chunk > b.Chunk {
		return 1
	}
	if a.Slot < b.Slot {
		return -1
	}
	if a.Slot > b.Slot {
		return 1
	}
	return 0
}

func validateTTLDirectoryHeader(header TTLDirectoryHeader, count, recordSize int, nextLogicalID uint64) error {
	if header.StoreID == ([16]byte{}) || header.Generation == 0 ||
		header.LogicalID <= StateRootLogicalID || header.LogicalID >= nextLogicalID ||
		!validPhysicalPageSize(header.PageSize) || header.Flags&^ttlDirectoryKnownFlags != 0 ||
		count <= 0 || count > int(^uint16(0)) ||
		recordSize == TTLDirectoryBranchRecordSize && count > 64 {
		return fmt.Errorf("%w: TTL node identity, count, or flags", ErrInvalidWrite)
	}
	payloadLength := uint64(TTLDirectoryPayloadHeaderSize) + uint64(count)*uint64(recordSize)
	if payloadLength > uint64(header.PageSize)-PageHeaderSize-PageTrailerSize {
		return fmt.Errorf("%w: TTL payload does not fit", ErrInvalidWrite)
	}
	return nil
}

func validateTTLDirectoryChild(header TTLDirectoryHeader, ref PageRef, fileEnd, nextLogicalID uint64) error {
	pageSize := uint64(header.PageSize)
	if fileEnd < uint64(superblockCopies)*pageSize || fileEnd > maxSuperblockFileOffset || fileEnd%pageSize != 0 ||
		ref.Kind != PageTTLDirectory || ref.Flags != 0 || ref.Aux != 0 || ref.Length != header.PageSize ||
		ref.Generation == 0 || ref.Generation > header.Generation ||
		ref.LogicalID <= StateRootLogicalID || ref.LogicalID >= nextLogicalID || ref.LogicalID == header.LogicalID ||
		ref.Offset < uint64(superblockCopies)*pageSize || ref.Offset%pageSize != 0 ||
		ref.Offset > maxSuperblockFileOffset || ref.Offset > fileEnd-pageSize {
		return fmt.Errorf("%w: invalid TTL-directory child", ErrInvalidWrite)
	}
	return nil
}
