//go:build go1.27 && !go1.28 && goexperiment.simd && arm64

package simd

import (
	"encoding/binary"
	"simd/archsimd"
)

var (
	digitWeights10ARM    = [...]uint8{10, 1, 10, 1, 10, 1, 10, 1, 10, 1, 10, 1, 10, 1, 10, 1}
	digitWeights100ARM   = [...]uint16{100, 1, 100, 1, 100, 1, 100, 1}
	digitWeights10000ARM = [...]uint32{10000, 1, 10000, 1}
	digitFormatDiv100ARM = [...]uint32{10486, 10486, 10486, 10486}
	digitFormatMul100ARM = [...]uint32{100, 100, 100, 100}
	digitFormatDiv10ARM  = [...]uint32{103, 103, 103, 103}
	digitFormatMul10ARM  = [...]uint32{10, 10, 10, 10}
	digitShiftRight20ARM = [...]int32{-20, -20, -20, -20}
	digitShiftRight10ARM = [...]int32{-10, -10, -10, -10}
	dateTimeIndicesARM   = [...]uint8{16, 0, 1, 2, 3, 16, 4, 5, 16, 6, 7, 16, 8, 9, 16, 10}
	dateTimeLiteralsARM  = [...]uint8{'"', 0, 0, 0, 0, '-', 0, 0, '-', 0, 0, 'T', 0, 0, ':', 0}
)

// Parse16Digits reduces sixteen ASCII decimal digits without validating them.
// The two-word SWAR reducer is faster than the NEON reduction on the Apple M4
// Max benchmark runner, so the public hot path calls it directly. Call
// All16Digits first when the input is not already known to be digits.
func Parse16Digits(digits *[16]byte) uint64 {
	hi := parse8DigitsWord(binary.LittleEndian.Uint64(digits[:8]))
	lo := parse8DigitsWord(binary.LittleEndian.Uint64(digits[8:]))
	return hi*100_000_000 + lo
}

// Parse16DigitsChecked validates and reduces sixteen ASCII decimal digits in
// one fused NEON operation. It returns false and zero when any byte is not a
// digit.
func Parse16DigitsChecked(digits *[16]byte) (uint64, bool) {
	values := archsimd.LoadUint8x16Array(digits).Sub(archsimd.BroadcastUint8x16('0'))
	if values.ReduceMax() > 9 {
		return 0, false
	}
	weighted10 := values.Mul(archsimd.LoadUint8x16Array(&digitWeights10ARM))
	lo := weighted10.ExtendLo8ToUint16()
	hi := weighted10.HiToLo().ExtendLo8ToUint16()
	pairs := lo.ConcatAddPairs(hi)
	weighted100 := pairs.Mul(archsimd.LoadUint16x8Array(&digitWeights100ARM))
	quads := weighted100.ConcatAddPairs(weighted100).ExtendLo4ToUint32()
	weighted10000 := quads.Mul(archsimd.LoadUint32x4Array(&digitWeights10000ARM))
	eights := weighted10000.ConcatAddPairs(weighted10000)
	return uint64(eights.GetElem(0))*100000000 + uint64(eights.GetElem(1)), true
}

func store16Digits(dst *[16]byte, value uint64) {
	format16Digits(value).StoreArray(dst)
}

func format16Digits(value uint64) archsimd.Uint8x16 {
	hi := value / 100_000_000
	lo := value - hi*100_000_000
	hiTop := (hi * 0xd1b71759) >> 45
	loTop := (lo * 0xd1b71759) >> 45
	hiChunks := hiTop | (hi-hiTop*10_000)<<32
	loChunks := loTop | (lo-loTop*10_000)<<32
	chunks := archsimd.Uint64x2{}.
		SetElem(0, hiChunks).
		SetElem(1, loChunks).
		ReshapeToUint32s()
	return format4DigitChunks(chunks)
}

func format4DigitChunks(chunks archsimd.Uint32x4) archsimd.Uint8x16 {
	div100 := archsimd.LoadUint32x4Array(&digitFormatDiv100ARM)
	mul100 := archsimd.LoadUint32x4Array(&digitFormatMul100ARM)
	hundreds := chunks.Mul(div100).Shift(archsimd.LoadInt32x4Array(&digitShiftRight20ARM))
	below100 := chunks.Sub(hundreds.Mul(mul100))

	div10 := archsimd.LoadUint32x4Array(&digitFormatDiv10ARM)
	mul10 := archsimd.LoadUint32x4Array(&digitFormatMul10ARM)
	shiftRight10 := archsimd.LoadInt32x4Array(&digitShiftRight10ARM)
	thousands := hundreds.Mul(div10).Shift(shiftRight10)
	hundredsDigit := hundreds.Sub(thousands.Mul(mul10))
	tens := below100.Mul(div10).Shift(shiftRight10)
	ones := below100.Sub(tens.Mul(mul10))

	thousandsBytes := thousands.TruncToUint16().TruncToUint8()
	hundredsBytes := hundredsDigit.TruncToUint16().TruncToUint8()
	tensBytes := tens.TruncToUint16().TruncToUint8()
	onesBytes := ones.TruncToUint16().TruncToUint8()
	highPairs := thousandsBytes.InterleaveLo(tensBytes)
	lowPairs := hundredsBytes.InterleaveLo(onesBytes)
	return highPairs.InterleaveLo(lowPairs).Add(archsimd.BroadcastUint8x16('0'))
}

func storeDateTimeParts(dst *[20]byte, year, month, day, hour, minute, second uint32) {
	yearHigh := year / 100
	yearLow := year - yearHigh*100
	highPairs := archsimd.Uint64x2{}.
		SetElem(0, uint64(yearHigh)|uint64(month)<<32).
		SetElem(1, uint64(hour)|uint64(second)<<32).
		ReshapeToUint32s()
	lowPairs := archsimd.Uint64x2{}.
		SetElem(0, uint64(yearLow)|uint64(day)<<32).
		SetElem(1, uint64(minute)).
		ReshapeToUint32s()

	div10 := archsimd.LoadUint32x4Array(&digitFormatDiv10ARM)
	mul10 := archsimd.LoadUint32x4Array(&digitFormatMul10ARM)
	shiftRight10 := archsimd.LoadInt32x4Array(&digitShiftRight10ARM)
	highTens := highPairs.Mul(div10).Shift(shiftRight10)
	lowTens := lowPairs.Mul(div10).Shift(shiftRight10)
	highDigits := highTens.TruncToUint16().TruncToUint8().
		InterleaveLo(highPairs.Sub(highTens.Mul(mul10)).TruncToUint16().TruncToUint8())
	lowDigits := lowTens.TruncToUint16().TruncToUint8().
		InterleaveLo(lowPairs.Sub(lowTens.Mul(mul10)).TruncToUint16().TruncToUint8())
	digits := highDigits.ReshapeToUint16s().InterleaveLo(lowDigits.ReshapeToUint16s()).
		ReshapeToUint8s().Add(archsimd.BroadcastUint8x16('0'))
	formatted := digits.LookupOrZero(archsimd.LoadUint8x16Array(&dateTimeIndicesARM)).
		Or(archsimd.LoadUint8x16Array(&dateTimeLiteralsARM))
	formatted.StoreArray((*[16]byte)(dst[:16]))
	dst[16] = digits.GetElem(11)
	dst[17] = ':'
	dst[18] = digits.GetElem(12)
	dst[19] = digits.GetElem(13)
}

func parseBackend() string {
	return "scalar"
}

func parseVectorBytes() int {
	return 0
}

func formatBackend() string {
	return "arm64-neon"
}

func formatVectorBytes() int {
	return 16
}
