package simdjson

import (
	"math"
	"strconv"

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
	dst = append(dst, '.')
	var digits [6]byte
	for i := 5; i >= 0; i-- {
		digits[i] = byte('0' + fraction%10)
		fraction /= 10
	}
	end := len(digits)
	for digits[end-1] == '0' {
		end--
	}
	return append(dst, digits[:end]...)
}
