package storeio

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	// StateRootLogicalID is fixed so a recovered superblock cannot silently
	// reinterpret an arbitrary page as the root of a Store generation.
	StateRootLogicalID = uint64(1)
	// StateRootPayloadSize leaves a fixed reserved suffix for compatible fields
	// while keeping every current root reference at a stable byte offset.
	StateRootPayloadSize = 256
	// PageRefSize is the fixed encoded width of one pointer-free physical page
	// reference.
	PageRefSize = 32

	stateRootVersionV2        = uint32(2)
	stateRootVersionV3        = uint32(3)
	stateRootVersionV4        = uint32(4)
	stateRootVersion          = uint32(5)
	stateRootFreeHintOffset   = 60
	stateRootChunkRefOffset   = 64
	stateRootKeyRefOffset     = stateRootChunkRefOffset + PageRefSize
	stateRootIndexRefOffset   = stateRootKeyRefOffset + PageRefSize
	stateRootTTLRefOffset     = stateRootIndexRefOffset + PageRefSize
	stateRootRefsEnd          = stateRootTTLRefOffset + PageRefSize
	stateRootFloat64Offset    = stateRootRefsEnd
	stateRootFloat64End       = stateRootFloat64Offset + PageRefSize
	stateRootIndexGroupOffset = stateRootFloat64End
	stateRootIndexGroupEnd    = stateRootIndexGroupOffset + PageRefSize
)

// State-root option bits are durable equivalents of Store construction
// options. Unknown bits fail closed.
const (
	StateOptionShapeTapes uint32 = 1 << iota
	StateOptionPostings
	StateOptionValueDict
	StateOptionHashKeys
	// StateOptionFloat64Columns means every live document page carries the
	// complete configured float64 covering-column catalog.
	StateOptionFloat64Columns
	// StateOptionSchema means the durable catalog hash also binds an
	// application-supplied document schema. The schema definition remains
	// caller configuration; reopening with a different definition fails.
	StateOptionSchema
)

const stateRootKnownOptions = StateOptionShapeTapes |
	StateOptionPostings |
	StateOptionValueDict |
	StateOptionHashKeys |
	StateOptionFloat64Columns |
	StateOptionSchema

// ErrStateRootCorrupt reports a common page that passed basic framing but does
// not encode a valid Store state root.
var ErrStateRootCorrupt = errors.New("slopjson: corrupt Store state root")

// PageRef is a durable pointer to one immutable logical-page version. Offset
// is physical and changes on replacement; LogicalID is stable. Generation may
// be older than the state root because unchanged pages are shared across
// copy-on-write generations.
type PageRef struct {
	Offset     uint64
	LogicalID  uint64
	Generation uint64
	Length     uint32
	Kind       PageKind
	Flags      uint8
	// Aux is routing metadata for schemas that explicitly define it. It is
	// zero for every ordinary reference. Document-group leaves use it only
	// with DocumentGroupFlagFloat64Sidecar to encode bounded physical and
	// logical deltas to a shared typed extent.
	Aux uint16
}

// StateRoot is the compact, pointer-free graph root named by a Superblock.
// Its four directory references separate document placement, key lookup,
// secondary indexes, and TTL ordering so a mutation copies only affected
// paths. The persistent free-page tree remains a separate Superblock root.
type StateRoot struct {
	StoreID        [16]byte
	Generation     uint64
	PageSize       uint32
	Options        uint32
	DocumentCount  uint64
	TTLCount       uint64
	NextLogicalID  uint64
	ChunkHighWater uint32
	LiveChunks     uint32
	ChunkDocuments uint32
	IndexCount     uint32
	IndexMaxDepth  uint32
	// IndexCatalogHash binds every caller-supplied durable accelerator and
	// validation contract: exact indexes, covering columns, and schema.
	IndexCatalogHash uint64
	// FreeChunkHint is a conservative lower bound for the first chunk that
	// may contain a free stable slot. ChunkHighWater means no known hole.
	// Insertion advances it while delete can lower it in O(1), avoiding a
	// heap-side free-slot object or pointer for every key.
	FreeChunkHint  uint32
	ChunkDirectory PageRef
	KeyDirectory   PageRef
	IndexDirectory PageRef
	TTLDirectory   PageRef
	// Float64ScanHead names the ordered value-stripe directory of a compact or
	// incrementally maintained generation. It is only a scan accelerator;
	// documented mutation fallbacks clear it and use the authoritative tree.
	Float64ScanHead PageRef
	// IndexGroupHead names bounded aggregate-only categorical cover pages.
	// Exact postings remain authoritative.
	IndexGroupHead PageRef
}

