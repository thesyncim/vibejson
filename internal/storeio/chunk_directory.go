package storeio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
)

const (
	ChunkDirectoryPayloadHeaderSize = 32
	chunkDirectoryVersion           = uint32(1)
	chunkDirectoryKnownFlags        = uint16(0)
	chunkDirectoryRadixBits         = uint8(6)
	chunkDirectoryMaxShift          = uint8(30)
	chunkDirectoryRefSetSize        = 128
)

// ErrChunkDirectoryCorrupt reports a common page whose packed chunk-directory
// payload is malformed or contains an invalid physical reference.
var ErrChunkDirectoryCorrupt = errors.New("simdjson: corrupt Store chunk directory")

// ChunkDirectoryHeader describes one immutable packed-radix node. Shift is a
// multiple of six; zero identifies a leaf whose entries point to document
// pages or immutable multi-chunk document groups. Several leaf lanes may name
// one group extent. Higher levels point to unique chunk-directory pages.
// Bitmap lane order defines the packed reference order, so sparse nodes store
// no empty slots.
type ChunkDirectoryHeader struct {
	StoreID    [16]byte
	Generation uint64
	LogicalID  uint64
	PageSize   uint32
	Prefix     uint32
	Bitmap     uint64
	Shift      uint8
	Flags      uint16
}

// ChunkDirectoryView is an admitted, checksum-verified directory page. It
// retains only a borrowed view of one resident page, so pointer count scales
// with the bounded frame cache rather than with keys or chunks.
type ChunkDirectoryView struct {
	header  ChunkDirectoryHeader
	payload []byte
}

// EncodeChunkDirectoryPage writes one complete pointer-free directory node.
// refs must be ordered by increasing set-bit lane. fileEnd and nextLogicalID
// come from the state root and bound every child before publication. No
// allocation is performed.
func EncodeChunkDirectoryPage(dst []byte, header ChunkDirectoryHeader, refs []PageRef, fileEnd, nextLogicalID uint64) ([]byte, error) {
	if err := validateChunkDirectoryHeader(header, len(refs), fileEnd, nextLogicalID); err != nil {
		return nil, err
	}
	if err := validateChunkDirectoryRefs(header, refs, fileEnd, nextLogicalID); err != nil {
		return nil, err
	}
	payloadLength := ChunkDirectoryPayloadHeaderSize + len(refs)*PageRefSize
	payload, err := InitPage(dst, PageHeader{
		StoreID:       header.StoreID,
		Generation:    header.Generation,
		LogicalID:     header.LogicalID,
		PageSize:      header.PageSize,
		PayloadLength: uint32(payloadLength),
		Kind:          PageChunkDirectory,
	})
	if err != nil {
		return nil, err
	}
	binary.LittleEndian.PutUint32(payload[0:4], chunkDirectoryVersion)
	binary.LittleEndian.PutUint32(payload[4:8], header.Prefix)
	binary.LittleEndian.PutUint64(payload[8:16], header.Bitmap)
	payload[16] = header.Shift
	payload[17] = uint8(len(refs))
	binary.LittleEndian.PutUint16(payload[18:20], header.Flags)
	for i, ref := range refs {
		start := ChunkDirectoryPayloadHeaderSize + i*PageRefSize
		encodePageRef(payload[start:start+PageRefSize], ref)
	}
	page := dst[:int(header.PageSize)]
	if _, err := sealInitializedPage(page); err != nil {
		return nil, err
	}
	return page, nil
}

