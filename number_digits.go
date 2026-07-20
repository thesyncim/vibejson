package simdjson

import (
	"encoding/binary"
	"math/bits"
	"unsafe"

	simdkernels "github.com/thesyncim/simdjson/simd"
)

const (
	digitLower  = uint64(0x3030303030303030)
	digitUpper  = uint64(0x4646464646464646)
	digitHigh   = uint64(0x8080808080808080)
	digitLower4 = uint32(0x30303030)
	digitUpper4 = uint32(0x46464646)
	digitHigh4  = uint32(0x80808080)
)

// numberSource is a GC-visible base for synchronous number scanners. Keeping
// the document pointer typed prevents the scanner family from passing an
// untyped unsafe.Pointer through every layer.
//
// Bounds: base points at byte zero of the live source; callers pass only
// validated indices and prove each fixed-width load fits before pointerAt.
// Ownership: the source remains live and immutable for the complete scanner
// call. Neither numberSource nor the pointers returned by pointerAt are stored.
// Postconditions: offsets are used immediately for reads; pointers are not
// converted to uintptr and cannot widen the source's validated bounds.
type numberSource struct {
	base *byte
}

// numberSourceOf constructs the GC-visible source used by scanners that carry
// numberSource through their call graph. The typed interior pointer keeps the
// source allocation visible throughout synchronous scanning. Empty sources
// must not be read; their base value is deliberately immaterial.
func numberSourceOf(src []byte) numberSource {
	return numberSource{base: unsafe.SliceData(src)}
}

func (s numberSource) byteAt(index int) byte {
	return *(*byte)(unsafe.Add(unsafe.Pointer(s.base), index))
}

func (s numberSource) pointerAt(index int) unsafe.Pointer {
	return unsafe.Add(unsafe.Pointer(s.base), index)
}

// stringRange returns a read-only string view over a caller-validated range.
// The returned string is itself GC-visible and keeps the source allocation
// live while strconv consumes it synchronously; the parser does not retain it.
func (s numberSource) stringRange(start, end int) string {
	return unsafe.String((*byte)(s.pointerAt(start)), end-start)
}

// rawNumberBase is the borrowed []byte-to-number-kernel boundary.
//
// Bounds: src is non-empty, so the result addresses src[0]; every caller passes
// len(src) to scanners and validates their returned end before later loads.
// Ownership: the result borrows src and is used only during the synchronous
// accessor call; callers keep src live and immutable for that complete call.
// Postconditions: the pointer is neither retained, stored, converted to
// uintptr, nor used to widen the source bounds.
// Callers: RawValue.Int64, RawValue.Uint64, and RawValue.Float64.
func rawNumberBase(src []byte) unsafe.Pointer {
	return unsafe.Pointer(unsafe.SliceData(src))
}

func nonDigitMask8(x uint64) uint64 {
	return ((x + digitUpper) | (x - digitLower)) & digitHigh
}

func nonDigitMask4(x uint32) uint32 {
	return ((x + digitUpper4) | (x - digitLower4)) & digitHigh4
}

// scanDigitsFast advances over a decimal digit run. Short runs stay scalar;
// sustained runs classify eight bytes per iteration and locate the delimiter
// directly from the first non-digit lane.
func scanDigitsFast(base unsafe.Pointer, n, i int) int {
	if i+4 <= n && isDigit(fastByteAt(base, i+3)) {
		for i+8 <= n {
			invalid := nonDigitMask8(loadUint64LE(unsafe.Add(base, i)))
			if invalid != 0 {
				return i + bits.TrailingZeros64(invalid)/8
			}
			i += 8
		}
	}
	for i < n && isDigit(fastByteAt(base, i)) {
		i++
	}
	return i
}

// scanDigitsLong is scanDigitsFast after the caller has proved that lanes 0
// through 7 are digits. It avoids repeating the short-run gate on fraction
// paths that already loaded lane 7.
func scanDigitsLong(base unsafe.Pointer, n, i int) int {
	for i+8 <= n {
		invalid := nonDigitMask8(loadUint64LE(unsafe.Add(base, i)))
		if invalid != 0 {
			return i + bits.TrailingZeros64(invalid)/8
		}
		i += 8
	}
	for i < n && isDigit(fastByteAt(base, i)) {
		i++
	}
	return i
}

func all16Digits(base unsafe.Pointer) bool {
	return simdkernels.All16Digits((*[16]byte)(base))
}

func all8Digits(base unsafe.Pointer) bool {
	return simdkernels.All8Digits((*[8]byte)(base))
}

// Provenance: ALGO-DIGITS-001.
// parse8Digits reduces eight ASCII digits with the same cited SWAR reduction
// as simd.Parse8Digits. It is the small-token companion to the architecture
// SIMD 16-digit parser; see docs/provenance.md.
func parse8Digits(base unsafe.Pointer) uint64 {
	return simdkernels.Parse8Digits((*[8]byte)(base))
}

func parse8DigitsWord(x uint64) uint64 {
	x = (x & 0x0f0f0f0f0f0f0f0f) * 2561 >> 8
	x = (x & 0x00ff00ff00ff00ff) * 6553601 >> 16
	return (x & 0x0000ffff0000ffff) * 42949672960001 >> 32
}

func loadUint64LE(base unsafe.Pointer) uint64 {
	return binary.LittleEndian.Uint64((*[8]byte)(base)[:])
}

func storeUint64LE(base unsafe.Pointer, v uint64) {
	binary.LittleEndian.PutUint64((*[8]byte)(base)[:], v)
}

func loadUint32LE(base unsafe.Pointer) uint32 {
	return binary.LittleEndian.Uint32((*[4]byte)(base)[:])
}

func loadUint16LE(base unsafe.Pointer) uint16 {
	return binary.LittleEndian.Uint16((*[2]byte)(base)[:])
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

// parse16Digits parses sixteen validated ASCII digits with the same two-word
// SWAR reduction as the public kernel. Keeping the three reduction steps here
// lets the compiler inline them into the root decoder instead of paying an
// extra package-boundary wrapper call. Keep this route synchronized with any
// future architecture-specific Parse16Digits policy in package simd.
func parse16Digits(base unsafe.Pointer) uint64 {
	hi := parse8DigitsWord(binary.LittleEndian.Uint64((*[8]byte)(base)[:]))
	lo := parse8DigitsWord(binary.LittleEndian.Uint64((*[8]byte)(unsafe.Add(base, 8))[:]))
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
func parseTapeDigitsUint64(base unsafe.Pointer, i, end int) (uint64, bool) {
	d := end - i
	switch {
	case d <= 8:
		if end >= 8 {
			return parseTrailingDigits(loadUint64LE(unsafe.Add(base, end-8)), d), true
		}
	case d <= 16:
		if end >= 16 {
			hi := parseTrailingDigits(loadUint64LE(unsafe.Add(base, end-16)), d-8)
			return hi*100_000_000 + parse8Digits(unsafe.Add(base, end-8)), true
		}
	case d <= 19:
		if end >= 24 {
			// Nineteen digits stay below 1e19, within uint64.
			hi := parseTrailingDigits(loadUint64LE(unsafe.Add(base, end-24)), d-16)
			return hi*10_000_000_000_000_000 + parse16Digits(unsafe.Add(base, end-16)), true
		}
	default:
		return 0, false
	}
	// The document is too short to back the word loads above; the number is
	// at most as long as the document, so this loop is as short.
	value := uint64(0)
	for ; i < end; i++ {
		value = value*10 + uint64(fastByteAt(base, i)-'0')
	}
	return value, true
}
