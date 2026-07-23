package storeio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
)

const (
	// SuperblockSize is the checksummed prefix written into either of the two
	// fixed root pages. The rest of each physical page stays reserved so a torn
	// root write cannot overlap the alternate copy.
	SuperblockSize          = 128
	superblockMagic         = "SJROOT01"
	superblockVersion       = 1
	superblockCopies        = 2
	superblockKnownFlags    = uint32(0)
	maxSuperblockFileOffset = uint64(^uint64(0) >> 1)
	physicalPageQuantum     = uint32(4096)
)

var (
	// ErrSuperblockCorrupt reports an invalid root header, checksum, extent, or
	// referenced root page.
	ErrSuperblockCorrupt = errors.New("slopjson: corrupt Store superblock")
	// ErrSuperblockNotFound reports that neither fixed root copy is valid.
	ErrSuperblockNotFound = errors.New("slopjson: no valid Store superblock")
	// ErrSuperblockConflict reports two individually valid root copies that do
	// not belong to one monotonic Store history.
	ErrSuperblockConflict = errors.New("slopjson: conflicting Store superblocks")
	// ErrRecoveryBufferTooSmall reports caller scratch that cannot hold one
	// referenced state-root page. Recovery never silently selects an older root
	// merely because caller memory is undersized.
	ErrRecoveryBufferTooSmall = errors.New("slopjson: Store recovery buffer too small")

	pageChecksumTable = crc32.MakeTable(crc32.Castagnoli)
)

// Superblock is the failure-atomic root of one attached Store generation. Two
// copies occupy the first two physical pages and alternate by generation. A
// copy becomes authoritative only after its referenced state page has passed a
// data-integrity barrier and the superblock itself has passed the final one.
//
// StoreID prevents a valid root page copied from another file from joining the
// history. FileEnd is the exclusive allocated high-water mark. StateOffset and
// FreeOffset name immutable, page-aligned roots; a zero FreeLength means no
// free-page tree has been published yet.
type Superblock struct {
	StoreID       [16]byte
	Generation    uint64
	StateOffset   uint64
	StateLength   uint32
	StateChecksum uint32
	FileEnd       uint64
	FreeOffset    uint64
	FreeLength    uint32
	FreeChecksum  uint32
	PageSize      uint32
	Flags         uint32
}

// PageChecksum returns the deterministic CRC32C used by attached-Store pages.
// SIMD builds may fold large pages with carry-less multiplication; all other
// builds use Go's hardware-dispatched implementation.
func PageChecksum(data []byte) uint32 { return pageChecksum(data) }

// SuperblockOffset returns the fixed slot selected by generation. Generation
// one uses page zero, generation two page one, and later generations alternate.
func SuperblockOffset(generation uint64, pageSize uint32) (int64, error) {
	if generation == 0 || !validPhysicalPageSize(pageSize) {
		return 0, fmt.Errorf("%w: generation=%d page-size=%d", ErrInvalidWrite, generation, pageSize)
	}
	return int64((generation-1)&(superblockCopies-1)) * int64(pageSize), nil
}

// EncodeSuperblock writes one deterministic fixed-size root record into dst.
// dst must provide SuperblockSize bytes; no allocation is performed.
func EncodeSuperblock(dst []byte, root Superblock) ([]byte, error) {
	if len(dst) < SuperblockSize {
		return nil, fmt.Errorf("%w: superblock buffer has %d bytes", ErrInvalidWrite, len(dst))
	}
	if err := validateSuperblock(root); err != nil {
		return nil, err
	}
	dst = dst[:SuperblockSize]
	clear(dst)
	copy(dst[0:8], superblockMagic)
	binary.LittleEndian.PutUint32(dst[8:12], superblockVersion)
	binary.LittleEndian.PutUint32(dst[12:16], SuperblockSize)
	binary.LittleEndian.PutUint32(dst[16:20], root.Flags)
	binary.LittleEndian.PutUint32(dst[20:24], root.PageSize)
	binary.LittleEndian.PutUint64(dst[24:32], root.Generation)
	binary.LittleEndian.PutUint64(dst[32:40], ^root.Generation)
	binary.LittleEndian.PutUint64(dst[40:48], root.StateOffset)
	binary.LittleEndian.PutUint32(dst[48:52], root.StateLength)
	binary.LittleEndian.PutUint32(dst[52:56], root.StateChecksum)
	binary.LittleEndian.PutUint32(dst[56:60], ^root.StateChecksum)
	binary.LittleEndian.PutUint64(dst[64:72], root.FileEnd)
	binary.LittleEndian.PutUint64(dst[72:80], root.FreeOffset)
	binary.LittleEndian.PutUint32(dst[80:84], root.FreeLength)
	binary.LittleEndian.PutUint32(dst[84:88], root.FreeChecksum)
	binary.LittleEndian.PutUint32(dst[88:92], ^root.FreeChecksum)
	copy(dst[96:112], root.StoreID[:])
	checksum := PageChecksum(dst[:120])
	binary.LittleEndian.PutUint32(dst[120:124], checksum)
	binary.LittleEndian.PutUint32(dst[124:128], ^checksum)
	return dst, nil
}

