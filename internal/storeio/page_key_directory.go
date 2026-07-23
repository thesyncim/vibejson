package storeio

import (
	"encoding/binary"
	"fmt"
	"math/bits"
)

const (
	PageKeyDirectoryPayloadHeaderSize = 64
	PageKeyLeafEntrySize              = 16
	PageKeyBranchEntrySize            = 40
	pageKeyDirectoryVersion           = uint32(1)
	pageKeyDirectoryKnownFlags        = uint8(0)
	pageKeyDirectoryMaxLevel          = uint8(15)
)

// PageKeyDirectoryHeader describes one sorted immutable B+tree page. Level zero
// is a leaf; larger levels contain child upper bounds. MinHash and MaxHash are
// inclusive. A leaf Next reference preserves complete collision enumeration
// when one 64-bit hash spans a physical-page boundary.
type PageKeyDirectoryHeader struct {
	StoreID    [16]byte
	Generation uint64
	LogicalID  uint64
	PageSize   uint32
	MinHash    uint64
	MaxHash    uint64
	Level      uint8
	Flags      uint8
	Next       PageRef
}

// PageKeyLocation is one collision-pruning leaf entry. Hash is never
// authoritative: readers must compare the complete key in the document page.
type PageKeyLocation struct {
	Hash  uint64
	Chunk uint32
	Slot  uint8
}

// PageKeyBranch routes hashes up to and including MaxHash to Child.
type PageKeyBranch struct {
	MaxHash uint64
	Child   PageRef
}

// PageKeyDirectoryView is a verified borrowed page. It retains one payload slice
// regardless of its entry count.
type PageKeyDirectoryView struct {
	header  PageKeyDirectoryHeader
	payload []byte
	count   uint16
}

// EncodePageKeyLeaf writes a sorted location leaf. Equal hashes are allowed
// and remain ordered by chunk then slot. next is zero for the last leaf.
func EncodePageKeyLeaf(dst []byte, header PageKeyDirectoryHeader, entries []PageKeyLocation, fileEnd, nextLogicalID uint64, chunkHighWater, chunkDocuments uint32) ([]byte, error) {
	header.Level = 0
	if err := validatePageKeyLeafWrite(header, entries, fileEnd, nextLogicalID, chunkHighWater, chunkDocuments); err != nil {
		return nil, err
	}
	payload, err := initPageKeyDirectory(dst, header, len(entries), PageKeyLeafEntrySize)
	if err != nil {
		return nil, err
	}
	for i, entry := range entries {
		start := PageKeyDirectoryPayloadHeaderSize + i*PageKeyLeafEntrySize
		binary.LittleEndian.PutUint64(payload[start:start+8], entry.Hash)
		binary.LittleEndian.PutUint32(payload[start+8:start+12], entry.Chunk)
		payload[start+12] = entry.Slot
	}
	page := dst[:int(header.PageSize)]
	if _, err := sealInitializedPage(page); err != nil {
		return nil, err
	}
	return page, nil
}

// EncodePageKeyBranch writes a sorted internal page. The first branch whose
// MaxHash covers a query is selected. Equal upper bounds are valid when one
// adversarial collision run spans physical pages; the leaf chain remains the
// authoritative enumeration order.
func EncodePageKeyBranch(dst []byte, header PageKeyDirectoryHeader, entries []PageKeyBranch, fileEnd, nextLogicalID uint64) ([]byte, error) {
	if header.Level == 0 {
		return nil, fmt.Errorf("%w: branch level is zero", ErrInvalidWrite)
	}
	header.Next = PageRef{}
	if err := validatePageKeyBranchWrite(header, entries, fileEnd, nextLogicalID); err != nil {
		return nil, err
	}
	payload, err := initPageKeyDirectory(dst, header, len(entries), PageKeyBranchEntrySize)
	if err != nil {
		return nil, err
	}
	for i, entry := range entries {
		start := PageKeyDirectoryPayloadHeaderSize + i*PageKeyBranchEntrySize
		binary.LittleEndian.PutUint64(payload[start:start+8], entry.MaxHash)
		encodePageRef(payload[start+8:start+PageKeyBranchEntrySize], entry.Child)
	}
	page := dst[:int(header.PageSize)]
	if _, err := sealInitializedPage(page); err != nil {
		return nil, err
	}
	return page, nil
}