// OpenChunkDirectoryPage verifies a common page and its packed-radix payload
// once, returning a borrowed view for repeated allocation-free lookups.
func OpenChunkDirectoryPage(src []byte, fileEnd, nextLogicalID uint64) (ChunkDirectoryView, error) {
	pageHeader, payload, err := OpenPage(src)
	if err != nil {
		return ChunkDirectoryView{}, fmt.Errorf("%w: %w", ErrChunkDirectoryCorrupt, err)
	}
	if pageHeader.Kind != PageChunkDirectory || len(payload) < ChunkDirectoryPayloadHeaderSize ||
		binary.LittleEndian.Uint32(payload[0:4]) != chunkDirectoryVersion ||
		!allZero(payload[20:ChunkDirectoryPayloadHeaderSize]) {
		return ChunkDirectoryView{}, fmt.Errorf("%w: header, version, or reserved bytes", ErrChunkDirectoryCorrupt)
	}
	count := int(payload[17])
	header := ChunkDirectoryHeader{
		StoreID:    pageHeader.StoreID,
		Generation: pageHeader.Generation,
		LogicalID:  pageHeader.LogicalID,
		PageSize:   pageHeader.PageSize,
		Prefix:     binary.LittleEndian.Uint32(payload[4:8]),
		Bitmap:     binary.LittleEndian.Uint64(payload[8:16]),
		Shift:      payload[16],
		Flags:      binary.LittleEndian.Uint16(payload[18:20]),
	}
	if len(payload) != ChunkDirectoryPayloadHeaderSize+count*PageRefSize {
		return ChunkDirectoryView{}, fmt.Errorf("%w: payload length", ErrChunkDirectoryCorrupt)
	}
	if err := validateChunkDirectoryHeader(header, count, fileEnd, nextLogicalID); err != nil {
		return ChunkDirectoryView{}, fmt.Errorf("%w: %v", ErrChunkDirectoryCorrupt, err)
	}
	var seen chunkDirectoryRefSet
	for i := 0; i < count; i++ {
		start := ChunkDirectoryPayloadHeaderSize + i*PageRefSize
		encoded := payload[start : start+PageRefSize]
		ref := decodePageRef(encoded)
		if err := validateChunkDirectoryRef(header, ref, fileEnd, nextLogicalID); err != nil {
			return ChunkDirectoryView{}, fmt.Errorf("%w: %v", ErrChunkDirectoryCorrupt, err)
		}
		if (header.Shift != 0 || ref.Kind != PageDocumentGroup) && !seen.add(ref) {
			return ChunkDirectoryView{}, fmt.Errorf("%w: duplicate child reference", ErrChunkDirectoryCorrupt)
		}
	}
	return ChunkDirectoryView{header: header, payload: payload}, nil
}

// AdmittedChunkDirectoryPage reconstructs a view of a page already validated
// by PageCache admission. Calling it on arbitrary bytes is invalid. It avoids
// repeating CRC and whole-node validation on every resident lookup.
func AdmittedChunkDirectoryPage(src []byte) ChunkDirectoryView {
	pageHeader, _ := decodePageHeader(src)
	payloadEnd := PageHeaderSize + int(pageHeader.PayloadLength)
	payload := src[PageHeaderSize:payloadEnd:payloadEnd]
	return ChunkDirectoryView{
		header: ChunkDirectoryHeader{
			StoreID: pageHeader.StoreID, Generation: pageHeader.Generation,
			LogicalID: pageHeader.LogicalID, PageSize: pageHeader.PageSize,
			Prefix: binary.LittleEndian.Uint32(payload[4:8]),
			Bitmap: binary.LittleEndian.Uint64(payload[8:16]), Shift: payload[16],
			Flags: binary.LittleEndian.Uint16(payload[18:20]),
		},
		payload: payload,
	}
}

// Header returns the value-only identity and radix metadata of the view.
func (v ChunkDirectoryView) Header() ChunkDirectoryHeader { return v.header }

// Len returns the number of packed child references.
func (v ChunkDirectoryView) Len() int { return bits.OnesCount64(v.header.Bitmap) }

// RefAt returns the reference at packed rank. It performs no checksum or heap
// work because OpenChunkDirectoryPage validated the complete payload once.
func (v ChunkDirectoryView) RefAt(rank int) (PageRef, bool) {
	if rank < 0 || rank >= v.Len() {
		return PageRef{}, false
	}
	return chunkDirectoryRefAt(v.payload, rank)
}

