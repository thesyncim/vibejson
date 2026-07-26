package vnext

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"

	"github.com/thesyncim/vibejson/internal/storeio"
)

// The location-keyed TTL page is deliberately isolated from the production
// deadline-ordered TTL tree. It is a format candidate for one structure that
// supports both exact row lookup and earliest-deadline traversal: leaves are
// ordered by stable row, while every branch child carries its exact subtree
// minimum deadline.
const (
	TTLLocationPageSize          = Quantum
	TTLLocationPayloadHeaderSize = 32
	TTLLocationLeafRecordSize    = 16
	TTLLocationBranchRecordSize  = 48
	TTLLocationLeafCapacity      = (TTLLocationPageSize - FrameHeaderSize -
		FrameTrailerSize - TTLLocationPayloadHeaderSize) / TTLLocationLeafRecordSize
	TTLLocationBranchCapacity = (TTLLocationPageSize - FrameHeaderSize -
		FrameTrailerSize - TTLLocationPayloadHeaderSize) / TTLLocationBranchRecordSize

	ttlLocationFrameVersion = uint16(1)
	ttlLocationVersion      = uint32(0)
	ttlLocationMagic        = "VJNXTL01"
	ttlLocationLeafKind     = uint8(1)
	ttlLocationBranchKind   = uint8(2)
	ttlLocationMaxLevel     = uint8(15)
)

// TTLLocationEntry is one exact expiry record. Row is a stable logical address,
// never a physical offset. Deadline is an exact Unix-nanosecond value; zero is
// reserved to mean that a row has no expiry and is therefore non-canonical in
// this tree.
type TTLLocationEntry struct {
	Row      uint64
	Deadline int64
}

// TTLLocationChild is one immutable branch record. Its fields are private so a
// builder cannot accidentally publish an invented subtree minimum. AsChild on
// a checksum-validated leaf or branch is the constructor.
type TTLLocationChild struct {
	lowerRow    uint64
	minDeadline int64
	ref         storeio.PageRef
}

// LowerRow is the first stable row reachable through the child.
func (c TTLLocationChild) LowerRow() uint64 { return c.lowerRow }

// MinDeadline is the exact minimum deadline in the child subtree.
func (c TTLLocationChild) MinDeadline() int64 { return c.minDeadline }

// Ref is the direct immutable page reference.
func (c TTLLocationChild) Ref() storeio.PageRef { return c.ref }

// TTLLocationPageSpace reports exact fixed-page accounting.
type TTLLocationPageSpace struct {
	ImageBytes         int
	FrameHeaderBytes   int
	PayloadHeaderBytes int
	RecordBytes        int
	PaddingBytes       int
	FrameTrailerBytes  int
	Capacity           int
	Live               int
}

// TTLLocationLeafView is a checksum- and structure-validated borrowed leaf.
type TTLLocationLeafView struct {
	identity    Identity
	payload     []byte
	count       uint16
	minRank     uint16
	lowerRow    uint64
	minDeadline int64
}

// TTLLocationBranchView is a checksum- and structure-validated borrowed branch.
type TTLLocationBranchView struct {
	identity    Identity
	payload     []byte
	count       uint16
	minRank     uint16
	level       uint8
	lowerRow    uint64
	minDeadline int64
}

