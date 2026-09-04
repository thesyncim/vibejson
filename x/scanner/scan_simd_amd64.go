//go:build !go1.28 && goexperiment.simd && amd64

package scanner

import (
	"encoding/binary"
	"math/bits"
	"simd/archsimd"
	"unicode/utf8"
	"unsafe"
)

// scanAMD64Level selects the vector width once at startup for v1/v2 binaries
// that may run on processors without AVX2. GOAMD64=v3 and newer builds compile
// scanner calls directly to AVX2. Both paths use static calls: indirect calls
// make escape analysis treat scanned buffers as leaking, which moves callers'
// stack storage onto the heap.
var scanAMD64Level uint8

const (
	scanLevelScalar uint8 = iota
	scanLevelAVX2
)

func selectAMD64ScannerLevel(hasAVX2 bool) uint8 {
	// AVX-512 remains an experimental direct kernel until it wins across
	// representative CPU families and short/long input distributions. AVX2 is
	// the demonstrated production width, including on AVX-512-capable CPUs.
	if hasAVX2 {
		return scanLevelAVX2
	}
	return scanLevelScalar
}

func initStringScanner() {
	// The raw AVX2 entry and staged syntax scanner need 32 remaining bytes; the
	// syntax scanner's 16-byte word probes run on spans of 40 or more. The
	// ordinary special scanner below owns its separate 24-byte prefix policy.
	// Capability checks happen only here. v1/v2 hot calls read the
	// process-constant level; v3 and newer builds compile directly to AVX2.
	scanAMD64Level = selectAMD64ScannerLevel(archsimd.X86.AVX2())
	if scanAMD64Level == scanLevelAVX2 {
		scanStringSelectedMinBytes = 32
		scanStringProbeMinBytes = 40
		scanStringSpecialBackend = "amd64-avx2"
		scanStringVectorBytes = 32
	}
}

// scanStringSpecial gives the ordinary amd64 scanner a fixed three-word
// prefix before AVX2. JSON strings found while building an index usually end
// in those words even when the remaining document is long. If a 32-55 byte
// span survives the prefix, the final AVX2 block overlaps only bytes already
// proved clean, closing the old 32-39 byte direct-vector gap without losing the
// vector win for late stops.
func scanStringSpecial(src []byte, i int) int {
	remaining := len(src) - i
	if remaining < 24 {
		return scanStringSpecialScalar(src, i)
	}
	window := src[i : i+24]
	if m := stringSpecialMask(binary.LittleEndian.Uint64(window)); m != 0 {
		return i + bits.TrailingZeros64(m)/8
	}
	if m := stringSpecialMask(binary.LittleEndian.Uint64(window[8:])); m != 0 {
		return i + 8 + bits.TrailingZeros64(m)/8
	}
	if m := stringSpecialMask(binary.LittleEndian.Uint64(window[16:])); m != 0 {
		return i + 16 + bits.TrailingZeros64(m)/8
	}
	i += 24
	if remaining < 32 || !scanAVX2Available() {
		return scanStringSpecialScalar(src, i)
	}
	if len(src)-i < 32 {
		i = len(src) - 32
	}
	return scanStringSpecialAVX2(src, i)
}

func scanStringSpecialRuntime(src []byte, i int) int {
	if scanAVX2Available() {
		return scanStringSpecialAVX2(src, i)
	}
	return scanStringSpecialScalar(src, i)
}

func scanStringSyntaxRuntime(src []byte, i int) int {
	if scanAVX2Available() {
		return scanStringSyntaxAVX2(src, i)
	}
	return scanStringSyntaxScalar(src, i)
}

func scanEncodedHTMLSpecialRuntime(src []byte, i int) int {
	if scanAVX2Available() {
		return scanEncodedHTMLSpecialAVX2(src, i)
	}
	return scanEncodedHTMLSpecialScalar(src, i)
}

func scanEncodedHTMLSyntaxRuntime(src []byte, i int) int {
	if scanAVX2Available() {
		return scanEncodedHTMLSyntaxAVX2(src, i)
	}
	return scanEncodedHTMLSyntaxScalar(src, i)
}

func scanVectorAvailable() bool {
	return scanAVX2Available()
}