// EncodeStateRootPage writes and seals one complete common-format page into
// caller storage. fileEnd is copied from the prospective Superblock and bounds
// every physical reference before publication. No allocation is performed.
func EncodeStateRootPage(dst []byte, root StateRoot, fileEnd uint64) ([]byte, error) {
	if err := validateStateRoot(root, fileEnd); err != nil {
		return nil, err
	}
	header := PageHeader{
		StoreID:       root.StoreID,
		Generation:    root.Generation,
		LogicalID:     StateRootLogicalID,
		PageSize:      root.PageSize,
		PayloadLength: StateRootPayloadSize,
		Kind:          PageStateRoot,
	}
	payload, err := InitPage(dst, header)
	if err != nil {
		return nil, err
	}
	binary.LittleEndian.PutUint32(payload[0:4], stateRootVersion)
	binary.LittleEndian.PutUint32(payload[4:8], root.Options)
	binary.LittleEndian.PutUint64(payload[8:16], root.DocumentCount)
	binary.LittleEndian.PutUint64(payload[16:24], root.TTLCount)
	binary.LittleEndian.PutUint64(payload[24:32], root.NextLogicalID)
	binary.LittleEndian.PutUint32(payload[32:36], root.ChunkHighWater)
	binary.LittleEndian.PutUint32(payload[36:40], root.LiveChunks)
	binary.LittleEndian.PutUint32(payload[40:44], root.ChunkDocuments)
	binary.LittleEndian.PutUint32(payload[44:48], root.IndexCount)
	binary.LittleEndian.PutUint32(payload[48:52], root.IndexMaxDepth)
	binary.LittleEndian.PutUint64(payload[52:60], root.IndexCatalogHash)
	binary.LittleEndian.PutUint32(payload[stateRootFreeHintOffset:stateRootChunkRefOffset], root.FreeChunkHint)
	encodePageRef(payload[stateRootChunkRefOffset:stateRootKeyRefOffset], root.ChunkDirectory)
	encodePageRef(payload[stateRootKeyRefOffset:stateRootIndexRefOffset], root.KeyDirectory)
	encodePageRef(payload[stateRootIndexRefOffset:stateRootTTLRefOffset], root.IndexDirectory)
	encodePageRef(payload[stateRootTTLRefOffset:stateRootRefsEnd], root.TTLDirectory)
	encodePageRef(payload[stateRootFloat64Offset:stateRootFloat64End], root.Float64ScanHead)
	encodePageRef(payload[stateRootIndexGroupOffset:stateRootIndexGroupEnd], root.IndexGroupHead)
	page := dst[:int(root.PageSize)]
	if _, err := sealInitializedPage(page); err != nil {
		return nil, err
	}
	return page, nil
}