func initPageKeyDirectory(dst []byte, header PageKeyDirectoryHeader, count, entrySize int) ([]byte, error) {
	payloadLength := PageKeyDirectoryPayloadHeaderSize + count*entrySize
	payload, err := InitPage(dst, PageHeader{
		StoreID:       header.StoreID,
		Generation:    header.Generation,
		LogicalID:     header.LogicalID,
		PageSize:      header.PageSize,
		PayloadLength: uint32(payloadLength),
		Kind:          PageKeyDirectory,
	})
	if err != nil {
		return nil, err
	}
	binary.LittleEndian.PutUint32(payload[0:4], pageKeyDirectoryVersion)
	payload[4] = header.Level
	payload[5] = header.Flags
	binary.LittleEndian.PutUint16(payload[6:8], uint16(count))
	binary.LittleEndian.PutUint64(payload[8:16], header.MinHash)
	binary.LittleEndian.PutUint64(payload[16:24], header.MaxHash)
	binary.LittleEndian.PutUint16(payload[24:26], uint16(entrySize))
	encodePageRef(payload[32:64], header.Next)
	return payload, nil
}

// OpenPageKeyDirectory verifies one complete key B+tree page. Repeated branch
// and leaf probes then allocate nothing and perform no checksum work.
func OpenPageKeyDirectory(src []byte, fileEnd, nextLogicalID uint64, chunkHighWater, chunkDocuments uint32) (PageKeyDirectoryView, error) {
	pageHeader, payload, err := OpenPage(src)
	if err != nil {
		return PageKeyDirectoryView{}, fmt.Errorf("%w: %w", ErrKeyDirectoryCorrupt, err)
	}
	if pageHeader.Kind != PageKeyDirectory || len(payload) < PageKeyDirectoryPayloadHeaderSize ||
		binary.LittleEndian.Uint32(payload[0:4]) != pageKeyDirectoryVersion ||
		!allZero(payload[26:32]) || !pageRefReservedZero(payload[32:64]) {
		return PageKeyDirectoryView{}, fmt.Errorf("%w: header, version, or reserved bytes", ErrKeyDirectoryCorrupt)
	}
	count := int(binary.LittleEndian.Uint16(payload[6:8]))
	header := PageKeyDirectoryHeader{
		StoreID:    pageHeader.StoreID,
		Generation: pageHeader.Generation,
		LogicalID:  pageHeader.LogicalID,
		PageSize:   pageHeader.PageSize,
		MinHash:    binary.LittleEndian.Uint64(payload[8:16]),
		MaxHash:    binary.LittleEndian.Uint64(payload[16:24]),
		Level:      payload[4],
		Flags:      payload[5],
		Next:       decodePageRef(payload[32:64]),
	}
	entrySize := PageKeyLeafEntrySize
	if header.Level != 0 {
		entrySize = PageKeyBranchEntrySize
	}
	if int(binary.LittleEndian.Uint16(payload[24:26])) != entrySize ||
		len(payload) != PageKeyDirectoryPayloadHeaderSize+count*entrySize {
		return PageKeyDirectoryView{}, fmt.Errorf("%w: payload length or entry size", ErrKeyDirectoryCorrupt)
	}
	if err := validatePageKeyDirectoryHeader(header, count, fileEnd, nextLogicalID); err != nil {
		return PageKeyDirectoryView{}, fmt.Errorf("%w: %v", ErrKeyDirectoryCorrupt, err)
	}
	if header.Level == 0 {
		if err := validateEncodedPageKeyLeaf(payload, count, header, fileEnd, nextLogicalID, chunkHighWater, chunkDocuments); err != nil {
			return PageKeyDirectoryView{}, fmt.Errorf("%w: %v", ErrKeyDirectoryCorrupt, err)
		}
	} else if err := validateEncodedPageKeyBranch(payload, count, header, fileEnd, nextLogicalID); err != nil {
		return PageKeyDirectoryView{}, fmt.Errorf("%w: %v", ErrKeyDirectoryCorrupt, err)
	}
	return PageKeyDirectoryView{header: header, payload: payload, count: uint16(count)}, nil
}

// AdmittedPageKeyDirectory reconstructs a view of a page already validated by
// PageCache admission. Calling it on arbitrary bytes is invalid. Resident
// probes therefore do not repeat CRC or scan every packed entry.
func AdmittedPageKeyDirectory(src []byte) PageKeyDirectoryView {
	pageHeader, _ := decodePageHeader(src)
	payloadEnd := PageHeaderSize + int(pageHeader.PayloadLength)
	payload := src[PageHeaderSize:payloadEnd:payloadEnd]
	return PageKeyDirectoryView{
		header: PageKeyDirectoryHeader{
			StoreID: pageHeader.StoreID, Generation: pageHeader.Generation,
			LogicalID: pageHeader.LogicalID, PageSize: pageHeader.PageSize,
			MinHash: binary.LittleEndian.Uint64(payload[8:16]),
			MaxHash: binary.LittleEndian.Uint64(payload[16:24]), Level: payload[4],
			Flags: payload[5], Next: decodePageRef(payload[32:64]),
		},
		payload: payload, count: binary.LittleEndian.Uint16(payload[6:8]),
	}
}

