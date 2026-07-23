package simd

import "encoding/binary"

const (
	digitLower = uint64(0x3030303030303030)
	digitUpper = uint64(0x4646464646464646)
	digitHigh  = uint64(0x8080808080808080)
)

// All8Digits reports whether every byte is an ASCII decimal digit.
func All8Digits(digits *[8]byte) bool {
	return nonDigitMask8(binary.LittleEndian.Uint64(digits[:])) == 0
}

// All16Digits reports whether every byte is an ASCII decimal digit.
func All16Digits(digits *[16]byte) bool {
	return nonDigitMask8(binary.LittleEndian.Uint64(digits[:8])) == 0 &&
		nonDigitMask8(binary.LittleEndian.Uint64(digits[8:])) == 0
}

// Provenance: ALGO-DIGITS-001.
// Parse8Digits reduces eight ASCII decimal digits without validating them,
// using the exact SWAR reduction recorded by Johnny Lee in "Fast numeric
// string to int" (2016), where Lee credits the formula to bormand. C++
// slopjson 4.6.4 preserves Lee's credit; docs/provenance.md records the exact
// references and Daniel Lemire's related derivation.
// Call All8Digits first when the input is not already known to be digits.
func Parse8Digits(digits *[8]byte) uint64 {
	return parse8DigitsWord(binary.LittleEndian.Uint64(digits[:]))
}

func parse8DigitsWord(x uint64) uint64 {
	x = (x & 0x0f0f0f0f0f0f0f0f) * 2561 >> 8
	x = (x & 0x00ff00ff00ff00ff) * 6553601 >> 16
	return (x & 0x0000ffff0000ffff) * 42949672960001 >> 32
}

// Store8Digits writes value as exactly eight ASCII decimal digits, including
// leading zeroes. Value must be less than 10^8.
func Store8Digits(dst *[8]byte, value uint64) {
	encoded := uint64(0x3030303030303030) + encodeTwoFourDigitChunks(value/10_000, value%10_000)
	binary.LittleEndian.PutUint64(dst[:], encoded)
}

// Store16Digits writes value as exactly sixteen ASCII decimal digits, including
// leading zeroes. Value must be less than 10^16.
func Store16Digits(dst *[16]byte, value uint64) {
	store16Digits(dst, value)
}

func store16DigitsScalar(dst *[16]byte, value uint64) {
	hi := value / 100_000_000
	lo := value - hi*100_000_000
	first := uint64(0x3030303030303030) + encodeTwoFourDigitChunks(hi/10_000, hi%10_000)
	second := uint64(0x3030303030303030) + encodeTwoFourDigitChunks(lo/10_000, lo%10_000)
	binary.LittleEndian.PutUint64(dst[:8], first)
	binary.LittleEndian.PutUint64(dst[8:], second)
}

func storeDateTimePartsScalar(dst *[20]byte, year, month, day, hour, minute, second uint32) {
	var digits [16]byte
	Store8Digits((*[8]byte)(digits[:8]), uint64(year)*10_000+uint64(month)*100+uint64(day))
	Store8Digits((*[8]byte)(digits[8:]), uint64(hour)*1_000_000+uint64(minute)*10_000+uint64(second)*100)
	copy(dst[:], `"0000-00-00T00:00:00`)
	dst[1], dst[2], dst[3], dst[4] = digits[0], digits[1], digits[2], digits[3]
	dst[6], dst[7] = digits[4], digits[5]
	dst[9], dst[10] = digits[6], digits[7]
	dst[12], dst[13] = digits[8], digits[9]
	dst[15], dst[16] = digits[10], digits[11]
	dst[18], dst[19] = digits[12], digits[13]
}

// encodeTwoFourDigitChunks converts two values below 10000 into eight unpacked
// decimal digits using parallel reciprocal division within one scalar word.
func encodeTwoFourDigitChunks(hi, lo uint64) uint64 {
	merged := hi | lo<<32
	top := ((merged * 10486) >> 20) & (0x7f | 0x7f<<32)
	bottom := merged - 100*top
	hundreds := bottom<<16 + top
	tens := (hundreds * 103) >> 10
	tens &= 0x0f | 0x0f<<16 | 0x0f<<32 | 0x0f<<48
	return tens + (hundreds-10*tens)<<8
}

func nonDigitMask8(x uint64) uint64 {
	return ((x + digitUpper) | (x - digitLower)) & digitHigh
}
