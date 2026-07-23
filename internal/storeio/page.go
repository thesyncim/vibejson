package storeio

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	// PageHeaderSize is the fixed, architecture-independent prefix shared by
	// every attached-Store page.
	PageHeaderSize = 64
	// PageTrailerSize stores CRC32C and its complement at the end of the
	// physical page. Keeping the trailer fixed makes the payload naturally
	// aligned and lets checksum code consume one contiguous prefix.
	PageTrailerSize = 8

	pageMagic   = "SJPAGE01"
	pageVersion = uint16(1)
)

// ErrPageCorrupt reports a malformed, truncated, or checksum-invalid common
// Store page.
var ErrPageCorrupt = errors.New("slopjson: corrupt Store page")

// PageKind identifies the pointer-free payload schema inside a common page.
// Values are durable format identifiers, not Go type ordinals.
type PageKind uint8

const (
	PageStateRoot PageKind = iota + 1
	PageDocument
	PageOverflow
	PageChunkDirectory
	PageKeyDirectory
	PageIndexDirectory
	PageTTLDirectory
	PageFreeDirectory
	PageIndexPosting
	PageDocumentGroup
	PageFloat64Group
	PageFloat64Catalog
	PageFloat64Stripe
	PageIndexGroupCatalog
)

// PageHeader is the decoded identity of one immutable physical page. StoreID
// prevents cross-file grafting, LogicalID remains stable across copy-on-write
// replacement, and Generation identifies the version of that logical page.
// PayloadLength excludes the fixed header, zero padding, and checksum trailer.
type PageHeader struct {
	StoreID       [16]byte
	Generation    uint64
	LogicalID     uint64
	PageSize      uint32
	PayloadLength uint32
	Kind          PageKind
	Flags         uint8
}

// InitPage clears one caller-owned physical page, writes its canonical header,
// and returns the exact payload window for the caller to fill. The returned
// slice aliases dst. Call SealPage only after filling it. No allocation is
// performed.
func InitPage(dst []byte, header PageHeader) ([]byte, error) {
	if err := validatePageHeader(header); err != nil {
		return nil, err
	}
	if uint64(len(dst)) < uint64(header.PageSize) {
		return nil, fmt.Errorf("%w: page buffer has %d bytes, need %d", ErrInvalidWrite, len(dst), header.PageSize)
	}
	page := dst[:int(header.PageSize)]
	clear(page)
	copy(page[0:8], pageMagic)
	binary.LittleEndian.PutUint16(page[8:10], pageVersion)
	binary.LittleEndian.PutUint16(page[10:12], PageHeaderSize)
	page[12] = byte(header.Kind)
	page[13] = header.Flags
	binary.LittleEndian.PutUint32(page[16:20], header.PageSize)
	binary.LittleEndian.PutUint32(page[20:24], header.PayloadLength)
	binary.LittleEndian.PutUint64(page[24:32], header.Generation)
	binary.LittleEndian.PutUint64(page[32:40], header.LogicalID)
	copy(page[40:56], header.StoreID[:])
	end := PageHeaderSize + int(header.PayloadLength)
	return page[PageHeaderSize:end:end], nil
}

// SealPage validates a page initialized by InitPage and writes its CRC32C
// trailer. Bytes outside the declared payload must remain zero, preventing
// stale buffer contents from becoming durable or leaking into deterministic
// images. No allocation is performed.
func SealPage(page []byte) (uint32, error) {
	return sealPage(page, true)
}

// sealInitializedPage is the internal fast path for encoders that call
// InitPage and write only through its capacity-clipped payload. InitPage has
// already cleared the padding, so rescanning it would add a second full-page
// pass without strengthening that construction path.
func sealInitializedPage(page []byte) (uint32, error) {
	return sealPage(page, false)
}