func validUTF8NoLineSeparatorRuntime(src []byte) bool {
	if !scanAVX2Available() {
		return utf8.Valid(src) && !hasJSONLineSeparatorScalar(src, 0)
	}
	return validUTF8NoLineSeparatorGeneric(src)
}

func validUTF8Runtime(src []byte) bool {
	if len(src) < 16 || !scanAVX2Available() {
		return utf8.Valid(src)
	}
	return validUTF8AVX2(src)
}

// Provenance: CPP-UTF8-001.
// The lookup tables and validation structure are adapted from C++ simdjson
// 4.6.4, commit 1bcf71bd85059ab6574ea1159de9298dcc1212c5,
// src/generic/stage1/utf8_lookup4_algorithm.h; Apache-2.0, see
// LICENSE-SIMDJSON. That source implements Keiser and Lemire, "Validating
// UTF-8 In Less Than One Instruction Per Byte" (2020). Local changes translate
// the kernel to Go SIMD, handle scalar tails, and use amd64 byte
// permutations and early error exits.
var utf8LookupFirstHigh = [16]uint8{
	2, 2, 2, 2, 2, 2, 2, 2,
	128, 128, 128, 128, 33, 1, 21, 73,
}

var utf8LookupFirstLow = [16]uint8{
	231, 163, 131, 131, 139, 203, 203, 203,
	203, 203, 203, 203, 203, 219, 203, 203,
}

var utf8LookupSecondHigh = [16]uint8{
	1, 1, 1, 1, 1, 1, 1, 1,
	230, 174, 186, 186, 1, 1, 1, 1,
}

// Keep the guarded AVX2 instructions out of baseline dispatch wrappers.
//
//go:noinline
func validUTF8AVX2(src []byte) bool {
	base := unsafe.Pointer(unsafe.SliceData(src))
	firstHighTable := archsimd.LoadUint8x16Array(&utf8LookupFirstHigh)
	firstLowTable := archsimd.LoadUint8x16Array(&utf8LookupFirstLow)
	secondHighTable := archsimd.LoadUint8x16Array(&utf8LookupSecondHigh)
	lowNibble := archsimd.BroadcastUint8x16(0x0f)
	e0Minus1 := archsimd.BroadcastUint8x16(0xdf)
	f0Minus1 := archsimd.BroadcastUint8x16(0xef)
	continuationBit := archsimd.BroadcastUint8x16(0x80)
	zero := archsimd.BroadcastUint8x16(0)
	previous := zero
	previousHigh := zero

	i := 0
	for i+16 <= len(src) {
		input := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i)))
		prev1 := input.ConcatShiftBytesRight(previous, 15)
		prev2 := input.ConcatShiftBytesRight(previous, 14)
		prev3 := input.ConcatShiftBytesRight(previous, 13)
		inputHigh := input.ReshapeToUint16s().ShiftAllRight(4).ReshapeToUint8s().And(lowNibble)
		prev1High := inputHigh.ConcatShiftBytesRight(previousHigh, 15)
		firstHigh := firstHighTable.PermuteOrZero(prev1High.BitsToInt8())
		firstLow := firstLowTable.PermuteOrZero(prev1.And(lowNibble).BitsToInt8())
		secondHigh := secondHighTable.PermuteOrZero(inputHigh.BitsToInt8())
		special := firstHigh.And(firstLow).And(secondHigh)
		mustContinue := prev2.SubSaturated(e0Minus1).
			Or(prev3.SubSaturated(f0Minus1)).Greater(zero)
		mustContinueBits := mustContinue.ToInt8x16().ToBits().And(continuationBit)
		if maskHasAnyLane(mustContinueBits.Xor(special).NotEqual(zero)) {
			return false
		}
		previous = input
		previousHigh = inputHigh
		i += 16
	}
	// One zero-padded final block continues the streamed state: the padding
	// reads as ASCII NUL, so a multi-byte sequence dangling at the true end
	// of input surfaces as a missing continuation. It runs even when the
	// input ends exactly on a block boundary, where an all-zero block plays
	// the same role for a sequence dangling out of the last full block.
	var tailBlock [16]uint8
	copy(tailBlock[:], src[i:])
	input := archsimd.LoadUint8x16Array(&tailBlock)
	prev1 := input.ConcatShiftBytesRight(previous, 15)
	prev2 := input.ConcatShiftBytesRight(previous, 14)
	prev3 := input.ConcatShiftBytesRight(previous, 13)
	inputHigh := input.ReshapeToUint16s().ShiftAllRight(4).ReshapeToUint8s().And(lowNibble)
	prev1High := inputHigh.ConcatShiftBytesRight(previousHigh, 15)
	firstHigh := firstHighTable.PermuteOrZero(prev1High.BitsToInt8())
	firstLow := firstLowTable.PermuteOrZero(prev1.And(lowNibble).BitsToInt8())
	secondHigh := secondHighTable.PermuteOrZero(inputHigh.BitsToInt8())
	special := firstHigh.And(firstLow).And(secondHigh)
	mustContinue := prev2.SubSaturated(e0Minus1).
		Or(prev3.SubSaturated(f0Minus1)).Greater(zero)
	mustContinueBits := mustContinue.ToInt8x16().ToBits().And(continuationBit)
	return !maskHasAnyLane(mustContinueBits.Xor(special).NotEqual(zero))
}