// EncodeTTLLocationLeaf writes one exact 4-KiB immutable leaf. Entries must be
// strictly ordered by stable row.
func EncodeTTLLocationLeaf(
	dst []byte,
	identity Identity,
	entries []TTLLocationEntry,
) ([]byte, error) {
	if len(dst) != TTLLocationPageSize || len(entries) == 0 ||
		len(entries) > TTLLocationLeafCapacity {
		return nil, ErrInvalidFrame
	}
	minRank := 0
	for i, entry := range entries {
		if entry.Deadline == 0 || i != 0 && entry.Row <= entries[i-1].Row {
			return nil, fmt.Errorf("%w: unordered TTL-location leaf", ErrInvalidFrame)
		}
		if entry.Deadline < entries[minRank].Deadline {
			minRank = i
		}
	}
	payloadLength := TTLLocationPayloadHeaderSize +
		len(entries)*TTLLocationLeafRecordSize
	payload, err := initTTLLocationPage(
		dst, identity, ttlLocationLeafKind, payloadLength,
	)
	if err != nil {
		return nil, err
	}
	encodeTTLLocationPayloadHeader(
		payload, 0, len(entries), minRank,
		entries[0].Row, entries[minRank].Deadline,
		TTLLocationLeafRecordSize,
	)
	records := payload[TTLLocationPayloadHeaderSize:]
	for i, entry := range entries {
		record := records[i*TTLLocationLeafRecordSize:]
		binary.LittleEndian.PutUint64(record[0:8], entry.Row)
		binary.LittleEndian.PutUint64(record[8:16], uint64(entry.Deadline))
	}
	if err := sealTTLLocationPage(dst); err != nil {
		return nil, err
	}
	return dst, nil
}

// OpenTTLLocationLeaf validates one complete immutable leaf.
func OpenTTLLocationLeaf(src []byte) (TTLLocationLeafView, error) {
	header, payload, err := openTTLLocationPage(src, ttlLocationLeafKind)
	if err != nil {
		return TTLLocationLeafView{}, err
	}
	common, err := openTTLLocationPayloadHeader(
		payload, 0, TTLLocationLeafRecordSize, TTLLocationLeafCapacity,
	)
	if err != nil {
		return TTLLocationLeafView{}, err
	}
	records := payload[TTLLocationPayloadHeaderSize:]
	var previousRow uint64
	minRank := 0
	for i := 0; i < common.count; i++ {
		record := records[i*TTLLocationLeafRecordSize:]
		row := binary.LittleEndian.Uint64(record[0:8])
		deadline := int64(binary.LittleEndian.Uint64(record[8:16]))
		if deadline == 0 || i != 0 && row <= previousRow {
			return TTLLocationLeafView{}, corrupt("TTL-location leaf ordering")
		}
		if i != 0 && deadline < int64(binary.LittleEndian.Uint64(
			records[minRank*TTLLocationLeafRecordSize+8:],
		)) {
			minRank = i
		}
		previousRow = row
	}
	firstRow := binary.LittleEndian.Uint64(records[0:8])
	minDeadline := int64(binary.LittleEndian.Uint64(
		records[minRank*TTLLocationLeafRecordSize+8:],
	))
	if common.lowerRow != firstRow || common.minRank != minRank ||
		common.minDeadline != minDeadline {
		return TTLLocationLeafView{}, corrupt("TTL-location leaf summary")
	}
	return TTLLocationLeafView{
		identity: header.identity, payload: payload, count: uint16(common.count),
		minRank: uint16(minRank), lowerRow: firstRow, minDeadline: minDeadline,
	}, nil
}

