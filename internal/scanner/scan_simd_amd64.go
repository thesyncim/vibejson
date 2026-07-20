//go:build go1.27 && !go1.28 && goexperiment.simd && amd64

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

func validUTF8NoLineSeparatorRuntime(src []byte) bool {
	return validUTF8NoLineSeparatorGeneric(src)
}

func validUTF8Runtime(src []byte) bool {
	if len(src) < 16 {
		return utf8.Valid(src)
	}
	base := unsafe.Pointer(unsafe.SliceData(src))
	b80 := archsimd.BroadcastUint8x16(0x80)
	b90 := archsimd.BroadcastUint8x16(0x90)
	ba0 := archsimd.BroadcastUint8x16(0xa0)
	bc0 := archsimd.BroadcastUint8x16(0xc0)
	bc2 := archsimd.BroadcastUint8x16(0xc2)
	be0 := archsimd.BroadcastUint8x16(0xe0)
	bed := archsimd.BroadcastUint8x16(0xed)
	bf0 := archsimd.BroadcastUint8x16(0xf0)
	bf4 := archsimd.BroadcastUint8x16(0xf4)
	bf5 := archsimd.BroadcastUint8x16(0xf5)
	zero := archsimd.BroadcastUint8x16(0)
	prevLead := zero
	prevLead34 := zero
	prevLead4 := zero
	prevE0 := zero
	prevED := zero
	prevF0 := zero
	prevF4 := zero

	i := 0
	for i+16 <= len(src) {
		v := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i)))
		continuation := v.GreaterEqual(b80).And(v.Less(bc0))
		lead2 := v.GreaterEqual(bc2).And(v.Less(be0))
		lead3 := v.GreaterEqual(be0).And(v.Less(bf0))
		lead4 := v.GreaterEqual(bf0).And(v.Less(bf5))
		invalid := v.GreaterEqual(bc0).And(v.Less(bc2)).Or(v.GreaterEqual(bf5))

		lead := lead2.Or(lead3).Or(lead4).ToInt8x16().ToBits()
		lead34 := lead3.Or(lead4).ToInt8x16().ToBits()
		lead4Bytes := lead4.ToInt8x16().ToBits()
		expected := lead.ConcatShiftBytesRight(prevLead, 15).
			Or(lead34.ConcatShiftBytesRight(prevLead34, 14)).
			Or(lead4Bytes.ConcatShiftBytesRight(prevLead4, 13))
		actual := continuation.ToInt8x16().ToBits()
		invalid = invalid.Or(actual.NotEqual(expected))

		afterE0 := v.Equal(be0).ToInt8x16().ToBits().ConcatShiftBytesRight(prevE0, 15).BitsToInt8().ToMask()
		afterED := v.Equal(bed).ToInt8x16().ToBits().ConcatShiftBytesRight(prevED, 15).BitsToInt8().ToMask()
		afterF0 := v.Equal(bf0).ToInt8x16().ToBits().ConcatShiftBytesRight(prevF0, 15).BitsToInt8().ToMask()
		afterF4 := v.Equal(bf4).ToInt8x16().ToBits().ConcatShiftBytesRight(prevF4, 15).BitsToInt8().ToMask()
		invalid = invalid.Or(afterE0.And(v.Less(ba0)).
			Or(afterED.And(v.GreaterEqual(ba0))).
			Or(afterF0.And(v.Less(b90))).
			Or(afterF4.And(v.GreaterEqual(b90))))
		if maskHasAnyLane(invalid) {
			return false
		}

		prevLead = lead
		prevLead34 = lead34
		prevLead4 = lead4Bytes
		prevE0 = v.Equal(be0).ToInt8x16().ToBits()
		prevED = v.Equal(bed).ToInt8x16().ToBits()
		prevF0 = v.Equal(bf0).ToInt8x16().ToBits()
		prevF4 = v.Equal(bf4).ToInt8x16().ToBits()
		i += 16
	}

	tail := i
	for tail > 0 && i-tail < 3 && src[tail-1]&0xc0 == 0x80 {
		tail--
	}
	if tail > 0 {
		tail--
	}
	return utf8.Valid(src[tail:])
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
				return i + bits.TrailingZeros32(b0)
			}
			return i + 32 + bits.TrailingZeros32(b1)
		}
		i += 64
	}
	if i+32 <= n {
		v := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(base, i)))
		b := v.Equal(quote).Or(v.Equal(slash)).Or(v.Equal(lt)).Or(v.Equal(gt)).Or(v.Equal(amp)).Or(v.BitsToInt8().Less(ctrlOrNonASCII)).ToBits()
		if b != 0 {
			return i + bits.TrailingZeros32(b)
		}
		i += 32
	}
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
				return i + bits.TrailingZeros32(b0)
			}
			return i + 32 + bits.TrailingZeros32(b1)
		}
		i += 64
	}
	if i+32 <= n {
		v := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(base, i)))
		b := v.Equal(quote).Or(v.Equal(slash)).Or(v.Equal(lt)).Or(v.Equal(gt)).Or(v.Equal(amp)).Or(v.Less(ctrl)).ToBits()
		if b != 0 {
			return i + bits.TrailingZeros32(b)
		}
		i += 32
	}
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
				return i + bits.TrailingZeros32(b0)
			}
			return i + 32 + bits.TrailingZeros32(b1)
		}
		i += 64
	}
	if i+32 <= n {
		v := archsimd.LoadUint8x32Array((*[32]uint8)(unsafe.Add(base, i)))
		b := v.Equal(quote).Or(v.Equal(slash)).Or(v.Less(ctrl)).ToBits()
		if b != 0 {
			return i + bits.TrailingZeros32(b)
		}
		i += 32
	}
	return scanStringSyntaxSIMD(src, i)
}
