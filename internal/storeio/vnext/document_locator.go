package vnext

import (
	"encoding/binary"

	"github.com/thesyncim/vibejson/internal/storeio"
)

// DocumentLocator is the compact physical half of an ordinary document
// reference. A lexical shard already supplies the expected logical block and
// chunk identity; the admitted document header and complete-key comparison
// verify both after acquisition. Keeping that identity out of every resident
// block-map cell avoids repeating the full 32-byte durable PageRef once per
// 64-row block.
//
// The exact twelve-byte encoding is:
//
//	offset in 4-KiB pages  43 bits
//	generation             48 bits
//	extent pages minus one  4 bits
//	document-group kind     1 bit
//
// It addresses 32 PiB at the fixed vnext quantum and 2^48 publications.
// Ordinary PageDocument and flag-free PageDocumentGroup extents use this form.
// A group carrying a derived sidecar is intentionally rejected and belongs in
// an explicit extended-reference component rather than taxing every block.
type DocumentLocator struct {
	Offset     uint64
	Generation uint64
	Length     uint32
	Grouped    bool
}

const (
	DocumentLocatorBytes = 12

	documentLocatorOffsetBits     = uint(43)
	documentLocatorGenerationBits = uint(48)
	documentLocatorGenerationLow  = uint(64 - documentLocatorOffsetBits)
	documentLocatorOffsetMask     = uint64(1<<documentLocatorOffsetBits - 1)
	documentLocatorGenerationMask = uint64(1<<documentLocatorGenerationBits - 1)
	documentLocatorHighGenMask    = uint32(1<<(documentLocatorGenerationBits-documentLocatorGenerationLow) - 1)
)

// EncodeDocumentLocator appends one canonical compact locator to dst. The
// caller retains the complete durable PageRef elsewhere while building; this
// form is valid only when the selecting shard supplies logical identity.
func EncodeDocumentLocator(dst []byte, ref storeio.PageRef) ([]byte, bool) {
	if ref.Offset%Quantum != 0 || ref.Offset/Quantum < 2 ||
		ref.Offset/Quantum > documentLocatorOffsetMask ||
		ref.Generation == 0 || ref.Generation > documentLocatorGenerationMask ||
		ref.LogicalID <= storeio.StateRootLogicalID ||
		ref.Length < Quantum || ref.Length > 16*Quantum ||
		ref.Length%Quantum != 0 || ref.Flags != 0 || ref.Aux != 0 ||
		ref.Kind != storeio.PageDocument && ref.Kind != storeio.PageDocumentGroup {
		return dst, false
	}
	// Grouped extents retain the current power-of-two physical geometry.
	if ref.Kind == storeio.PageDocumentGroup &&
		(ref.Length&(ref.Length-1) != 0) {
		return dst, false
	}
	start := len(dst)
	dst = append(dst, make([]byte, DocumentLocatorBytes)...)
	pages := ref.Offset / Quantum
	low := pages | (ref.Generation&((1<<documentLocatorGenerationLow)-1))<<documentLocatorOffsetBits
	high := uint32(ref.Generation >> documentLocatorGenerationLow)
	high |= (ref.Length/Quantum - 1) << 27
	if ref.Kind == storeio.PageDocumentGroup {
		high |= 1 << 31
	}
	binary.LittleEndian.PutUint64(dst[start:start+8], low)
	binary.LittleEndian.PutUint32(dst[start+8:start+12], high)
	return dst, true
}

// DecodeDocumentLocator decodes one exact twelve-byte locator. The returned
// value deliberately has no LogicalID: a future compact cache acquisition
// validates the selecting shard/block identity against the admitted document
// header instead of reconstructing an invented durable PageRef.
func DecodeDocumentLocator(src []byte) (DocumentLocator, bool) {
	if len(src) != DocumentLocatorBytes {
		return DocumentLocator{}, false
	}
	low := binary.LittleEndian.Uint64(src[0:8])
	high := binary.LittleEndian.Uint32(src[8:12])
	pages := low & documentLocatorOffsetMask
	generation := low >> documentLocatorOffsetBits
	generation |= uint64(high&documentLocatorHighGenMask) << documentLocatorGenerationLow
	span := (high>>27)&15 + 1
	if pages < 2 || generation == 0 {
		return DocumentLocator{}, false
	}
	return DocumentLocator{
		Offset:     pages * Quantum,
		Generation: generation,
		Length:     span * Quantum,
		Grouped:    high>>31 != 0,
	}, true
}

// PageRef combines the compact physical locator with the logical block
// identity supplied by its selecting lexical shard. It returns false for an
// invalid logical identity. This is the only reconstruction needed by the
// existing page cache; admission still compares every common-header field.
func (r DocumentLocator) PageRef(logicalID uint64) (storeio.PageRef, bool) {
	if logicalID <= storeio.StateRootLogicalID ||
		r.Offset%Quantum != 0 || r.Offset/Quantum < 2 ||
		r.Offset/Quantum > documentLocatorOffsetMask ||
		r.Generation == 0 || r.Generation > documentLocatorGenerationMask ||
		r.Length < Quantum || r.Length > 16*Quantum ||
		r.Length%Quantum != 0 ||
		r.Grouped && r.Length&(r.Length-1) != 0 {
		return storeio.PageRef{}, false
	}
	kind := storeio.PageDocument
	if r.Grouped {
		kind = storeio.PageDocumentGroup
	}
	return storeio.PageRef{
		Offset: r.Offset, LogicalID: logicalID, Generation: r.Generation,
		Length: r.Length, Kind: kind,
	}, true
}