// EncodeTTLLocationBranch writes one exact 4-KiB immutable branch. Children
// must be strictly ordered by LowerRow. Each child must have been derived from
// a validated page through AsChild.
func EncodeTTLLocationBranch(
	dst []byte,
	identity Identity,
	level uint8,
	children []TTLLocationChild,
) ([]byte, error) {
	if len(dst) != TTLLocationPageSize || level == 0 ||
		level > ttlLocationMaxLevel || len(children) == 0 ||
		len(children) > TTLLocationBranchCapacity {
		return nil, ErrInvalidFrame
	}
	minRank := 0
	for i, child := range children {
		if child.minDeadline == 0 ||
			!validTTLLocationChildRef(identity, child.ref) ||
			i != 0 && child.lowerRow <= children[i-1].lowerRow {
			return nil, fmt.Errorf("%w: TTL-location branch child", ErrInvalidFrame)
		}
		if child.minDeadline < children[minRank].minDeadline {
			minRank = i
		}
		for earlier := 0; earlier < i; earlier++ {
			other := children[earlier].ref
			if child.ref == other ||
				child.ref.LogicalID == other.LogicalID ||
				child.ref.Offset == other.Offset {
				return nil, fmt.Errorf(
					"%w: duplicate TTL-location child", ErrInvalidFrame,
				)
			}
		}
	}
	payloadLength := TTLLocationPayloadHeaderSize +
		len(children)*TTLLocationBranchRecordSize
	payload, err := initTTLLocationPage(
		dst, identity, ttlLocationBranchKind, payloadLength,
	)
	if err != nil {
		return nil, err
	}
	encodeTTLLocationPayloadHeader(
		payload, level, len(children), minRank,
		children[0].lowerRow, children[minRank].minDeadline,
		TTLLocationBranchRecordSize,
	)
	records := payload[TTLLocationPayloadHeaderSize:]
	for i, child := range children {
		record := records[i*TTLLocationBranchRecordSize:]
		binary.LittleEndian.PutUint64(record[0:8], child.lowerRow)
		binary.LittleEndian.PutUint64(record[8:16], uint64(child.minDeadline))
		encodeTTLLocationPageRef(record[16:48], child.ref)
	}
	if err := sealTTLLocationPage(dst); err != nil {
		return nil, err
	}
	return dst, nil
}

// OpenTTLLocationBranch validates one complete immutable branch, including its
// ordering, exact recorded minimum, direct references, and duplicate-child
// rejection.
func OpenTTLLocationBranch(src []byte) (TTLLocationBranchView, error) {
	header, payload, err := openTTLLocationPage(src, ttlLocationBranchKind)
	if err != nil {
		return TTLLocationBranchView{}, err
	}
	level := payload[4]
	if level == 0 || level > ttlLocationMaxLevel {
		return TTLLocationBranchView{}, corrupt("TTL-location branch level")
	}
	common, err := openTTLLocationPayloadHeader(
		payload, level, TTLLocationBranchRecordSize,
		TTLLocationBranchCapacity,
	)
	if err != nil {
		return TTLLocationBranchView{}, err
	}
	records := payload[TTLLocationPayloadHeaderSize:]
	var previousRow uint64
	minRank := 0
	for i := 0; i < common.count; i++ {
		record := records[i*TTLLocationBranchRecordSize:]
		row := binary.LittleEndian.Uint64(record[0:8])
		deadline := int64(binary.LittleEndian.Uint64(record[8:16]))
		ref := decodeTTLLocationPageRef(record[16:48])
		if deadline == 0 || !validTTLLocationChildRef(header.identity, ref) ||
			i != 0 && row <= previousRow {
			return TTLLocationBranchView{}, corrupt(
				"TTL-location branch ordering or reference",
			)
		}
		if i != 0 && deadline < int64(binary.LittleEndian.Uint64(
			records[minRank*TTLLocationBranchRecordSize+8:],
		)) {
			minRank = i
		}
		for earlier := 0; earlier < i; earlier++ {
			other := decodeTTLLocationPageRef(
				records[earlier*TTLLocationBranchRecordSize+16:],
			)
			if ref == other || ref.LogicalID == other.LogicalID ||
				ref.Offset == other.Offset {
				return TTLLocationBranchView{}, corrupt(
					"duplicate TTL-location branch child",
				)
			}
		}
		previousRow = row
	}
	firstRow := binary.LittleEndian.Uint64(records[0:8])
	minDeadline := int64(binary.LittleEndian.Uint64(
		records[minRank*TTLLocationBranchRecordSize+8:],
	))
	if common.lowerRow != firstRow || common.minRank != minRank ||
		common.minDeadline != minDeadline {
		return TTLLocationBranchView{}, corrupt("TTL-location branch summary")
	}
	return TTLLocationBranchView{
		identity: header.identity, payload: payload, count: uint16(common.count),
		minRank: uint16(minRank), level: level,
		lowerRow: firstRow, minDeadline: minDeadline,
	}, nil
}