func sealPage(page []byte, validatePadding bool) (uint32, error) {
	header, ok := decodePageHeader(page)
	if !ok || uint64(len(page)) < uint64(header.PageSize) {
		return 0, fmt.Errorf("%w: invalid page header", ErrInvalidWrite)
	}
	page = page[:int(header.PageSize)]
	payloadEnd := PageHeaderSize + int(header.PayloadLength)
	trailer := len(page) - PageTrailerSize
	if !allZero(page[14:16]) || !allZero(page[56:64]) ||
		validatePadding && !allZero(page[payloadEnd:trailer]) {
		return 0, fmt.Errorf("%w: non-zero page reserved bytes or padding", ErrInvalidWrite)
	}
	checksum := PageChecksum(page[:trailer])
	binary.LittleEndian.PutUint32(page[trailer:trailer+4], checksum)
	binary.LittleEndian.PutUint32(page[trailer+4:], ^checksum)
	return checksum, nil
}

// OpenPage verifies and decodes one physical page, returning a capacity-clipped
// payload view that borrows src. Unknown kinds or flags, non-canonical reserved
// bytes, impossible lengths, and checksum failures are rejected before a
// payload byte is trusted. Padding is checksum-covered but deliberately not
// scanned a second time; InitPage and SealPage enforce zero padding on writers,
// and the clipped view makes padding inaccessible to readers. No allocation is
// performed on success.
func OpenPage(src []byte) (PageHeader, []byte, error) {
	header, ok := decodePageHeader(src)
	if !ok || uint64(len(src)) < uint64(header.PageSize) {
		return PageHeader{}, nil, fmt.Errorf("%w: header", ErrPageCorrupt)
	}
	page := src[:int(header.PageSize)]
	payloadEnd := PageHeaderSize + int(header.PayloadLength)
	trailer := len(page) - PageTrailerSize
	checksum := binary.LittleEndian.Uint32(page[trailer : trailer+4])
	if binary.LittleEndian.Uint32(page[trailer+4:]) != ^checksum ||
		PageChecksum(page[:trailer]) != checksum {
		return PageHeader{}, nil, fmt.Errorf("%w: checksum", ErrPageCorrupt)
	}
	if !allZero(page[14:16]) || !allZero(page[56:64]) {
		return PageHeader{}, nil, fmt.Errorf("%w: reserved bytes", ErrPageCorrupt)
	}
	return header, page[PageHeaderSize:payloadEnd:payloadEnd], nil
}

func decodePageHeader(src []byte) (PageHeader, bool) {
	if len(src) < PageHeaderSize || string(src[0:8]) != pageMagic ||
		binary.LittleEndian.Uint16(src[8:10]) != pageVersion ||
		binary.LittleEndian.Uint16(src[10:12]) != PageHeaderSize {
		return PageHeader{}, false
	}
	header := PageHeader{
		Kind:          PageKind(src[12]),
		Flags:         src[13],
		PageSize:      binary.LittleEndian.Uint32(src[16:20]),
		PayloadLength: binary.LittleEndian.Uint32(src[20:24]),
		Generation:    binary.LittleEndian.Uint64(src[24:32]),
		LogicalID:     binary.LittleEndian.Uint64(src[32:40]),
	}
	copy(header.StoreID[:], src[40:56])
	return header, validatePageHeader(header) == nil
}

func validatePageHeader(header PageHeader) error {
	if header.StoreID == ([16]byte{}) || header.Generation == 0 || header.LogicalID == 0 {
		return fmt.Errorf("%w: zero page identity", ErrInvalidWrite)
	}
	if !validPageKind(header.Kind) || !validPageFlags(header.Kind, header.Flags) {
		return fmt.Errorf("%w: page kind or flags", ErrInvalidWrite)
	}
	if !validPhysicalPageSize(header.PageSize) ||
		uint64(header.PayloadLength) > uint64(header.PageSize)-PageHeaderSize-PageTrailerSize {
		return fmt.Errorf("%w: page or payload size", ErrInvalidWrite)
	}
	return nil
}

func validPageKind(kind PageKind) bool {
	return kind >= PageStateRoot && kind <= PageIndexGroupCatalog
}

func validPageFlags(kind PageKind, flags uint8) bool {
	if kind == PageDocumentGroup {
		encoded := uint16(flags)
		return encoded&^documentGroupKnownFlags == 0 &&
			(encoded&DocumentGroupFlagFloat64Sidecar != 0 || encoded == 0)
	}
	return flags == 0
}