// DecodeSuperblock validates and decodes one fixed root record. Unknown flags,
// non-zero reserved bytes, impossible extents, and checksum mismatches fail
// closed before any offset is trusted.
func DecodeSuperblock(src []byte) (Superblock, error) {
	if len(src) < SuperblockSize {
		return Superblock{}, fmt.Errorf("%w: short record", ErrSuperblockCorrupt)
	}
	src = src[:SuperblockSize]
	if string(src[0:8]) != superblockMagic {
		return Superblock{}, fmt.Errorf("%w: magic", ErrSuperblockCorrupt)
	}
	if binary.LittleEndian.Uint32(src[8:12]) != superblockVersion ||
		binary.LittleEndian.Uint32(src[12:16]) != SuperblockSize {
		return Superblock{}, fmt.Errorf("%w: version or size", ErrSuperblockCorrupt)
	}
	checksum := binary.LittleEndian.Uint32(src[120:124])
	if binary.LittleEndian.Uint32(src[124:128]) != ^checksum || PageChecksum(src[:120]) != checksum {
		return Superblock{}, fmt.Errorf("%w: checksum", ErrSuperblockCorrupt)
	}
	if !allZero(src[60:64]) || !allZero(src[92:96]) || !allZero(src[112:120]) {
		return Superblock{}, fmt.Errorf("%w: reserved bytes", ErrSuperblockCorrupt)
	}
	root := Superblock{
		Generation:    binary.LittleEndian.Uint64(src[24:32]),
		StateOffset:   binary.LittleEndian.Uint64(src[40:48]),
		StateLength:   binary.LittleEndian.Uint32(src[48:52]),
		StateChecksum: binary.LittleEndian.Uint32(src[52:56]),
		FileEnd:       binary.LittleEndian.Uint64(src[64:72]),
		FreeOffset:    binary.LittleEndian.Uint64(src[72:80]),
		FreeLength:    binary.LittleEndian.Uint32(src[80:84]),
		FreeChecksum:  binary.LittleEndian.Uint32(src[84:88]),
		Flags:         binary.LittleEndian.Uint32(src[16:20]),
		PageSize:      binary.LittleEndian.Uint32(src[20:24]),
	}
	copy(root.StoreID[:], src[96:112])
	if binary.LittleEndian.Uint64(src[32:40]) != ^root.Generation ||
		binary.LittleEndian.Uint32(src[56:60]) != ^root.StateChecksum ||
		binary.LittleEndian.Uint32(src[88:92]) != ^root.FreeChecksum {
		return Superblock{}, fmt.Errorf("%w: complement", ErrSuperblockCorrupt)
	}
	if err := validateSuperblock(root); err != nil {
		return Superblock{}, fmt.Errorf("%w: %v", ErrSuperblockCorrupt, err)
	}
	return root, nil
}

type superblockCandidate struct {
	root Superblock
	slot int
}

// SelectSuperblock returns the newest valid root header. It deliberately does
// not read the referenced state page; use RecoverSuperblock for crash recovery.
// If one copy is torn or corrupt, the other remains eligible.
func SelectSuperblock(first, second []byte) (Superblock, int, error) {
	candidates, _, err := orderedSuperblocks(first, second)
	if err != nil {
		return Superblock{}, -1, err
	}
	return candidates[0].root, candidates[0].slot, nil
}

// RecoverSuperblock reads both fixed root pages and returns the newest one
// whose referenced state and free-tree root bytes exist and match their CRC32C.
// pageScratch must be at least pageSize bytes and is reused for every check. A
// corrupt newest root page falls back to the preceding valid generation.
func RecoverSuperblock(file *os.File, pageSize uint32, pageScratch []byte) (Superblock, int, error) {
	root, _, slot, err := recoverRoots(file, pageSize, pageScratch, false)
	return root, slot, err
}

