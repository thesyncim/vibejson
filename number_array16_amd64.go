//go:build !go1.28 && goexperiment.simd && amd64

package vibejson

import (
	"encoding/binary"
	"simd/archsimd"
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

	rem := src[start : closePosition+1]
	ascii0 := archsimd.LoadUint8x16Array(&fixed16ASCII0AMD)
	for len(rem) >= 68 {
		b := (*[68]byte)(rem)
		if b[0] == '0' || b[17] == '0' || b[34] == '0' || b[51] == '0' {
			return 0, 0, false
		}
		if b[16] != ',' || b[33] != ',' || b[50] != ',' || (len(rem) > 68 && b[67] != ',') {
			return 0, 0, false
		}
		digits0 := archsimd.LoadUint8x16Array((*[16]uint8)(b[0:16])).Sub(ascii0)
		digits1 := archsimd.LoadUint8x16Array((*[16]uint8)(b[17:33])).Sub(ascii0)
		digits2 := archsimd.LoadUint8x16Array((*[16]uint8)(b[34:50])).Sub(ascii0)
		digits3 := archsimd.LoadUint8x16Array((*[16]uint8)(b[51:67])).Sub(ascii0)
		if digits0.Max(digits1).Max(digits2.Max(digits3)).Greater(archsimd.BroadcastUint8x16(9)).ToBits() != 0 {
			return 0, 0, false
		}
		rem = rem[68:]
	}
	for len(rem) >= 17 {
		b := (*[17]byte)(rem)
		if b[0] == '0' ||
			nonDigitMask8(binary.LittleEndian.Uint64(b[0:8]))|nonDigitMask8(binary.LittleEndian.Uint64(b[8:16])) != 0 ||
			(len(rem) > 17 && b[16] != ',') {
			return 0, 0, false
		}
		rem = rem[17:]
	}
	return count, closePosition, true
}

// parseFixed16Uint64Array converts a shape already proved by
// fixed16Uint64ArrayShape. Constants remain register-resident across four
// independent vectors, avoiding the setup and ABI costs that made a
// per-number SIMD helper slower than the scalar inline path.
//
// Provenance: ALGO-DIGITS-001.
func parseFixed16Uint64Array(src []byte, dst []uint64) {
	// Each multiply-add is exact: pairs <= 99, quads <= 9999,
	// and eight-digit halves <= 99999999. The signed pack cannot saturate.
	nibble := archsimd.BroadcastUint8x16(15)
	pairs := archsimd.LoadInt8x16Array(&fixed16PairWeightsAMD)
	quads := archsimd.LoadInt16x8Array(&fixed16QuadWeightsAMD)
	halves := archsimd.LoadInt16x8Array(&fixed16HalfWeightsAMD)

	rem := src
	out := dst
	for len(rem) >= 68 && len(out) >= 4 {
		b := (*[68]byte)(rem)
		o := (*[4]uint64)(out)
		q0 := archsimd.LoadUint8x16Array((*[16]uint8)(b[0:16])).And(nibble).DotProductPairsSaturated(pairs).DotProductPairs(quads)
		q1 := archsimd.LoadUint8x16Array((*[16]uint8)(b[17:33])).And(nibble).DotProductPairsSaturated(pairs).DotProductPairs(quads)
		q2 := archsimd.LoadUint8x16Array((*[16]uint8)(b[34:50])).And(nibble).DotProductPairsSaturated(pairs).DotProductPairs(quads)
		q3 := archsimd.LoadUint8x16Array((*[16]uint8)(b[51:67])).And(nibble).DotProductPairsSaturated(pairs).DotProductPairs(quads)
		h01 := q0.SaturateToInt16Concat(q1).DotProductPairs(halves)
		h23 := q2.SaturateToInt16Concat(q3).DotProductPairs(halves)
		o[0] = uint64(h01.GetElem(0))*100_000_000 + uint64(h01.GetElem(1))
		o[1] = uint64(h01.GetElem(2))*100_000_000 + uint64(h01.GetElem(3))
		o[2] = uint64(h23.GetElem(0))*100_000_000 + uint64(h23.GetElem(1))
		o[3] = uint64(h23.GetElem(2))*100_000_000 + uint64(h23.GetElem(3))
		rem = rem[68:]
		out = out[4:]
	}
	for len(rem) >= 17 && len(out) > 0 {
		b := (*[17]byte)(rem)
		hi := parse8DigitsWord(binary.LittleEndian.Uint64(b[0:8]))
		lo := parse8DigitsWord(binary.LittleEndian.Uint64(b[8:16]))
		out[0] = hi*100_000_000 + lo
		rem = rem[17:]
		out = out[1:]
	}
}