// ChunkIDAt returns the logical chunk id represented by a leaf rank. It is
// false for branch nodes. Rank follows the increasing set-bit order used by
// RefAt, so callers can enumerate a sparse leaf without scanning empty ids.
func (v ChunkDirectoryView) ChunkIDAt(rank int) (uint32, bool) {
	if v.header.Shift != 0 || rank < 0 || rank >= v.Len() {
		return 0, false
	}
	bitmap := v.header.Bitmap
	for range rank {
		bitmap &= bitmap - 1
	}
	lane := uint32(bits.TrailingZeros64(bitmap))
	return v.header.Prefix | lane, true
}

// Lookup resolves one logical chunk id with a prefix check, bitmap probe, and
// popcount rank. A branch result names another chunk-directory page; a leaf
// result names the immutable document page for that chunk.
func (v ChunkDirectoryView) Lookup(chunkID uint32) (PageRef, bool) {
	if chunkDirectoryPrefix(chunkID, v.header.Shift) != v.header.Prefix {
		return PageRef{}, false
	}
	lane := uint8(chunkID >> v.header.Shift & 63)
	bit := uint64(1) << lane
	if v.header.Bitmap&bit == 0 {
		return PageRef{}, false
	}
	rank := bits.OnesCount64(v.header.Bitmap & (bit - 1))
	return chunkDirectoryRefAt(v.payload, rank)
}

func chunkDirectoryRefAt(payload []byte, rank int) (PageRef, bool) {
	start := ChunkDirectoryPayloadHeaderSize + rank*PageRefSize
	if rank < 0 || start < ChunkDirectoryPayloadHeaderSize || start+PageRefSize > len(payload) {
		return PageRef{}, false
	}
	return decodePageRef(payload[start : start+PageRefSize]), true
}

func validateChunkDirectoryHeader(header ChunkDirectoryHeader, count int, fileEnd, nextLogicalID uint64) error {
	if header.StoreID == ([16]byte{}) || header.Generation == 0 || header.LogicalID <= StateRootLogicalID ||
		header.LogicalID >= nextLogicalID || !validPhysicalPageSize(header.PageSize) ||
		header.Flags&^chunkDirectoryKnownFlags != 0 {
		return fmt.Errorf("%w: directory identity, page size, or flags", ErrInvalidWrite)
	}
	if header.Bitmap == 0 || count != bits.OnesCount64(header.Bitmap) || count > 64 ||
		header.Shift > chunkDirectoryMaxShift || header.Shift%chunkDirectoryRadixBits != 0 ||
		chunkDirectoryPrefix(header.Prefix, header.Shift) != header.Prefix {
		return fmt.Errorf("%w: directory radix metadata", ErrInvalidWrite)
	}
	if header.Shift == chunkDirectoryMaxShift && header.Bitmap&^uint64(0xf) != 0 {
		return fmt.Errorf("%w: high directory lanes exceed uint32", ErrInvalidWrite)
	}
	pageSize := uint64(header.PageSize)
	if fileEnd < uint64(superblockCopies)*pageSize || fileEnd > maxSuperblockFileOffset || fileEnd%pageSize != 0 ||
		nextLogicalID <= StateRootLogicalID {
		return fmt.Errorf("%w: directory bounds", ErrInvalidWrite)
	}
	payloadLength := uint64(ChunkDirectoryPayloadHeaderSize + count*PageRefSize)
	if payloadLength > pageSize-PageHeaderSize-PageTrailerSize {
		return fmt.Errorf("%w: directory payload does not fit", ErrInvalidWrite)
	}
	return nil
}

func validateChunkDirectoryRefs(header ChunkDirectoryHeader, refs []PageRef, fileEnd, nextLogicalID uint64) error {
	var seen chunkDirectoryRefSet
	for _, ref := range refs {
		if err := validateChunkDirectoryRef(header, ref, fileEnd, nextLogicalID); err != nil {
			return err
		}
		if (header.Shift != 0 || ref.Kind != PageDocumentGroup) && !seen.add(ref) {
			return fmt.Errorf("%w: duplicate child reference", ErrInvalidWrite)
		}
	}
	return nil
}