// RecoverStateRoot applies the same newest-to-oldest selection as
// RecoverSuperblock, then also validates the common page envelope, state-root
// schema, Store identity, generation, and every top-level page reference. A
// semantically torn newest state falls back to the preceding generation even
// when its outer checksum was recomputed. pageScratch is caller-owned and no
// allocation is performed on success.
func RecoverStateRoot(file *os.File, pageSize uint32, pageScratch []byte) (Superblock, StateRoot, int, error) {
	return recoverRoots(file, pageSize, pageScratch, true)
}

func recoverRoots(file *os.File, pageSize uint32, pageScratch []byte, decodeState bool) (Superblock, StateRoot, int, error) {
	if file == nil || !validPhysicalPageSize(pageSize) {
		return Superblock{}, StateRoot{}, -1, fmt.Errorf("%w: invalid recovery file or page size", ErrInvalidWrite)
	}
	if uint64(len(pageScratch)) < uint64(pageSize) {
		return Superblock{}, StateRoot{}, -1, fmt.Errorf("%w: have=%d need=%d", ErrRecoveryBufferTooSmall, len(pageScratch), pageSize)
	}
	var headers [superblockCopies * SuperblockSize]byte
	for slot := 0; slot < superblockCopies; slot++ {
		buf := headers[slot*SuperblockSize : (slot+1)*SuperblockSize]
		n, err := file.ReadAt(buf, int64(slot)*int64(pageSize))
		if err != nil && !errors.Is(err, io.EOF) {
			return Superblock{}, StateRoot{}, -1, err
		}
		if n < len(buf) {
			clear(buf[n:])
		}
	}
	candidates, count, err := orderedSuperblocks(headers[:SuperblockSize], headers[SuperblockSize:])
	if err != nil {
		return Superblock{}, StateRoot{}, -1, err
	}
	info, err := file.Stat()
	if err != nil {
		return Superblock{}, StateRoot{}, -1, err
	}
	if info.Size() < 0 {
		return Superblock{}, StateRoot{}, -1, ErrSuperblockNotFound
	}
	fileSize := uint64(info.Size())
	for i := 0; i < count; i++ {
		candidate := candidates[i]
		root := candidate.root
		if root.PageSize != pageSize || root.FileEnd > fileSize {
			continue
		}
		stateOK, readErr := readCheckedPage(file, root.StateOffset, root.StateLength, root.StateChecksum, pageScratch)
		if readErr != nil {
			return Superblock{}, StateRoot{}, -1, readErr
		}
		if !stateOK {
			continue
		}
		var state StateRoot
		if decodeState {
			state, err = DecodeStateRootPage(pageScratch[:root.StateLength], root.FileEnd)
			if err != nil || state.StoreID != root.StoreID || state.Generation != root.Generation ||
				state.PageSize != root.PageSize || stateRootReferencesOffset(state, root.StateOffset) ||
				root.FreeLength != 0 && stateRootReferencesOffset(state, root.FreeOffset) {
				continue
			}
			refsOK, refsErr := readStateRootRefs(file, state, pageScratch)
			if refsErr != nil {
				return Superblock{}, StateRoot{}, -1, refsErr
			}
			if !refsOK {
				continue
			}
		}
		if root.FreeLength != 0 {
			freeOK, freeErr := readCheckedPage(file, root.FreeOffset, root.FreeLength, root.FreeChecksum, pageScratch)
			if freeErr != nil {
				return Superblock{}, StateRoot{}, -1, freeErr
			}
			if !freeOK {
				continue
			}
			if decodeState {
				free, freeOpenErr := OpenFreeDirectoryPage(pageScratch[:root.FreeLength], root.FileEnd, state.NextLogicalID)
				freeHeader := free.Header()
				if freeOpenErr != nil || freeHeader.StoreID != root.StoreID ||
					freeHeader.PageSize != root.PageSize || freeHeader.Generation > root.Generation ||
					freeHeader.LogicalID == StateRootLogicalID {
					continue
				}
			}
		}
		return root, state, candidate.slot, nil
	}
	return Superblock{}, StateRoot{}, -1, ErrSuperblockNotFound
}

func stateRootReferencesOffset(root StateRoot, offset uint64) bool {
	return root.ChunkDirectory.Offset == offset || root.KeyDirectory.Offset == offset ||
		root.IndexDirectory.Offset == offset || root.TTLDirectory.Offset == offset ||
		root.Float64ScanHead.Offset == offset || root.IndexGroupHead.Offset == offset
}