// DecodeStateRootPage verifies a complete common page and its state-root
// schema. fileEnd is the high-water mark from the selecting Superblock. The
// result contains values only and retains no reference to src.
func DecodeStateRootPage(src []byte, fileEnd uint64) (StateRoot, error) {
	header, payload, err := OpenPage(src)
	if err != nil {
		return StateRoot{}, fmt.Errorf("%w: %w", ErrStateRootCorrupt, err)
	}
	version := binary.LittleEndian.Uint32(payload[0:4])
	if header.Kind != PageStateRoot || header.LogicalID != StateRootLogicalID ||
		len(payload) != StateRootPayloadSize ||
		version != stateRootVersionV2 && version != stateRootVersionV3 &&
			version != stateRootVersionV4 && version != stateRootVersion ||
		!pageRefReservedZero(payload[stateRootChunkRefOffset:stateRootKeyRefOffset]) ||
		!pageRefReservedZero(payload[stateRootKeyRefOffset:stateRootIndexRefOffset]) ||
		!pageRefReservedZero(payload[stateRootIndexRefOffset:stateRootTTLRefOffset]) ||
		!pageRefReservedZero(payload[stateRootTTLRefOffset:stateRootRefsEnd]) {
		return StateRoot{}, fmt.Errorf("%w: header, version, or reserved bytes", ErrStateRootCorrupt)
	}
	freeChunkHint := uint32(0)
	if version == stateRootVersionV2 {
		if !allZero(payload[stateRootFreeHintOffset:stateRootChunkRefOffset]) {
			return StateRoot{}, fmt.Errorf("%w: version-two reserved bytes", ErrStateRootCorrupt)
		}
	} else {
		freeChunkHint = binary.LittleEndian.Uint32(payload[stateRootFreeHintOffset:stateRootChunkRefOffset])
	}
	var float64ScanHead, indexGroupHead PageRef
	if version == stateRootVersion || version == stateRootVersionV4 {
		if !pageRefReservedZero(payload[stateRootFloat64Offset:stateRootFloat64End]) ||
			version == stateRootVersionV4 && !allZero(payload[stateRootFloat64End:]) {
			return StateRoot{}, fmt.Errorf("%w: float64 scan head or reserved bytes", ErrStateRootCorrupt)
		}
		float64ScanHead = decodePageRef(payload[stateRootFloat64Offset:stateRootFloat64End])
		if version == stateRootVersion {
			if !pageRefReservedZero(payload[stateRootIndexGroupOffset:stateRootIndexGroupEnd]) ||
				!allZero(payload[stateRootIndexGroupEnd:]) {
				return StateRoot{}, fmt.Errorf("%w: index group head or reserved bytes", ErrStateRootCorrupt)
			}
			indexGroupHead = decodePageRef(payload[stateRootIndexGroupOffset:stateRootIndexGroupEnd])
		}
	} else if !allZero(payload[stateRootRefsEnd:]) {
		return StateRoot{}, fmt.Errorf("%w: legacy reserved bytes", ErrStateRootCorrupt)
	}
	root := StateRoot{
		StoreID:          header.StoreID,
		Generation:       header.Generation,
		PageSize:         header.PageSize,
		Options:          binary.LittleEndian.Uint32(payload[4:8]),
		DocumentCount:    binary.LittleEndian.Uint64(payload[8:16]),
		TTLCount:         binary.LittleEndian.Uint64(payload[16:24]),
		NextLogicalID:    binary.LittleEndian.Uint64(payload[24:32]),
		ChunkHighWater:   binary.LittleEndian.Uint32(payload[32:36]),
		LiveChunks:       binary.LittleEndian.Uint32(payload[36:40]),
		ChunkDocuments:   binary.LittleEndian.Uint32(payload[40:44]),
		IndexCount:       binary.LittleEndian.Uint32(payload[44:48]),
		IndexMaxDepth:    binary.LittleEndian.Uint32(payload[48:52]),
		IndexCatalogHash: binary.LittleEndian.Uint64(payload[52:60]),
		FreeChunkHint:    freeChunkHint,
		ChunkDirectory:   decodePageRef(payload[stateRootChunkRefOffset:stateRootKeyRefOffset]),
		KeyDirectory:     decodePageRef(payload[stateRootKeyRefOffset:stateRootIndexRefOffset]),
		IndexDirectory:   decodePageRef(payload[stateRootIndexRefOffset:stateRootTTLRefOffset]),
		TTLDirectory:     decodePageRef(payload[stateRootTTLRefOffset:stateRootRefsEnd]),
		Float64ScanHead:  float64ScanHead,
		IndexGroupHead:   indexGroupHead,
	}
	if err := validateStateRoot(root, fileEnd); err != nil {
		return StateRoot{}, fmt.Errorf("%w: %v", ErrStateRootCorrupt, err)
	}
	return root, nil
}