// Lookup resolves one exact stable row in a leaf.
func (v TTLLocationLeafView) Lookup(row uint64) (int64, bool) {
	low, high := 0, int(v.count)
	records := v.payload[TTLLocationPayloadHeaderSize:]
	for low < high {
		middle := int(uint(low+high) >> 1)
		record := records[middle*TTLLocationLeafRecordSize:]
		current := binary.LittleEndian.Uint64(record[0:8])
		if current < row {
			low = middle + 1
		} else {
			high = middle
		}
	}
	if low == int(v.count) {
		return 0, false
	}
	record := records[low*TTLLocationLeafRecordSize:]
	if binary.LittleEndian.Uint64(record[0:8]) != row {
		return 0, false
	}
	return int64(binary.LittleEndian.Uint64(record[8:16])), true
}

// FirstDue returns the leaf's exact minimum-deadline row when it is due.
func (v TTLLocationLeafView) FirstDue(now int64) (TTLLocationEntry, bool) {
	if v.count == 0 || v.minDeadline > now {
		return TTLLocationEntry{}, false
	}
	record := v.payload[TTLLocationPayloadHeaderSize+int(v.minRank)*TTLLocationLeafRecordSize:]
	return TTLLocationEntry{
		Row:      binary.LittleEndian.Uint64(record[0:8]),
		Deadline: int64(binary.LittleEndian.Uint64(record[8:16])),
	}, true
}

// Select returns the branch child whose greatest lower bound does not exceed
// row. Rows below the branch's first lower bound have no child.
func (v TTLLocationBranchView) Select(row uint64) (TTLLocationChild, bool) {
	low, high := 0, int(v.count)
	records := v.payload[TTLLocationPayloadHeaderSize:]
	for low < high {
		middle := int(uint(low+high) >> 1)
		record := records[middle*TTLLocationBranchRecordSize:]
		if binary.LittleEndian.Uint64(record[0:8]) <= row {
			low = middle + 1
		} else {
			high = middle
		}
	}
	if low == 0 {
		return TTLLocationChild{}, false
	}
	return decodeTTLLocationChild(
		records[(low-1)*TTLLocationBranchRecordSize:],
	), true
}

// FirstDue selects the child with the exact minimum subtree deadline when that
// deadline is due. Following FirstDue until a leaf yields the global earliest
// expiry without a duplicate inverse table.
func (v TTLLocationBranchView) FirstDue(now int64) (TTLLocationChild, bool) {
	if v.count == 0 || v.minDeadline > now {
		return TTLLocationChild{}, false
	}
	record := v.payload[TTLLocationPayloadHeaderSize+int(v.minRank)*TTLLocationBranchRecordSize:]
	return decodeTTLLocationChild(record), true
}

// EntryAt and ChildAt expose detached value copies for builders and tests.
func (v TTLLocationLeafView) EntryAt(rank int) (TTLLocationEntry, bool) {
	if rank < 0 || rank >= int(v.count) {
		return TTLLocationEntry{}, false
	}
	record := v.payload[TTLLocationPayloadHeaderSize+rank*TTLLocationLeafRecordSize:]
	return TTLLocationEntry{
		Row:      binary.LittleEndian.Uint64(record[0:8]),
		Deadline: int64(binary.LittleEndian.Uint64(record[8:16])),
	}, true
}

func (v TTLLocationBranchView) ChildAt(rank int) (TTLLocationChild, bool) {
	if rank < 0 || rank >= int(v.count) {
		return TTLLocationChild{}, false
	}
	return decodeTTLLocationChild(v.payload[TTLLocationPayloadHeaderSize+rank*TTLLocationBranchRecordSize:]), true
}

// AsChild derives a branch record from a validated page. It verifies that ref
// names that exact page identity, preventing a builder from pairing a PageRef
// with an invented lower row or minimum deadline.
func (v TTLLocationLeafView) AsChild(
	ref storeio.PageRef,
) (TTLLocationChild, error) {
	return ttlLocationAsChild(
		v.identity, v.lowerRow, v.minDeadline, ref,
	)
}