func readStateRootRefs(file *os.File, root StateRoot, scratch []byte) (bool, error) {
	refs := [...]PageRef{root.ChunkDirectory, root.KeyDirectory, root.IndexDirectory, root.TTLDirectory}
	for _, ref := range refs {
		if ref == (PageRef{}) {
			continue
		}
		buf := scratch[:ref.Length]
		n, err := file.ReadAt(buf, int64(ref.Offset))
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		if n != len(buf) {
			return false, nil
		}
		header, _, openErr := OpenPage(buf)
		if openErr != nil || header.StoreID != root.StoreID || header.PageSize != root.PageSize ||
			header.Kind != ref.Kind || header.LogicalID != ref.LogicalID || header.Generation != ref.Generation {
			return false, nil
		}
	}
	return true, nil
}

func readCheckedPage(file *os.File, offset uint64, length, checksum uint32, scratch []byte) (bool, error) {
	buf := scratch[:int(length)]
	n, err := file.ReadAt(buf, int64(offset))
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return n == len(buf) && PageChecksum(buf) == checksum, nil
}

func orderedSuperblocks(first, second []byte) ([superblockCopies]superblockCandidate, int, error) {
	var candidates [superblockCopies]superblockCandidate
	firstRoot, firstErr := DecodeSuperblock(first)
	secondRoot, secondErr := DecodeSuperblock(second)
	if firstErr == nil && (firstRoot.Generation-1)&(superblockCopies-1) != 0 {
		firstErr = fmt.Errorf("%w: generation in wrong slot", ErrSuperblockCorrupt)
	}
	if secondErr == nil && (secondRoot.Generation-1)&(superblockCopies-1) != 1 {
		secondErr = fmt.Errorf("%w: generation in wrong slot", ErrSuperblockCorrupt)
	}
	count := 0
	if firstErr == nil {
		candidates[count] = superblockCandidate{root: firstRoot, slot: 0}
		count++
	}
	if secondErr == nil {
		candidates[count] = superblockCandidate{root: secondRoot, slot: 1}
		count++
	}
	if count == 0 {
		return candidates, 0, fmt.Errorf("%w: slot 0: %v; slot 1: %v", ErrSuperblockNotFound, firstErr, secondErr)
	}
	if count == 2 {
		if candidates[0].root.StoreID != candidates[1].root.StoreID ||
			candidates[0].root.PageSize != candidates[1].root.PageSize {
			return candidates, 0, ErrSuperblockConflict
		}
		if candidates[1].root.Generation > candidates[0].root.Generation {
			candidates[0], candidates[1] = candidates[1], candidates[0]
		}
	}
	return candidates, count, nil
}

func validateSuperblock(root Superblock) error {
	if root.Generation == 0 || root.Flags&^superblockKnownFlags != 0 || !validPhysicalPageSize(root.PageSize) {
		return fmt.Errorf("%w: invalid generation, flags, or page size", ErrInvalidWrite)
	}
	if root.StoreID == ([16]byte{}) {
		return fmt.Errorf("%w: zero Store id", ErrInvalidWrite)
	}
	pageSize := uint64(root.PageSize)
	dataStart := uint64(superblockCopies) * pageSize
	if root.FileEnd < dataStart || root.FileEnd > maxSuperblockFileOffset || root.FileEnd%pageSize != 0 {
		return fmt.Errorf("%w: file high-water mark", ErrInvalidWrite)
	}
	if !validRootExtent(root.StateOffset, root.StateLength, root.FileEnd, root.PageSize, true) {
		return fmt.Errorf("%w: state-root extent", ErrInvalidWrite)
	}
	if !validRootExtent(root.FreeOffset, root.FreeLength, root.FileEnd, root.PageSize, false) {
		return fmt.Errorf("%w: free-root extent", ErrInvalidWrite)
	}
	if root.FreeLength == 0 && root.FreeChecksum != 0 {
		return fmt.Errorf("%w: checksum without free root", ErrInvalidWrite)
	}
	if root.FreeLength != 0 && root.FreeOffset == root.StateOffset {
		return fmt.Errorf("%w: overlapping state and free roots", ErrInvalidWrite)
	}
	return nil
}

func validRootExtent(offset uint64, length uint32, fileEnd uint64, pageSize uint32, required bool) bool {
	if length == 0 {
		return !required && offset == 0
	}
	size := uint64(pageSize)
	return length <= pageSize && offset >= uint64(superblockCopies)*size && offset%size == 0 &&
		offset <= maxSuperblockFileOffset && offset <= fileEnd-size
}

func validPhysicalPageSize(pageSize uint32) bool {
	return pageSize >= physicalPageQuantum && pageSize&(pageSize-1) == 0
}

func allZero(src []byte) bool {
	var combined byte
	for _, value := range src {
		combined |= value
	}
	return combined == 0
}