// chunkDirectoryRefSet is a bounded, stack-resident uniqueness filter for one
// 64-way node. Two independent tables avoid conflating logical and physical
// namespaces. Valid references never contain zero, which is the empty marker.
// A 2:1 table-to-entry ratio bounds probes without allocating or retaining a
// Go pointer per reference.
type chunkDirectoryRefSet struct {
	logical [chunkDirectoryRefSetSize]uint64
	offset  [chunkDirectoryRefSetSize]uint64
}

func (set *chunkDirectoryRefSet) add(ref PageRef) bool {
	if !chunkDirectoryRefSetInsert(&set.logical, ref.LogicalID) {
		return false
	}
	if !chunkDirectoryRefSetInsert(&set.offset, ref.Offset) {
		return false
	}
	return true
}

func chunkDirectoryRefSetInsert(table *[chunkDirectoryRefSetSize]uint64, value uint64) bool {
	// Fibonacci hashing distributes both consecutive logical ids and aligned
	// physical offsets across the power-of-two table.
	slot := value * 0x9e3779b97f4a7c15 >> 57
	for {
		current := table[slot]
		if current == 0 {
			table[slot] = value
			return true
		}
		if current == value {
			return false
		}
		slot = (slot + 1) & (chunkDirectoryRefSetSize - 1)
	}
}

func validateChunkDirectoryRef(header ChunkDirectoryHeader, ref PageRef, fileEnd, nextLogicalID uint64) error {
	wantKind := PageChunkDirectory
	validLength := ref.Length == header.PageSize
	if header.Shift == 0 {
		wantKind = ref.Kind
		// Directory and metadata nodes use the Store's allocation quantum.
		// Document and group leaves may occupy larger power-of-two extents so
		// packed rows stay contiguous without forcing sparse metadata pages to
		// the same size.
		validLength = (ref.Kind == PageDocument || ref.Kind == PageDocumentGroup) &&
			ref.Length >= header.PageSize && validPhysicalPageSize(ref.Length)
	}
	pageSize := uint64(header.PageSize)
	length := uint64(ref.Length)
	validFlags := ref.Flags == 0 && ref.Aux == 0
	if header.Shift == 0 && ref.Kind == PageDocumentGroup {
		flags := uint16(ref.Flags)
		validFlags = flags&^documentGroupKnownFlags == 0 &&
			(flags&DocumentGroupFlagFloat64Sidecar != 0 || flags == 0)
		if validFlags && flags == 0 {
			validFlags = ref.Aux == 0
		}
		if validFlags && flags != 0 {
			sidecar, found, err := DocumentGroupFloat64Sidecar(ref, header.PageSize)
			validFlags = err == nil && found && sidecar.LogicalID < nextLogicalID &&
				uint64(sidecar.Length) <= fileEnd && sidecar.Offset <= fileEnd-uint64(sidecar.Length)
		}
	}
	if ref.Kind != wantKind || !validFlags || !validLength ||
		ref.Generation == 0 || ref.Generation > header.Generation ||
		ref.LogicalID <= StateRootLogicalID || ref.LogicalID >= nextLogicalID ||
		ref.Offset < uint64(superblockCopies)*pageSize || ref.Offset%pageSize != 0 ||
		ref.Offset > maxSuperblockFileOffset || length > fileEnd || ref.Offset > fileEnd-length {
		return fmt.Errorf("%w: invalid chunk-directory child", ErrInvalidWrite)
	}
	return nil
}

func chunkDirectoryPrefix(chunkID uint32, shift uint8) uint32 {
	covered := shift + chunkDirectoryRadixBits
	if covered >= 32 {
		return 0
	}
	return chunkID &^ (uint32(1)<<covered - 1)
}