func (v TTLLocationBranchView) AsChild(
	ref storeio.PageRef,
) (TTLLocationChild, error) {
	return ttlLocationAsChild(
		v.identity, v.lowerRow, v.minDeadline, ref,
	)
}

// Identity, Len, Level, LowerRow, and MinDeadline expose value-only summaries.
func (v TTLLocationLeafView) Identity() Identity { return v.identity }
func (v TTLLocationLeafView) Len() int           { return int(v.count) }
func (v TTLLocationLeafView) LowerRow() uint64   { return v.lowerRow }
func (v TTLLocationLeafView) MinDeadline() int64 { return v.minDeadline }

func (v TTLLocationBranchView) Identity() Identity { return v.identity }
func (v TTLLocationBranchView) Len() int           { return int(v.count) }
func (v TTLLocationBranchView) Level() uint8       { return v.level }
func (v TTLLocationBranchView) LowerRow() uint64   { return v.lowerRow }
func (v TTLLocationBranchView) MinDeadline() int64 { return v.minDeadline }

func (v TTLLocationLeafView) Space() TTLLocationPageSpace {
	return ttlLocationSpace(
		TTLLocationLeafRecordSize, TTLLocationLeafCapacity, int(v.count),
	)
}

func (v TTLLocationBranchView) Space() TTLLocationPageSpace {
	return ttlLocationSpace(
		TTLLocationBranchRecordSize, TTLLocationBranchCapacity, int(v.count),
	)
}

type ttlLocationCommon struct {
	count       int
	minRank     int
	lowerRow    uint64
	minDeadline int64
}

func encodeTTLLocationPayloadHeader(
	payload []byte,
	level uint8,
	count, minRank int,
	lowerRow uint64,
	minDeadline int64,
	recordSize int,
) {
	binary.LittleEndian.PutUint32(payload[0:4], ttlLocationVersion)
	payload[4] = level
	binary.LittleEndian.PutUint16(payload[6:8], uint16(count))
	binary.LittleEndian.PutUint64(payload[8:16], uint64(minDeadline))
	binary.LittleEndian.PutUint64(payload[16:24], lowerRow)
	binary.LittleEndian.PutUint16(payload[24:26], uint16(minRank))
	binary.LittleEndian.PutUint16(payload[26:28], uint16(recordSize))
}

func openTTLLocationPayloadHeader(
	payload []byte,
	level uint8,
	recordSize, capacity int,
) (ttlLocationCommon, error) {
	if len(payload) < TTLLocationPayloadHeaderSize ||
		binary.LittleEndian.Uint32(payload[0:4]) != ttlLocationVersion ||
		payload[4] != level || payload[5] != 0 ||
		!allZero(payload[28:TTLLocationPayloadHeaderSize]) ||
		int(binary.LittleEndian.Uint16(payload[26:28])) != recordSize {
		return ttlLocationCommon{}, corrupt("TTL-location payload header")
	}
	count := int(binary.LittleEndian.Uint16(payload[6:8]))
	minRank := int(binary.LittleEndian.Uint16(payload[24:26]))
	if count == 0 || count > capacity || minRank >= count ||
		len(payload) != TTLLocationPayloadHeaderSize+count*recordSize {
		return ttlLocationCommon{}, corrupt("TTL-location payload bounds")
	}
	return ttlLocationCommon{
		count: count, minRank: minRank,
		minDeadline: int64(binary.LittleEndian.Uint64(payload[8:16])),
		lowerRow:    binary.LittleEndian.Uint64(payload[16:24]),
	}, nil
}

