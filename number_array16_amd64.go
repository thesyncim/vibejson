//go:build !go1.28 && goexperiment.simd && amd64

package vibejson

import (
	"simd/archsimd"
	"unsafe"
)

const fixed16Uint64BatchMin = 16

var fixed16HasAVX2 = archsimd.X86.AVX2()

var (
	fixed16ASCII0AMD      = [16]uint8{'0', '0', '0', '0', '0', '0', '0', '0', '0', '0', '0', '0', '0', '0', '0', '0'}
	fixed16PairWeightsAMD = [16]int8{10, 1, 10, 1, 10, 1, 10, 1, 10, 1, 10, 1, 10, 1, 10, 1}
	fixed16QuadWeightsAMD = [8]int16{100, 1, 100, 1, 100, 1, 100, 1}
	fixed16HalfWeightsAMD = [8]int16{10000, 1, 10000, 1, 10000, 1, 10000, 1}
)

// fixed16Uint64ArrayShape recognizes a compact array of positive sixteen-digit
// integers and validates every digit before the destination is touched. Four
// tokens share one horizontal maximum so validation amortizes the reduction
// and preserves the generic decoder's transactional fallback on malformed
// input.
func fixed16Uint64ArrayShape(src []byte, start int) (count, closePosition int, ok bool) {
	if !fixed16HasAVX2 {
		return 0, 0, false
	}
	end := len(src)
	for end > start && IsJSONWhitespace(src[end-1]) {
		end--
	}
	if end <= start || src[end-1] != ']' {
		return 0, 0, false
	}
	closePosition = end - 1
	payloadBytes := closePosition - start
	if payloadBytes < 16 || (payloadBytes+1)%17 != 0 {
		return 0, 0, false
	}
	count = (payloadBytes + 1) / 17
	if count < fixed16Uint64BatchMin {
		return 0, 0, false
	}

	base := sliceBase(src)
	ascii0 := archsimd.LoadUint8x16Array(&fixed16ASCII0AMD)
	index := 0
	for ; index+4 <= count; index += 4 {
		offset0 := start + index*17
		offset1 := offset0 + 17
		offset2 := offset1 + 17
		offset3 := offset2 + 17
		if fastByteAt(base, offset0) == '0' || fastByteAt(base, offset1) == '0' ||
			fastByteAt(base, offset2) == '0' || fastByteAt(base, offset3) == '0' {
			return 0, 0, false
		}
		if fastByteAt(base, offset0+16) != ',' || fastByteAt(base, offset1+16) != ',' ||
			fastByteAt(base, offset2+16) != ',' ||
			(index+4 < count && fastByteAt(base, offset3+16) != ',') {
			return 0, 0, false
		}
		digits0 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, offset0))).Sub(ascii0)
		digits1 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, offset1))).Sub(ascii0)
		digits2 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, offset2))).Sub(ascii0)
		digits3 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, offset3))).Sub(ascii0)
		if digits0.Max(digits1).Max(digits2.Max(digits3)).Greater(archsimd.BroadcastUint8x16(9)).ToBits() != 0 {
			return 0, 0, false
		}
	}
	for ; index < count; index++ {
		offset := start + index*17
		if fastByteAt(base, offset) == '0' || !all16Digits(unsafe.Add(base, offset)) ||
			(index+1 < count && fastByteAt(base, offset+16) != ',') {
			return 0, 0, false
		}
	}
	return count, closePosition, true
}

// parseFixed16Uint64Array converts a shape already proved by
// fixed16Uint64ArrayShape. Constants remain register-resident across four
// independent vectors, avoiding the setup and ABI costs that made a
// per-number SIMD helper slower than the scalar inline path.
//
// Provenance: ALGO-DIGITS-001.
func parseFixed16Uint64Array(base unsafe.Pointer, start, count int, dst unsafe.Pointer) {
	// Each multiply-add is exact: pairs <= 99, quads <= 9999,
	// and eight-digit halves <= 99999999. The signed pack cannot saturate.
	nibble := archsimd.BroadcastUint8x16(15)
	pairs := archsimd.LoadInt8x16Array(&fixed16PairWeightsAMD)
	quads := archsimd.LoadInt16x8Array(&fixed16QuadWeightsAMD)
	halves := archsimd.LoadInt16x8Array(&fixed16HalfWeightsAMD)

	index := 0
	for ; index+4 <= count; index += 4 {
		offset0 := start + index*17
		offset1 := offset0 + 17
		offset2 := offset1 + 17
		offset3 := offset2 + 17
		q0 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, offset0))).And(nibble).DotProductPairsSaturated(pairs).DotProductPairs(quads)
		q1 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, offset1))).And(nibble).DotProductPairsSaturated(pairs).DotProductPairs(quads)
		q2 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, offset2))).And(nibble).DotProductPairsSaturated(pairs).DotProductPairs(quads)
		q3 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, offset3))).And(nibble).DotProductPairsSaturated(pairs).DotProductPairs(quads)
		h01 := q0.SaturateToInt16Concat(q1).DotProductPairs(halves)
		h23 := q2.SaturateToInt16Concat(q3).DotProductPairs(halves)
		*(*uint64)(unsafe.Add(dst, uintptr(index+0)*8)) = uint64(h01.GetElem(0))*100_000_000 + uint64(h01.GetElem(1))
		*(*uint64)(unsafe.Add(dst, uintptr(index+1)*8)) = uint64(h01.GetElem(2))*100_000_000 + uint64(h01.GetElem(3))
		*(*uint64)(unsafe.Add(dst, uintptr(index+2)*8)) = uint64(h23.GetElem(0))*100_000_000 + uint64(h23.GetElem(1))
		*(*uint64)(unsafe.Add(dst, uintptr(index+3)*8)) = uint64(h23.GetElem(2))*100_000_000 + uint64(h23.GetElem(3))

	}
	for ; index < count; index++ {
		*(*uint64)(unsafe.Add(dst, uintptr(index)*8)) =
			parse16Digits(unsafe.Add(base, start+index*17))
	}
}