func scanEncodedHTMLSpecialAVX2(src []byte, i int) int {
	n := len(src)
	quote := archsimd.BroadcastUint8x32('"')
	slash := archsimd.BroadcastUint8x32('\\')
	lt := archsimd.BroadcastUint8x32('<')
	gt := archsimd.BroadcastUint8x32('>')
	amp := archsimd.BroadcastUint8x32('&')
	ctrlOrNonASCII := archsimd.BroadcastInt8x32(0x20)
	base := unsafe.Pointer(unsafe.SliceData(src))
	for i+64 <= n {
		v0 := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(base, i)))
		v1 := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(base, i+32)))
		b0 := v0.Equal(quote).Or(v0.Equal(slash)).Or(v0.Equal(lt)).Or(v0.Equal(gt)).Or(v0.Equal(amp)).Or(v0.BitsToInt8().Less(ctrlOrNonASCII)).ToBits()
		b1 := v1.Equal(quote).Or(v1.Equal(slash)).Or(v1.Equal(lt)).Or(v1.Equal(gt)).Or(v1.Equal(amp)).Or(v1.BitsToInt8().Less(ctrlOrNonASCII)).ToBits()
		if b0|b1 != 0 {
			if b0 != 0 {
				archsimd.ClearAVXUpperBits()
				return i + bits.TrailingZeros32(b0)
			}
			archsimd.ClearAVXUpperBits()
			return i + 32 + bits.TrailingZeros32(b1)
		}
		i += 64
	}
	if i+32 <= n {
		v := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(base, i)))
		b := v.Equal(quote).Or(v.Equal(slash)).Or(v.Equal(lt)).Or(v.Equal(gt)).Or(v.Equal(amp)).Or(v.BitsToInt8().Less(ctrlOrNonASCII)).ToBits()
		if b != 0 {
			archsimd.ClearAVXUpperBits()
			return i + bits.TrailingZeros32(b)
		}
		i += 32
	}
	archsimd.ClearAVXUpperBits()
	return scanEncodedHTMLSpecialSIMD(src, i)
}

func scanEncodedHTMLSyntaxAVX2(src []byte, i int) int {
	n := len(src)
	quote := archsimd.BroadcastUint8x32('"')
	slash := archsimd.BroadcastUint8x32('\\')
	lt := archsimd.BroadcastUint8x32('<')
	gt := archsimd.BroadcastUint8x32('>')
	amp := archsimd.BroadcastUint8x32('&')
	ctrl := archsimd.BroadcastUint8x32(0x20)
	base := unsafe.Pointer(unsafe.SliceData(src))
	for i+64 <= n {
		v0 := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(base, i)))
		v1 := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(base, i+32)))
		b0 := v0.Equal(quote).Or(v0.Equal(slash)).Or(v0.Equal(lt)).Or(v0.Equal(gt)).Or(v0.Equal(amp)).Or(v0.Less(ctrl)).ToBits()
		b1 := v1.Equal(quote).Or(v1.Equal(slash)).Or(v1.Equal(lt)).Or(v1.Equal(gt)).Or(v1.Equal(amp)).Or(v1.Less(ctrl)).ToBits()
		if b0|b1 != 0 {
			if b0 != 0 {
				archsimd.ClearAVXUpperBits()
				return i + bits.TrailingZeros32(b0)
			}
			archsimd.ClearAVXUpperBits()
			return i + 32 + bits.TrailingZeros32(b1)
		}
		i += 64
	}
	if i+32 <= n {
		v := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(base, i)))
		b := v.Equal(quote).Or(v.Equal(slash)).Or(v.Equal(lt)).Or(v.Equal(gt)).Or(v.Equal(amp)).Or(v.Less(ctrl)).ToBits()
		if b != 0 {
			archsimd.ClearAVXUpperBits()
			return i + bits.TrailingZeros32(b)
		}
		i += 32
	}
	archsimd.ClearAVXUpperBits()
	return scanEncodedHTMLSyntaxSIMD(src, i)
}

