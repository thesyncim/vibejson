package simdjson

import (
	"encoding/binary"
	"math"
	"math/bits"
	"strconv"
	"unsafe"

	simdkernels "github.com/thesyncim/simdjson/simd"
)

// appendJSONFloat appends value the way encoding/json spells it, shared by
// the compiled encoder and the streaming writer.
func appendJSONFloat(dst []byte, value float64, bits int) ([]byte, error) {
	if math.IsInf(value, 0) || math.IsNaN(value) {
		return dst, &EncodeError{Reason: "unsupported float value " + strconv.FormatFloat(value, 'g', -1, bits)}
	}

	// Fast paths for values whose shortest fixed form is provably the digits
	// emitted here. Integer-valued floats below 1e15 sit in rounding
	// intervals narrower than the integer grid, so their exact integer is
	// the shortest representation. Decimals below 1e9 with up to six exact
	// fractional digits sit in intervals narrower than the 1e-6 grid, so no
	// shorter or alternative fixed decimal can round to the same value; the
	// division check guarantees the digits parse back to exactly this float.
	// Shortest-representation intervals depend on the value's own precision:
	// float32 integers are only exact and unique below 2^24.
	integerLimit := 1e15
	if bits == 32 {
		integerLimit = 1 << 24
	}
	if positive := math.Abs(value); positive < integerLimit {
		if truncated := math.Trunc(value); truncated == value {
			if value == 0 && math.Signbit(value) {
				return append(dst, '-', '0'), nil
			}
			return appendCompactInt(dst, int64(value)), nil
		}
		if bits == 64 && positive < 1e9 {
			if scaled := value * 1e6; math.Trunc(scaled) == scaled && scaled/1e6 == value {
				return appendScaledDecimal6(dst, value, scaled), nil
			}
		}
	}

	if bits == 32 {
		dst, _ = simdkernels.AppendFloat32(dst, float32(value))
	} else {
		dst, _ = simdkernels.AppendFloat64(dst, value)
	}
	return dst, nil
}

// appendScaledDecimal6 writes an exactly recoverable fixed decimal with up
// to six fractional digits. Callers only reach it below 1e9, where adjacent
// 1e-6 grid points are wider than a float64 rounding interval.
func appendScaledDecimal6(dst []byte, value, scaled float64) []byte {
	if math.Signbit(value) {
		dst = append(dst, '-')
		scaled = -scaled
	}
	units := uint64(scaled)
	fraction := units % 1e6
	units /= 1e6
	dst = appendCompactUint(dst, units)
	// Three digit-pair stores spell the six fractional digits — the same
	// table technique the compact integer formatter uses — and one XOR plus
	// LeadingZeros64 counts the trailing zero digits without a loop; the
	// caller guarantees a nonzero fraction, so at least one digit survives.
	var digits [8]byte
	digits[1] = '.'
	storeCompactDigitPair((*[2]byte)(unsafe.Pointer(&digits[2])), fraction/10000)
	storeCompactDigitPair((*[2]byte)(unsafe.Pointer(&digits[4])), fraction/100%100)
	storeCompactDigitPair((*[2]byte)(unsafe.Pointer(&digits[6])), fraction%100)
	word := binary.LittleEndian.Uint64(digits[:]) ^ 0x3030303030303030
	trailingZeroDigits := bits.LeadingZeros64(word) >> 3
	return append(dst, digits[1:8-trailingZeroDigits]...)
}
