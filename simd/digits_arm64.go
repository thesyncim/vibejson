//go:build !go1.28 && goexperiment.simd && arm64

package simd

import (
	"simd/archsimd"
)

var (
	digitFormatDiv100ARM = [...]uint32{10486, 10486, 10486, 10486}
	digitFormatMul100ARM = [...]uint32{100, 100, 100, 100}
	digitShiftRight20ARM = [...]int32{-20, -20, -20, -20}
	digitFormatDiv10ARM  = [...]uint16{103, 103, 103, 103, 103, 103, 103, 103}
	digitFormatMul10ARM  = [...]uint16{10, 10, 10, 10, 10, 10, 10, 10}
	digitShiftRight10ARM = [...]int16{-10, -10, -10, -10, -10, -10, -10, -10}
	dateTimeIndicesARM   = [...]uint8{16, 0, 1, 2, 3, 16, 4, 5, 16, 6, 7, 16, 8, 9, 16, 10}
	dateTimeLiteralsARM  = [...]uint8{'"', 0, 0, 0, 0, '-', 0, 0, '-', 0, 0, 'T', 0, 0, ':', 0}
)

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
	hundreds := chunks.Mul(div100).Shift(archsimd.LoadInt32x4Array(&digitShiftRight20ARM))
	below100 := chunks.Sub(hundreds.Mul(archsimd.LoadUint32x4Array(&digitFormatMul100ARM)))
	pairs := hundreds.TruncToUint16().InterleaveLo(below100.TruncToUint16())
	return formatDigitPairs(pairs)
}

// Each lane is in [0, 99], so multiplication by 103 fits in 16 bits and
// (pair*103)>>10 is exact division by ten throughout the input range.
func formatDigitPairs(pairs archsimd.Uint16x8) archsimd.Uint8x16 {
	tens := pairs.Mul(archsimd.LoadUint16x8Array(&digitFormatDiv10ARM)).Shift(archsimd.LoadInt16x8Array(&digitShiftRight10ARM))
	ones := pairs.Sub(tens.Mul(archsimd.LoadUint16x8Array(&digitFormatMul10ARM)))
	return tens.TruncToUint8().InterleaveLo(ones.TruncToUint8()).Add(archsimd.BroadcastUint8x16('0'))
}

func storeDateTimeParts(dst *[20]byte, year, month, day, hour, minute, second uint32) {
	yearHigh := year / 100
	yearLow := year - yearHigh*100
	pairs := archsimd.Uint64x2{}.
		SetElem(0, uint64(yearHigh)|uint64(yearLow)<<16|uint64(month)<<32|uint64(day)<<48).
		SetElem(1, uint64(hour)|uint64(minute)<<16|uint64(second)<<32).
		ReshapeToUint16s()
	digits := formatDigitPairs(pairs)
	formatted := digits.LookupOrZero(archsimd.LoadUint8x16Array(&dateTimeIndicesARM)).
		Or(archsimd.LoadUint8x16Array(&dateTimeLiteralsARM))
	formatted.StoreArray((*[16]byte)(dst[:16]))
	dst[16] = digits.GetElem(11)
	dst[17] = ':'
	dst[18] = digits.GetElem(12)
	dst[19] = digits.GetElem(13)
}

func formatBackend() string {
	return "arm64-neon"
}

func formatVectorBytes() int {
	return 16
}
