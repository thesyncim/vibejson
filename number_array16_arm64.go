//go:build !go1.28 && goexperiment.simd && arm64

package vibejson

import (
	"encoding/binary"
	"simd/archsimd"
)

const fixed16Uint64BatchMin = 16

var (
	fixed16ASCII0ARM      = [16]uint8{'0', '0', '0', '0', '0', '0', '0', '0', '0', '0', '0', '0', '0', '0', '0', '0'}
	fixed16DigitMask1ARM  = [4]uint32{0x0f0f0f0f, 0x0f0f0f0f, 0x0f0f0f0f, 0x0f0f0f0f}
	fixed16DigitMul1ARM   = [4]uint32{2561, 2561, 2561, 2561}
	fixed16DigitMask2ARM  = [4]uint32{0x00ff00ff, 0x00ff00ff, 0x00ff00ff, 0x00ff00ff}
	fixed16DigitMul2ARM   = [4]uint32{6553601, 6553601, 6553601, 6553601}
	fixed16ChunkWeightARM = [4]uint32{10_000, 1, 10_000, 1}
	fixed16ShiftPairsARM  = [4]int32{-8, -8, -8, -8}
	fixed16ShiftQuadsARM  = [4]int32{-16, -16, -16, -16}
)

// fixed16Uint64ArrayShape recognizes a compact array of positive sixteen-digit
// integers and validates every digit before the destination is touched. Four
// tokens share one horizontal maximum so validation amortizes the reduction
// and preserves the generic decoder's transactional fallback on malformed
// input.
func fixed16Uint64ArrayShape(src []byte, start int) (count, closePosition int, ok bool) {
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
	ascii0 := archsimd.LoadUint8x16Array(&fixed16ASCII0ARM)
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
		if digits0.Max(digits1).Max(digits2).Max(digits3).ReduceMax() > 9 {
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
	mask1 := archsimd.LoadUint32x4Array(&fixed16DigitMask1ARM)
	mul1 := archsimd.LoadUint32x4Array(&fixed16DigitMul1ARM)
	mask2 := archsimd.LoadUint32x4Array(&fixed16DigitMask2ARM)
	mul2 := archsimd.LoadUint32x4Array(&fixed16DigitMul2ARM)
	chunkWeight := archsimd.LoadUint32x4Array(&fixed16ChunkWeightARM)
	shiftPairs := archsimd.LoadInt32x4Array(&fixed16ShiftPairsARM)
	shiftQuads := archsimd.LoadInt32x4Array(&fixed16ShiftQuadsARM)

	rem := src
	out := dst
	for len(rem) >= 68 && len(out) >= 4 {
		b := (*[68]byte)(rem)
		o := (*[4]uint64)(out)
		chunks0 := archsimd.LoadUint8x16Array((*[16]uint8)(b[0:16])).ReshapeToUint32s()
		chunks1 := archsimd.LoadUint8x16Array((*[16]uint8)(b[17:33])).ReshapeToUint32s()
		chunks2 := archsimd.LoadUint8x16Array((*[16]uint8)(b[34:50])).ReshapeToUint32s()
		chunks3 := archsimd.LoadUint8x16Array((*[16]uint8)(b[51:67])).ReshapeToUint32s()

		chunks0 = chunks0.And(mask1).Mul(mul1).Shift(shiftPairs)
		chunks1 = chunks1.And(mask1).Mul(mul1).Shift(shiftPairs)
		chunks2 = chunks2.And(mask1).Mul(mul1).Shift(shiftPairs)
		chunks3 = chunks3.And(mask1).Mul(mul1).Shift(shiftPairs)
		chunks0 = chunks0.And(mask2).Mul(mul2).Shift(shiftQuads)
		chunks1 = chunks1.And(mask2).Mul(mul2).Shift(shiftQuads)
		chunks2 = chunks2.And(mask2).Mul(mul2).Shift(shiftQuads)
		chunks3 = chunks3.And(mask2).Mul(mul2).Shift(shiftQuads)

		halves0 := chunks0.Mul(chunkWeight)
		halves1 := chunks1.Mul(chunkWeight)
		halves2 := chunks2.Mul(chunkWeight)
		halves3 := chunks3.Mul(chunkWeight)
		halves0 = halves0.ConcatAddPairs(halves0)
		halves1 = halves1.ConcatAddPairs(halves1)
		halves2 = halves2.ConcatAddPairs(halves2)
		halves3 = halves3.ConcatAddPairs(halves3)
		o[0] = uint64(halves0.GetElem(0))*100_000_000 + uint64(halves0.GetElem(1))
		o[1] = uint64(halves1.GetElem(0))*100_000_000 + uint64(halves1.GetElem(1))
		o[2] = uint64(halves2.GetElem(0))*100_000_000 + uint64(halves2.GetElem(1))
		o[3] = uint64(halves3.GetElem(0))*100_000_000 + uint64(halves3.GetElem(1))
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