func initTTLLocationPage(
	dst []byte,
	identity Identity,
	kind uint8,
	payloadLength int,
) ([]byte, error) {
	if len(dst) != TTLLocationPageSize ||
		identity.StoreID == ([16]byte{}) || identity.Generation == 0 ||
		identity.LogicalID <= storeio.StateRootLogicalID ||
		kind != ttlLocationLeafKind && kind != ttlLocationBranchKind ||
		payloadLength < TTLLocationPayloadHeaderSize ||
		payloadLength > TTLLocationPageSize-FrameHeaderSize-FrameTrailerSize {
		return nil, ErrInvalidFrame
	}
	clear(dst)
	copy(dst[0:8], ttlLocationMagic)
	binary.LittleEndian.PutUint16(dst[8:10], ttlLocationFrameVersion)
	binary.LittleEndian.PutUint16(dst[10:12], FrameHeaderSize)
	dst[12] = kind
	binary.LittleEndian.PutUint32(dst[16:20], TTLLocationPageSize)
	binary.LittleEndian.PutUint32(dst[20:24], uint32(payloadLength))
	binary.LittleEndian.PutUint64(dst[24:32], identity.Generation)
	binary.LittleEndian.PutUint64(dst[32:40], identity.LogicalID)
	copy(dst[40:56], identity.StoreID[:])
	end := FrameHeaderSize + payloadLength
	return dst[FrameHeaderSize:end:end], nil
}

func sealTTLLocationPage(page []byte) error {
	if len(page) != TTLLocationPageSize ||
		!validTTLLocationFrameHeader(page, 0) {
		return ErrInvalidFrame
	}
	payloadLength := int(binary.LittleEndian.Uint32(page[20:24]))
	trailer := TTLLocationPageSize - FrameTrailerSize
	if !allZero(page[13:16]) || !allZero(page[56:64]) ||
		!allZero(page[FrameHeaderSize+payloadLength:trailer]) {
		return ErrInvalidFrame
	}
	checksum := crc32.Checksum(page[:trailer], frameCRC)
	binary.LittleEndian.PutUint32(page[trailer:trailer+4], checksum)
	binary.LittleEndian.PutUint32(page[trailer+4:], ^checksum)
	return nil
}

func openTTLLocationPage(
	src []byte,
	kind uint8,
) (frameHeader, []byte, error) {
	if len(src) != TTLLocationPageSize ||
		!validTTLLocationFrameHeader(src, kind) {
		return frameHeader{}, nil, ErrCorrupt
	}
	trailer := TTLLocationPageSize - FrameTrailerSize
	checksum := binary.LittleEndian.Uint32(src[trailer : trailer+4])
	payloadLength := int(binary.LittleEndian.Uint32(src[20:24]))
	if binary.LittleEndian.Uint32(src[trailer+4:]) != ^checksum ||
		crc32.Checksum(src[:trailer], frameCRC) != checksum ||
		!allZero(src[13:16]) || !allZero(src[56:64]) ||
		!allZero(src[FrameHeaderSize+payloadLength:trailer]) {
		return frameHeader{}, nil, ErrCorrupt
	}
	header := frameHeader{
		kind:          frameKind(kind),
		span:          TTLLocationPageSize,
		payloadLength: uint32(payloadLength),
		identity: Identity{
			Generation: binary.LittleEndian.Uint64(src[24:32]),
			LogicalID:  binary.LittleEndian.Uint64(src[32:40]),
		},
	}
	copy(header.identity.StoreID[:], src[40:56])
	end := FrameHeaderSize + payloadLength
	return header, src[FrameHeaderSize:end:end], nil
}

func validTTLLocationFrameHeader(src []byte, kind uint8) bool {
	if len(src) != TTLLocationPageSize ||
		string(src[0:8]) != ttlLocationMagic ||
		binary.LittleEndian.Uint16(src[8:10]) != ttlLocationFrameVersion ||
		binary.LittleEndian.Uint16(src[10:12]) != FrameHeaderSize ||
		(kind != 0 && src[12] != kind) ||
		(src[12] != ttlLocationLeafKind && src[12] != ttlLocationBranchKind) ||
		binary.LittleEndian.Uint32(src[16:20]) != TTLLocationPageSize {
		return false
	}
	payloadLength := binary.LittleEndian.Uint32(src[20:24])
	if uint64(payloadLength) < TTLLocationPayloadHeaderSize ||
		uint64(payloadLength) >
			TTLLocationPageSize-FrameHeaderSize-FrameTrailerSize ||
		binary.LittleEndian.Uint64(src[24:32]) == 0 ||
		binary.LittleEndian.Uint64(src[32:40]) <= storeio.StateRootLogicalID ||
		allZero(src[40:56]) {
		return false
	}
	return true
}

