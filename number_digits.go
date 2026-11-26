package vibejson

import (
	"encoding/binary"
	"math/bits"
	"unsafe"

	simdkernels "github.com/thesyncim/vibejson/simd"
)

const (
	digitLower  = uint64(0x3030303030303030)
	digitUpper  = uint64(0x4646464646464646)
	digitHigh   = uint64(0x8080808080808080)
	digitLower4 = uint32(0x30303030)
	digitUpper4 = uint32(0x46464646)
	digitHigh4  = uint32(0x80808080)
)

// numericBitSize keeps the representation query inside the number-source
// boundary. Generic numeric decoders use it only for concrete integer and
// float instantiations, so the compiler folds the size after inlining.
func numericBitSize[T any]() int {
	var value T
	return int(unsafe.Sizeof(value)) * 8
}

func nonDigitMask8(x uint64) uint64 {
	return ((x + digitUpper) | (x - digitLower)) & digitHigh
}

func nonDigitMask4(x uint32) uint32 {
	return ((x + digitUpper4) | (x - digitLower4)) & digitHigh4
}

// scanDigitsLong advances over a decimal digit run after the caller has proved
// that lanes 0 through 7 are digits. It avoids repeating the short-run gate on
// fraction paths that already loaded lane 7.
func scanDigitsLong(src []byte, i int) int {
	rem := src[i:]
	for len(rem) >= 8 {
		invalid := nonDigitMask8(binary.LittleEndian.Uint64(rem))
		if invalid != 0 {
			return i + bits.TrailingZeros64(invalid)/8
		}
		rem = rem[8:]
		i += 8
	}
	for _, c := range rem {
		if !IsDigit(c) {
			break
		}
		i++
	}
	return i
}

func all16Digits(src []byte) bool {
	return simdkernels.All16Digits((*[16]byte)(src))
}

func all8Digits(src []byte) bool {
	return simdkernels.All8Digits((*[8]byte)(src))
}

// Provenance: ALGO-DIGITS-001.
// parse8Digits reduces eight ASCII digits with the same cited SWAR reduction
// as simd.Parse8Digits. It is the small-token companion to the architecture
// SIMD 16-digit parser; see docs/provenance.md.
func parse8Digits(src []byte) uint64 {
	return simdkernels.Parse8Digits((*[8]byte)(src))
}

func parse8DigitsWord(x uint64) uint64 {
	x = (x & 0x0f0f0f0f0f0f0f0f) * 2561 >> 8
	x = (x & 0x00ff00ff00ff00ff) * 6553601 >> 16
	return (x & 0x0000ffff0000ffff) * 42949672960001 >> 32
}

// Little-endian word images of the JSON literals and the key epilogue,
// compared in one load instead of byte-at-a-time.
const (
	wordTrueLE   = uint32('t') | uint32('r')<<8 | uint32('u')<<16 | uint32('e')<<24
	wordAlseLE   = uint32('a') | uint32('l')<<8 | uint32('s')<<16 | uint32('e')<<24
	wordNullLE   = uint32('n') | uint32('u')<<8 | uint32('l')<<16 | uint32('l')<<24
	quoteColonLE = uint16('"') | uint16(':')<<8
)

func literalNullAt(src []byte, i int) bool {
	if i < 0 || i > len(src)-4 {
		return false
	}
	return binary.LittleEndian.Uint32(src[i:i+4]) == wordNullLE
}

func literalTrueAt(src []byte, i int) bool {
	if i < 0 || i > len(src)-4 {
		return false
	}
	return binary.LittleEndian.Uint32(src[i:i+4]) == wordTrueLE
}

// literalFalseTailAt validates the bytes after a leading 'f' already observed
// by the caller. Keeping that precondition in the name prevents a redundant
// indexed load and bounds check in every scalar and boolean dispatch path.
func literalFalseTailAt(src []byte, i int) bool {
	if i < 0 || i > len(src)-5 {
		return false
	}
	return binary.LittleEndian.Uint32(src[i+1:i+5]) == wordAlseLE
}

// parse16Digits parses sixteen validated ASCII digits by applying the cited
// eight-digit SWAR reduction to each word and combining them in base 10^8.
// Keeping the three reduction steps here lets the compiler inline them into
// the root decoder instead of paying an extra package-boundary wrapper call.
func parse16Digits(src []byte) uint64 {
	b := (*[16]byte)(src)
	hi := parse8DigitsWord(binary.LittleEndian.Uint64(b[0:8]))
	lo := parse8DigitsWord(binary.LittleEndian.Uint64(b[8:16]))
	return hi*100_000_000 + lo
}

// parseTrailingDigits parses the k digits held in the top k bytes of a
// little-endian word, backfilling the low bytes with ASCII zeros so the
// eight-digit kernel sees a full window. k must be in [1, 8].
func parseTrailingDigits(word uint64, k int) uint64 {
	s := uint(8-k) * 8
	return parse8DigitsWord(word&(^uint64(0)<<s) | digitLower>>(64-s))
}

// parseTapeDigitsUint64 parses the validated digit run in [i, end): decimal
// digits with no redundant leading zero, as the tape builders record for
// plain-integer numbers. It reports false only past nineteen digits, where an
// int64 read overflows regardless of the digits.
//
// The word kernels anchor every load at end-8, end-16, and end-24, so each
// load ends inside the number and starts at or after the document's first
// byte whenever end allows it; shorter documents take the scalar loop.
func parseTapeDigitsUint64(src []byte, i, end int) (uint64, bool) {
	src = src[:end]
	d := end - i
	switch {
	case d <= 8:
		if end >= 8 {
			return parseTrailingDigits(binary.LittleEndian.Uint64(src[end-8:end]), d), true
		}
	case d <= 16:
		if end >= 16 {
			b := (*[16]byte)(src[end-16 : end])
			hi := parseTrailingDigits(binary.LittleEndian.Uint64(b[0:8]), d-8)
			return hi*100_000_000 + parse8DigitsWord(binary.LittleEndian.Uint64(b[8:16])), true
		}
	case d <= 19:
		if end >= 24 {
			// Nineteen digits stay below 1e19, within uint64.
			b := (*[24]byte)(src[end-24 : end])
			hi := parseTrailingDigits(binary.LittleEndian.Uint64(b[0:8]), d-16)
			return hi*10_000_000_000_000_000 + parse16Digits(b[8:24]), true
		}
	default:
		return 0, false
	}
	// The document is too short to back the word loads above; the number is
	// at most as long as the document, so this loop is as short.
	value := uint64(0)
	for _, c := range src[i:end] {
		value = value*10 + uint64(c-'0')
	}
	return value, true
}