// scanStringSpecialAVX2 clears the upper vector lanes before every exit. Its
// callers resume scalar or 128-bit Go immediately, and leaving YMM state dirty
// imposes a transition penalty on affected x86 processors.
func scanStringSpecialAVX2(src []byte, i int) int {
	n := len(src)
	quote := archsimd.BroadcastUint8x32('"')
	slash := archsimd.BroadcastUint8x32('\\')
	ctrlOrNonASCII := archsimd.BroadcastInt8x32(0x20)
	base := unsafe.Pointer(unsafe.SliceData(src))

	for i+64 <= n {
		v0 := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(base, i)))
		v1 := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(base, i+32)))
		b0 := v0.Equal(quote).
			Or(v0.Equal(slash)).
			Or(v0.BitsToInt8().Less(ctrlOrNonASCII)).ToBits()
		b1 := v1.Equal(quote).
			Or(v1.Equal(slash)).
			Or(v1.BitsToInt8().Less(ctrlOrNonASCII)).ToBits()
		if b0|b1 != 0 {
			if b0 != 0 {
				archsimd.ClearAVXUpperBits()
				return i + bits.TrailingZeros32(b0)
			}
			archsimd.ClearAVXUpperBits()
			return i + 32 + bits.TrailingZeros32(b1)
		}
		i += 64
	}
	if i+32 <= n {
		v := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(base, i)))
		b := v.Equal(quote).
			Or(v.Equal(slash)).
			Or(v.BitsToInt8().Less(ctrlOrNonASCII)).ToBits()
		if b != 0 {
			archsimd.ClearAVXUpperBits()
			return i + bits.TrailingZeros32(b)
		}
		i += 32
	}
	if i == n {
		archsimd.ClearAVXUpperBits()
		return i
	}
	archsimd.ClearAVXUpperBits()
	return scanStringSpecialSIMD(src, i)
}

func scanStringSyntaxAVX2(src []byte, i int) int {
	n := len(src)
	quote := archsimd.BroadcastUint8x32('"')
	slash := archsimd.BroadcastUint8x32('\\')
	ctrl := archsimd.BroadcastUint8x32(0x20)
	base := unsafe.Pointer(unsafe.SliceData(src))

	for i+64 <= n {
		v0 := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(base, i)))
		v1 := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(base, i+32)))
		b0 := v0.Equal(quote).Or(v0.Equal(slash)).Or(v0.Less(ctrl)).ToBits()
		b1 := v1.Equal(quote).Or(v1.Equal(slash)).Or(v1.Less(ctrl)).ToBits()
		if b0|b1 != 0 {
			if b0 != 0 {
				archsimd.ClearAVXUpperBits()
				return i + bits.TrailingZeros32(b0)
			}
			archsimd.ClearAVXUpperBits()
			return i + 32 + bits.TrailingZeros32(b1)
		}
		i += 64
	}
	if i+32 <= n {
		v := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(base, i)))
		b := v.Equal(quote).Or(v.Equal(slash)).Or(v.Less(ctrl)).ToBits()
		if b != 0 {
			archsimd.ClearAVXUpperBits()
			return i + bits.TrailingZeros32(b)
		}
		i += 32
	}
	archsimd.ClearAVXUpperBits()
	return scanStringSyntaxSIMD(src, i)
}