// Header returns value-only page metadata.
func (v PageKeyDirectoryView) Header() PageKeyDirectoryHeader { return v.header }

// Len returns the number of leaf locations or branch children.
func (v PageKeyDirectoryView) Len() int { return int(v.count) }

// CandidateRange returns the contiguous leaf interval with hash. Every
// candidate still requires a full-key comparison in its document page.
func (v PageKeyDirectoryView) CandidateRange(hash uint64) (first, end int, ok bool) {
	if v.header.Level != 0 || hash < v.header.MinHash || hash > v.header.MaxHash {
		return 0, 0, false
	}
	lo, hi := 0, int(v.count)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if v.leafHash(mid) < hash {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	first = lo
	for lo < int(v.count) && v.leafHash(lo) == hash {
		lo++
	}
	return first, lo, first != lo
}

// LocationAt returns one leaf candidate by packed rank.
func (v PageKeyDirectoryView) LocationAt(rank int) (PageKeyLocation, bool) {
	if v.header.Level != 0 || rank < 0 || rank >= int(v.count) {
		return PageKeyLocation{}, false
	}
	start := PageKeyDirectoryPayloadHeaderSize + rank*PageKeyLeafEntrySize
	return PageKeyLocation{
		Hash:  binary.LittleEndian.Uint64(v.payload[start : start+8]),
		Chunk: binary.LittleEndian.Uint32(v.payload[start+8 : start+12]),
		Slot:  v.payload[start+12],
	}, true
}

// BranchAt returns one internal upper bound and child by packed rank. It is
// used by copy-on-write path reconstruction; ordinary point reads should use
// Child's binary search.
func (v PageKeyDirectoryView) BranchAt(rank int) (PageKeyBranch, bool) {
	if v.header.Level == 0 || rank < 0 || rank >= int(v.count) {
		return PageKeyBranch{}, false
	}
	start := PageKeyDirectoryPayloadHeaderSize + rank*PageKeyBranchEntrySize
	return PageKeyBranch{
		MaxHash: binary.LittleEndian.Uint64(v.payload[start : start+8]),
		Child:   decodePageRef(v.payload[start+8 : start+PageKeyBranchEntrySize]),
	}, true
}

// ChildIndex returns the child selected by hash and its packed rank. The rank
// lets copy-on-write consumers derive a collision successor from the current
// immutable parent path instead of trusting an older leaf's physical Next
// hint.
func (v PageKeyDirectoryView) ChildIndex(hash uint64) (PageRef, int, bool) {
	if v.header.Level == 0 || hash < v.header.MinHash || hash > v.header.MaxHash {
		return PageRef{}, 0, false
	}
	lo, hi := 0, int(v.count)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if v.branchMax(mid) < hash {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == int(v.count) {
		return PageRef{}, 0, false
	}
	start := PageKeyDirectoryPayloadHeaderSize + lo*PageKeyBranchEntrySize
	return decodePageRef(v.payload[start+8 : start+PageKeyBranchEntrySize]), lo, true
}

// Child returns the unique internal child whose upper bound covers hash.
func (v PageKeyDirectoryView) Child(hash uint64) (PageRef, bool) {
	if v.header.Level == 0 || hash < v.header.MinHash || hash > v.header.MaxHash {
		return PageRef{}, false
	}
	lo, hi := 0, int(v.count)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if v.branchMax(mid) < hash {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == int(v.count) {
		return PageRef{}, false
	}
	start := PageKeyDirectoryPayloadHeaderSize + lo*PageKeyBranchEntrySize
	return decodePageRef(v.payload[start+8 : start+PageKeyBranchEntrySize]), true
}

func (v PageKeyDirectoryView) leafHash(rank int) uint64 {
	start := PageKeyDirectoryPayloadHeaderSize + rank*PageKeyLeafEntrySize
	return binary.LittleEndian.Uint64(v.payload[start : start+8])
}

func (v PageKeyDirectoryView) branchMax(rank int) uint64 {
	start := PageKeyDirectoryPayloadHeaderSize + rank*PageKeyBranchEntrySize
	return binary.LittleEndian.Uint64(v.payload[start : start+8])
}

func validatePageKeyLeafWrite(header PageKeyDirectoryHeader, entries []PageKeyLocation, fileEnd, nextLogicalID uint64, chunkHighWater, chunkDocuments uint32) error {
	if len(entries) == 0 {
		return fmt.Errorf("%w: empty key leaf", ErrInvalidWrite)
	}
	if header.MinHash != entries[0].Hash || header.MaxHash != entries[len(entries)-1].Hash {
		return fmt.Errorf("%w: key leaf range", ErrInvalidWrite)
	}
	if err := validatePageKeyDirectoryHeader(header, len(entries), fileEnd, nextLogicalID); err != nil {
		return err
	}
	var previous PageKeyLocation
	for i, entry := range entries {
		if entry.Chunk >= chunkHighWater || uint32(entry.Slot) >= chunkDocuments ||
			i != 0 && (entry.Hash < previous.Hash || entry.Hash == previous.Hash &&
				(entry.Chunk < previous.Chunk || entry.Chunk == previous.Chunk && entry.Slot <= previous.Slot)) {
			return fmt.Errorf("%w: unsorted or invalid key leaf", ErrInvalidWrite)
		}
		previous = entry
	}
	return nil
}

func validatePageKeyBranchWrite(header PageKeyDirectoryHeader, entries []PageKeyBranch, fileEnd, nextLogicalID uint64) error {
	if len(entries) == 0 {
		return fmt.Errorf("%w: empty key branch", ErrInvalidWrite)
	}
	if header.MaxHash != entries[len(entries)-1].MaxHash || header.MinHash > entries[0].MaxHash {
		return fmt.Errorf("%w: key branch range", ErrInvalidWrite)
	}
	if err := validatePageKeyDirectoryHeader(header, len(entries), fileEnd, nextLogicalID); err != nil {
		return err
	}
	var previous uint64
	for i, entry := range entries {
		if i != 0 && entry.MaxHash < previous {
			return fmt.Errorf("%w: unsorted key branch", ErrInvalidWrite)
		}
		if err := validatePageKeyChildRef(header, entry.Child, fileEnd, nextLogicalID); err != nil {
			return err
		}
		previous = entry.MaxHash
	}
	return nil
}

func validatePageKeyDirectoryHeader(header PageKeyDirectoryHeader, count int, fileEnd, nextLogicalID uint64) error {
	entrySize := PageKeyLeafEntrySize
	if header.Level != 0 {
		entrySize = PageKeyBranchEntrySize
	}
	if header.StoreID == ([16]byte{}) || header.Generation == 0 ||
		header.LogicalID <= StateRootLogicalID || header.LogicalID >= nextLogicalID ||
		!validPhysicalPageSize(header.PageSize) || header.Level > pageKeyDirectoryMaxLevel ||
		header.Flags&^pageKeyDirectoryKnownFlags != 0 || count <= 0 || count > int(^uint16(0)) ||
		PageKeyDirectoryPayloadHeaderSize+count*entrySize > int(header.PageSize)-PageHeaderSize-PageTrailerSize ||
		header.MinHash > header.MaxHash {
		return fmt.Errorf("%w: key directory identity, range, or count", ErrInvalidWrite)
	}
	if header.Level == 0 {
		if header.Next != (PageRef{}) {
			if err := validatePageKeyChildRef(header, header.Next, fileEnd, nextLogicalID); err != nil {
				return err
			}
		}
	} else if header.Next != (PageRef{}) {
		return fmt.Errorf("%w: branch has next leaf", ErrInvalidWrite)
	}
	return nil
}

func validateEncodedPageKeyLeaf(payload []byte, count int, header PageKeyDirectoryHeader, fileEnd, nextLogicalID uint64, chunkHighWater, chunkDocuments uint32) error {
	var previous PageKeyLocation
	for i := 0; i < count; i++ {
		start := PageKeyDirectoryPayloadHeaderSize + i*PageKeyLeafEntrySize
		entry := PageKeyLocation{
			Hash:  binary.LittleEndian.Uint64(payload[start : start+8]),
			Chunk: binary.LittleEndian.Uint32(payload[start+8 : start+12]),
			Slot:  payload[start+12],
		}
		if !allZero(payload[start+13:start+16]) || entry.Chunk >= chunkHighWater ||
			uint32(entry.Slot) >= chunkDocuments || i != 0 &&
			(entry.Hash < previous.Hash || entry.Hash == previous.Hash &&
				(entry.Chunk < previous.Chunk || entry.Chunk == previous.Chunk && entry.Slot <= previous.Slot)) {
			return fmt.Errorf("%w: key leaf entry", ErrInvalidWrite)
		}
		previous = entry
	}
	if previous.Hash != header.MaxHash || binary.LittleEndian.Uint64(payload[PageKeyDirectoryPayloadHeaderSize:]) != header.MinHash {
		return fmt.Errorf("%w: key leaf bounds", ErrInvalidWrite)
	}
	if header.Next != (PageRef{}) {
		return validatePageKeyChildRef(header, header.Next, fileEnd, nextLogicalID)
	}
	return nil
}

func validateEncodedPageKeyBranch(payload []byte, count int, header PageKeyDirectoryHeader, fileEnd, nextLogicalID uint64) error {
	var previous uint64
	for i := 0; i < count; i++ {
		start := PageKeyDirectoryPayloadHeaderSize + i*PageKeyBranchEntrySize
		maxHash := binary.LittleEndian.Uint64(payload[start : start+8])
		encoded := payload[start+8 : start+PageKeyBranchEntrySize]
		if i != 0 && maxHash < previous || !pageRefReservedZero(encoded) {
			return fmt.Errorf("%w: key branch order or reserved bytes", ErrInvalidWrite)
		}
		if err := validatePageKeyChildRef(header, decodePageRef(encoded), fileEnd, nextLogicalID); err != nil {
			return err
		}
		previous = maxHash
	}
	if previous != header.MaxHash || header.MinHash > binary.LittleEndian.Uint64(payload[PageKeyDirectoryPayloadHeaderSize:]) {
		return fmt.Errorf("%w: key branch bounds", ErrInvalidWrite)
	}
	return nil
}

func validatePageKeyChildRef(header PageKeyDirectoryHeader, ref PageRef, fileEnd, nextLogicalID uint64) error {
	pageSize := uint64(header.PageSize)
	if ref.Kind != PageKeyDirectory || ref.Flags != 0 || ref.Aux != 0 || ref.Length != header.PageSize ||
		ref.Generation == 0 || ref.Generation > header.Generation ||
		ref.LogicalID <= StateRootLogicalID || ref.LogicalID >= nextLogicalID ||
		ref.LogicalID == header.LogicalID ||
		ref.Offset < uint64(superblockCopies)*pageSize || ref.Offset%pageSize != 0 ||
		ref.Offset > maxSuperblockFileOffset || uint64(ref.Length) > fileEnd || ref.Offset > fileEnd-uint64(ref.Length) {
		return fmt.Errorf("%w: invalid key-directory child", ErrInvalidWrite)
	}
	return nil
}

// KeyHash returns the deterministic 64-bit keyed hash used by durable key
// leaves. It is SipHash-1-3 with its 128-bit key derived from StoreID. Hashes
// only prune; document pages still verify complete key bytes.
func KeyHash(storeID [16]byte, key string) uint64 {
	k0 := binary.LittleEndian.Uint64(storeID[0:8])
	k1 := binary.LittleEndian.Uint64(storeID[8:16])
	v0 := k0 ^ 0x736f6d6570736575
	v1 := k1 ^ 0x646f72616e646f6d
	v2 := k0 ^ 0x6c7967656e657261
	v3 := k1 ^ 0x7465646279746573

	i := 0
	for ; i+8 <= len(key); i += 8 {
		m := uint64(key[i]) | uint64(key[i+1])<<8 | uint64(key[i+2])<<16 | uint64(key[i+3])<<24 |
			uint64(key[i+4])<<32 | uint64(key[i+5])<<40 | uint64(key[i+6])<<48 | uint64(key[i+7])<<56
		v3 ^= m
		sipRound(&v0, &v1, &v2, &v3)
		v0 ^= m
	}
	b := uint64(len(key)) << 56
	for j := i; j < len(key); j++ {
		b |= uint64(key[j]) << uint(8*(j-i))
	}
	v3 ^= b
	sipRound(&v0, &v1, &v2, &v3)
	v0 ^= b
	v2 ^= 0xff
	sipRound(&v0, &v1, &v2, &v3)
	sipRound(&v0, &v1, &v2, &v3)
	sipRound(&v0, &v1, &v2, &v3)
	return v0 ^ v1 ^ v2 ^ v3
}

func sipRound(v0, v1, v2, v3 *uint64) {
	*v0 += *v1
	*v1 = bits.RotateLeft64(*v1, 13)
	*v1 ^= *v0
	*v0 = bits.RotateLeft64(*v0, 32)
	*v2 += *v3
	*v3 = bits.RotateLeft64(*v3, 16)
	*v3 ^= *v2
	*v0 += *v3
	*v3 = bits.RotateLeft64(*v3, 21)
	*v3 ^= *v0
	*v2 += *v1
	*v1 = bits.RotateLeft64(*v1, 17)
	*v1 ^= *v2
	*v2 = bits.RotateLeft64(*v2, 32)
}
