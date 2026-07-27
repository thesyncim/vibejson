package vibejson

import (
	"math/bits"
	"unsafe"

	"github.com/thesyncim/vibejson/document"
	simdkernels "github.com/thesyncim/vibejson/x/kernels"
)

// The Go SIMD index engine writes the private 16-byte tape layout directly.
// These assertions pin that coupling at compile time; escaped flags are the
// only facts applied after the forward position machine completes a chunk.

// The machine hardcodes the tape layout; these equalities pin the
// private packing to the constants it writes.
const (
	_ = uint(unsafe.Sizeof(IndexEntry{}) - 16)
	_ = uint(16 - unsafe.Sizeof(IndexEntry{}))
	_ = uint(unsafe.Offsetof(IndexEntry{}.Start) - 0)
	_ = uint(unsafe.Offsetof(IndexEntry{}.End) - 4)
	_ = uint(unsafe.Offsetof(IndexEntry{}.Next) - 8)
	_ = uint(unsafe.Offsetof(IndexEntry{}.Info) - 12)
	_ = uint(uint32(document.Object)<<InfoKindShift - simdkernels.Stage2IndexInfoObject)
	_ = uint(simdkernels.Stage2IndexInfoObject - uint32(document.Object)<<InfoKindShift)
	_ = uint(uint32(document.Array)<<InfoKindShift - simdkernels.Stage2IndexInfoArray)
	_ = uint(simdkernels.Stage2IndexInfoArray - uint32(document.Array)<<InfoKindShift)
	_ = uint(uint32(document.String)<<InfoKindShift - simdkernels.Stage2IndexInfoString)
	_ = uint(simdkernels.Stage2IndexInfoString - uint32(document.String)<<InfoKindShift)
	_ = uint(uint32(document.Number)<<InfoKindShift - simdkernels.Stage2IndexInfoNumber)
	_ = uint(simdkernels.Stage2IndexInfoNumber - uint32(document.Number)<<InfoKindShift)
	_ = uint(uint32(document.Bool)<<InfoKindShift - simdkernels.Stage2IndexInfoBool)
	_ = uint(simdkernels.Stage2IndexInfoBool - uint32(document.Bool)<<InfoKindShift)
	_ = uint(uint32(document.Null)<<InfoKindShift - simdkernels.Stage2IndexInfoNull)
	_ = uint(simdkernels.Stage2IndexInfoNull - uint32(document.Null)<<InfoKindShift)
	_ = uint(uint32(TapeFlagKey)<<InfoFlagsShift - simdkernels.Stage2IndexKeyFlag)
	_ = uint(simdkernels.Stage2IndexKeyFlag - uint32(TapeFlagKey)<<InfoFlagsShift)
	_ = uint(uint32(TapeFlagInt)<<InfoFlagsShift - simdkernels.Stage2IndexIntFlag)
	_ = uint(simdkernels.Stage2IndexIntFlag - uint32(TapeFlagInt)<<InfoFlagsShift)
	_ = uint(fastWalkMaxDepth - simdkernels.Stage2IndexMaxDepth)
	_ = uint(simdkernels.Stage2IndexMaxDepth - fastWalkMaxDepth)
)

// indexBitmapMaxBytes bounds the engine to documents whose member counts
// cannot overflow the info word's 26-bit count field: a member costs at
// least two bytes (itself and a separator or closer), so any container
// in a document below 2^27 bytes holds fewer than 2^26 members. Larger
// documents keep the portable builder, which carries the explicit check.
const indexBitmapMaxBytes = 1 << 27

// BuildIndexBitmap is retained as the internal differential-test entry point.
func BuildIndexBitmap(src []byte, storage []IndexEntry) (entries []IndexEntry, ok bool) {
	return buildIndexPositions(src, storage)
}

// indexBitmapFinish applies escaped flags from the per-block masks. Scalar
// bodies, exact string ends, and containers are completed by the forward Go
// machine while their source bytes are hot.
type indexBitmapEscapeState struct {
	entry int
	seen  bool
}

func indexBitmapFinish(entries []IndexEntry, toOff uint64,
	recs *[simdkernels.Stage1ChunkBlocks]simdkernels.Stage1Rec, chunkPos, cnt int, escapeBlocks uint32, escapeState *indexBitmapEscapeState) bool {
	// Escaped flags: InStr partitions escape targets into string runs. Only
	// the first target in each run needs an entry lookup; the flag is already
	// set for every later escape in that string. The carried state also spans
	// chunks, while the previous escaped entry narrows each binary search.
	total, blockCursor := int(toOff>>4), 0
	for escapeBlocks != 0 {
		i := bits.TrailingZeros32(escapeBlocks)
		escapeBlocks &= escapeBlocks - 1
		if escapeState.seen {
			for ; blockCursor < i; blockCursor++ {
				if recs[blockCursor].InStr != ^uint64(0) {
					escapeState.seen = false
					break
				}
			}
		}
		esc := recs[i].EscInStr
		inStr := recs[i].InStr
		bitCursor := 0
		for ; esc != 0; esc &= esc - 1 {
			bit := bits.TrailingZeros64(esc)
			throughTarget := ^uint64(0) >> uint(63-bit)
			fromCursor := ^uint64(0) << uint(bitCursor)
			if (^inStr)&throughTarget&fromCursor != 0 {
				escapeState.seen = false
			}
			if escapeState.seen {
				bitCursor = bit + 1
				continue
			}

			p := uint32(chunkPos + i*64 + bit)
			lo, hi := escapeState.entry+1, total
			for lo < hi {
				mid := int(uint(lo+hi) >> 1)
				if entries[mid].Start < p {
					lo = mid + 1
				} else {
					hi = mid
				}
			}
			entryIndex := lo - 1
			if entryIndex < 0 {
				return false
			}
			e := &entries[entryIndex]
			if e.Kind() != document.String {
				return false
			}
			e.Info |= uint32(TapeFlagEscaped) << InfoFlagsShift
			escapeState.entry = entryIndex
			escapeState.seen = true
			bitCursor = bit + 1
		}
		if (^inStr)&(^uint64(0)<<uint(bitCursor)) != 0 {
			escapeState.seen = false
		}
		blockCursor = i + 1
	}
	if escapeState.seen {
		for ; blockCursor < cnt; blockCursor++ {
			if recs[blockCursor].InStr != ^uint64(0) {
				escapeState.seen = false
				break
			}
		}
	}
	return true
}