func ttlLocationAsChild(
	identity Identity,
	lowerRow uint64,
	minDeadline int64,
	ref storeio.PageRef,
) (TTLLocationChild, error) {
	if minDeadline == 0 || ref.Offset < 2*Quantum ||
		ref.Offset%Quantum != 0 ||
		ref.Offset > ^uint64(0)-TTLLocationPageSize ||
		ref.LogicalID <= storeio.StateRootLogicalID ||
		ref.Length != TTLLocationPageSize ||
		ref.Kind != storeio.PageTTLDirectory ||
		ref.Flags != 0 || ref.Aux != 0 ||
		ref.Generation != identity.Generation ||
		ref.LogicalID != identity.LogicalID {
		return TTLLocationChild{}, fmt.Errorf(
			"%w: TTL-location child identity", ErrInvalidFrame,
		)
	}
	return TTLLocationChild{
		lowerRow: lowerRow, minDeadline: minDeadline, ref: ref,
	}, nil
}

func validTTLLocationChildRef(
	parent Identity,
	ref storeio.PageRef,
) bool {
	return ref.Offset >= 2*Quantum &&
		ref.Offset%Quantum == 0 &&
		ref.Offset <= ^uint64(0)-TTLLocationPageSize &&
		ref.LogicalID > storeio.StateRootLogicalID &&
		ref.LogicalID != parent.LogicalID &&
		ref.Generation != 0 &&
		ref.Generation <= parent.Generation &&
		ref.Length == TTLLocationPageSize &&
		ref.Kind == storeio.PageTTLDirectory &&
		ref.Flags == 0 && ref.Aux == 0
}

func encodeTTLLocationPageRef(dst []byte, ref storeio.PageRef) {
	binary.LittleEndian.PutUint64(dst[0:8], ref.Offset)
	binary.LittleEndian.PutUint64(dst[8:16], ref.LogicalID)
	binary.LittleEndian.PutUint64(dst[16:24], ref.Generation)
	binary.LittleEndian.PutUint32(dst[24:28], ref.Length)
	dst[28] = byte(ref.Kind)
	dst[29] = ref.Flags
	binary.LittleEndian.PutUint16(dst[30:32], ref.Aux)
}

func decodeTTLLocationPageRef(src []byte) storeio.PageRef {
	return storeio.PageRef{
		Offset:     binary.LittleEndian.Uint64(src[0:8]),
		LogicalID:  binary.LittleEndian.Uint64(src[8:16]),
		Generation: binary.LittleEndian.Uint64(src[16:24]),
		Length:     binary.LittleEndian.Uint32(src[24:28]),
		Kind:       storeio.PageKind(src[28]),
		Flags:      src[29],
		Aux:        binary.LittleEndian.Uint16(src[30:32]),
	}
}

func decodeTTLLocationChild(src []byte) TTLLocationChild {
	return TTLLocationChild{
		lowerRow:    binary.LittleEndian.Uint64(src[0:8]),
		minDeadline: int64(binary.LittleEndian.Uint64(src[8:16])),
		ref:         decodeTTLLocationPageRef(src[16:48]),
	}
}

func ttlLocationSpace(
	recordSize, capacity, live int,
) TTLLocationPageSpace {
	recordBytes := live * recordSize
	return TTLLocationPageSpace{
		ImageBytes:         TTLLocationPageSize,
		FrameHeaderBytes:   FrameHeaderSize,
		PayloadHeaderBytes: TTLLocationPayloadHeaderSize,
		RecordBytes:        recordBytes,
		PaddingBytes: TTLLocationPageSize - FrameHeaderSize -
			FrameTrailerSize - TTLLocationPayloadHeaderSize - recordBytes,
		FrameTrailerBytes: FrameTrailerSize,
		Capacity:          capacity,
		Live:              live,
	}
}
