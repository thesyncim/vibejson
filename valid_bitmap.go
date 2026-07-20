package simdjson

import (
	"math/bits"
	"unsafe"
)

// The bitmap validator is a validation-only consumer of the stage-1 masks:
// whitespace disappears into the block masks, string interiors are skipped
// wholesale by the in-string mask, and the grammar machine touches only
// structural characters, string openers, and the first byte of each scalar.
// On indentation-heavy documents most bytes never reach the walk at all.
//
// Dense compact documents emit a position every few bytes, so a density sample
// of the leading blocks decides which engine runs (see
// validBitmapSampleCommit). The engine only reports validity; Validate re-runs
// the scalar validator for exact error offsets.
//
// Production uses packed structural positions in every supported build:
// Stage1ValidBlocks selects an architecture kernel when available and the
// portable SWAR classifier otherwise, then Stage2PositionsTrusted consumes
// each position once. docs/architecture.md records the routing rationale.

const (
	// validBitmapMinBytes keeps small and mid-size inputs on the recursive
	// scanner: below this even the sampling blocks would show up in their
	// latency.
	validBitmapMinBytes = 1 << 16

	// The density dispatch samples the first 2 KiB; the commit rule itself
	// lives in validBitmapSampleCommit. The whitespace leg requires at least
	// 25% outside whitespace and a ws:emit ratio above 3.5 (2ws >= 7emits).
	validBitmapSampleBlocks = 32
	validBitmapSampleMinWs  = 512

	// validBitmapSampleMinInStr is the string leg's floor: 9/16 of the
	// sampled bytes inside strings.
	validBitmapSampleMinInStr = validBitmapSampleBlocks * 64 * 9 / 16

	// Adjacent non-ASCII runs absorb short ASCII gaps to amortize SIMD UTF-8
	// setup without rereading large sparse spans.
	validUTF8CoalesceBlocks = 2
)

const (
	vbNumberDefault = iota
	vbNumberNine
	vbNumberShort
)

// validBitmapSampleCommit decides engine commitment from the byte classes
// of the 2 KiB sample: whitespace outside strings, grammar emits, in-string
// bytes, and escape targets inside strings. Three signals select document
// shapes that can skip enough scalar work:
//
//   - Spacey-structural: skipped whitespace must dominate emitted positions.
//   - String-heavy (the in-string leg): string interiors die inside the
//     masks, but skipped whitespace and string bytes together must still
//     outnumber emitted positions six to one.
//   - Escape-dense (the guard on the in-string leg): every escape target
//     costs a scalar check, so the string-heavy leg rejects more than one
//     escape target per sixteen string bytes.
func validBitmapSampleCommit(ws, emit, inStr, esc int) bool {
	if ws >= validBitmapSampleMinWs && ws*2 >= emit*7 {
		return true
	}
	return inStr >= validBitmapSampleMinInStr && esc*16 <= inStr && ws+inStr >= emit*6
}

func validBitmapNumberMode(inStr int) uint8 {
	if inStr >= validBitmapSampleMinInStr {
		return vbNumberShort
	}
	return vbNumberNine
}

// validBitmap reports strict validity through the packed stage-1 position
// stream and Go stage-2 grammar machine. Both backends are available in every
// supported build; decided=false means the density sample chose the recursive
// scanner instead and the result is then meaningless.
func validBitmap(src []byte) (valid, decided bool) {
	if len(src) < validBitmapSampleBlocks*64 {
		return false, false
	}
	return validPositionsStreamed(src)
}

// validBitmapEscapes validates every escape-target byte named in escInStr
// (offsets are block-relative bits over pos). \u needs four hex digits,
// read directly from src across block boundaries; a matched surrogate pair
// records its low half in skipEscape so the next block skips it. It reports
// whether any escape is malformed.
func validBitmapEscapes(src []byte, n, pos int, escInStr uint64, skipEscape *int) (bad bool) {
	if uint(n) > uint(len(src)) {
		return true
	}
	src = src[:n:n]
	base := sliceBase(src)
	for e := escInStr; e != 0; e &= e - 1 {
		j := pos + bits.TrailingZeros64(e)
		if uint(j) >= uint(len(src)) {
			return true
		}
		if j == *skipEscape {
			continue
		}
		switch src[j] {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		case 'u':
			if len(src)-j < 5 {
				return true
			}
			u, ok := hex4Fixed((*[4]byte)(unsafe.Add(base, j+1)))
			if !ok {
				return true
			}
			// Surrogate halves must pair, matching the scalar validator.
			if 0xDC00 <= u && u <= 0xDFFF {
				return true
			}
			if 0xD800 <= u && u <= 0xDBFF {
				if len(src)-j < 11 {
					return true
				}
				pair := (*[6]byte)(unsafe.Add(base, j+5))
				if pair[0] != '\\' || pair[1] != 'u' {
					return true
				}
				lo, ok := hex4Fixed((*[4]byte)(unsafe.Add(unsafe.Pointer(pair), 2)))
				if !ok || lo < 0xDC00 || lo > 0xDFFF {
					return true
				}
				*skipEscape = j + 6
			}
		default:
			return true
		}
	}
	return false
}

func hex4Fixed(src *[4]byte) (uint16, bool) {
	a := hexNibbleTable[src[0]]
	b := hexNibbleTable[src[1]]
	c := hexNibbleTable[src[2]]
	d := hexNibbleTable[src[3]]
	return uint16(a)<<12 | uint16(b)<<8 | uint16(c)<<4 | uint16(d), a|b|c|d < 0x10
}

func isJSONSpaceOrStructural(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', ',', ':', '{', '}', '[', ']':
		return true
	}
	return false
}