func encodePageRef(dst []byte, ref PageRef) {
	binary.LittleEndian.PutUint64(dst[0:8], ref.Offset)
	binary.LittleEndian.PutUint64(dst[8:16], ref.LogicalID)
	binary.LittleEndian.PutUint64(dst[16:24], ref.Generation)
	binary.LittleEndian.PutUint32(dst[24:28], ref.Length)
	dst[28] = byte(ref.Kind)
	dst[29] = ref.Flags
	binary.LittleEndian.PutUint16(dst[30:32], ref.Aux)
}

func decodePageRef(src []byte) PageRef {
	return PageRef{
		Offset:     binary.LittleEndian.Uint64(src[0:8]),
		LogicalID:  binary.LittleEndian.Uint64(src[8:16]),
		Generation: binary.LittleEndian.Uint64(src[16:24]),
		Length:     binary.LittleEndian.Uint32(src[24:28]),
		Kind:       PageKind(src[28]),
		Flags:      src[29],
		Aux:        binary.LittleEndian.Uint16(src[30:32]),
	}
}

func pageRefReservedZero(src []byte) bool {
	return binary.LittleEndian.Uint16(src[30:PageRefSize]) == 0
}

func validateStateRoot(root StateRoot, fileEnd uint64) error {
	if root.StoreID == ([16]byte{}) || root.Generation == 0 ||
		!validPhysicalPageSize(root.PageSize) || root.Options&^stateRootKnownOptions != 0 {
		return fmt.Errorf("%w: state identity, page size, or options", ErrInvalidWrite)
	}
	pageSize := uint64(root.PageSize)
	if fileEnd < uint64(superblockCopies)*pageSize || fileEnd > maxSuperblockFileOffset || fileEnd%pageSize != 0 {
		return fmt.Errorf("%w: state file high-water mark", ErrInvalidWrite)
	}
	hasCatalog := root.IndexCount != 0 ||
		root.Options&(StateOptionFloat64Columns|StateOptionSchema) != 0
	if root.ChunkDocuments == 0 || root.ChunkDocuments > 64 ||
		root.LiveChunks > root.ChunkHighWater || root.TTLCount > root.DocumentCount ||
		root.FreeChunkHint > root.ChunkHighWater || root.NextLogicalID <= StateRootLogicalID ||
		hasCatalog != (root.IndexCatalogHash != 0) {
		return fmt.Errorf("%w: state counts", ErrInvalidWrite)
	}
	if root.LiveChunks == 0 {
		if root.DocumentCount != 0 {
			return fmt.Errorf("%w: documents without chunks", ErrInvalidWrite)
		}
	} else if root.DocumentCount < uint64(root.LiveChunks) ||
		root.DocumentCount > uint64(root.LiveChunks)*uint64(root.ChunkDocuments) {
		return fmt.Errorf("%w: document/chunk counts", ErrInvalidWrite)
	}

	refs := [4]struct {
		ref      PageRef
		kind     PageKind
		required bool
		allowed  bool
	}{
		{root.ChunkDirectory, PageChunkDirectory, root.LiveChunks != 0, root.LiveChunks != 0},
		{root.KeyDirectory, PageKeyDirectory, root.DocumentCount != 0, root.DocumentCount != 0},
		{root.IndexDirectory, PageIndexDirectory, false, root.IndexCount != 0},
		{root.TTLDirectory, PageTTLDirectory, root.TTLCount != 0, root.TTLCount != 0},
	}
	for i := range refs {
		if err := validateStatePageRef(refs[i].ref, refs[i].kind, refs[i].required, root, fileEnd); err != nil {
			return err
		}
		if !refs[i].allowed && refs[i].ref != (PageRef{}) {
			return fmt.Errorf("%w: unneeded %d root", ErrInvalidWrite, refs[i].kind)
		}
		if refs[i].ref == (PageRef{}) {
			continue
		}
		for j := 0; j < i; j++ {
			if refs[j].ref != (PageRef{}) &&
				(refs[j].ref.LogicalID == refs[i].ref.LogicalID || refs[j].ref.Offset == refs[i].ref.Offset) {
				return fmt.Errorf("%w: duplicate root reference", ErrInvalidWrite)
			}
		}
	}
	if root.Float64ScanHead != (PageRef{}) {
		ref := root.Float64ScanHead
		length := uint64(ref.Length)
		if root.Options&StateOptionFloat64Columns == 0 ||
			ref.Kind != PageFloat64Catalog || ref.Flags != 0 || ref.Aux != 0 ||
			ref.Length != root.PageSize ||
			ref.Generation == 0 || ref.Generation > root.Generation ||
			ref.LogicalID <= StateRootLogicalID || ref.LogicalID >= root.NextLogicalID ||
			ref.Offset < uint64(superblockCopies)*pageSize || ref.Offset%pageSize != 0 ||
			length > fileEnd || ref.Offset > maxSuperblockFileOffset ||
			ref.Offset > fileEnd-length {
			return fmt.Errorf("%w: invalid float64 scan head", ErrInvalidWrite)
		}
		for i := range refs {
			if refs[i].ref != (PageRef{}) &&
				(refs[i].ref.LogicalID == ref.LogicalID || refs[i].ref.Offset == ref.Offset) {
				return fmt.Errorf("%w: duplicate float64 scan head", ErrInvalidWrite)
			}
		}
	}
	if root.IndexGroupHead != (PageRef{}) {
		ref := root.IndexGroupHead
		length := uint64(ref.Length)
		if root.IndexCount == 0 || root.DocumentCount == 0 ||
			ref.Kind != PageIndexGroupCatalog || ref.Flags != 0 || ref.Aux != 0 ||
			!validPhysicalPageSize(ref.Length) || ref.Length < root.PageSize ||
			ref.Length%root.PageSize != 0 ||
			ref.Generation == 0 || ref.Generation > root.Generation ||
			ref.LogicalID <= StateRootLogicalID || ref.LogicalID >= root.NextLogicalID ||
			ref.Offset < uint64(superblockCopies)*pageSize || ref.Offset%pageSize != 0 ||
			length > fileEnd || ref.Offset > maxSuperblockFileOffset ||
			ref.Offset > fileEnd-length {
			return fmt.Errorf("%w: invalid index group head", ErrInvalidWrite)
		}
		for i := range refs {
			if refs[i].ref != (PageRef{}) &&
				(refs[i].ref.LogicalID == ref.LogicalID || refs[i].ref.Offset == ref.Offset) {
				return fmt.Errorf("%w: duplicate index group head", ErrInvalidWrite)
			}
		}
		if root.Float64ScanHead != (PageRef{}) &&
			(root.Float64ScanHead.LogicalID == ref.LogicalID ||
				root.Float64ScanHead.Offset == ref.Offset) {
			return fmt.Errorf("%w: duplicate aggregate head", ErrInvalidWrite)
		}
	}
	return nil
}

func validateStatePageRef(ref PageRef, kind PageKind, required bool, root StateRoot, fileEnd uint64) error {
	if ref == (PageRef{}) {
		if required {
			return fmt.Errorf("%w: missing %d root", ErrInvalidWrite, kind)
		}
		return nil
	}
	pageSize := uint64(root.PageSize)
	if ref.Kind != kind || ref.Flags != 0 || ref.Aux != 0 || ref.Length != root.PageSize ||
		ref.Generation == 0 || ref.Generation > root.Generation ||
		ref.LogicalID <= StateRootLogicalID || ref.LogicalID >= root.NextLogicalID ||
		ref.Offset < uint64(superblockCopies)*pageSize || ref.Offset%pageSize != 0 ||
		ref.Offset > maxSuperblockFileOffset || ref.Offset > fileEnd-pageSize {
		return fmt.Errorf("%w: invalid %d root reference", ErrInvalidWrite, kind)
	}
	return nil
}
