package simdjson

//go:generate go run ./internal/cmd/codegen float-eisel-table

import (
	"math"
	"math/bits"
)

// Provenance: GO-EISEL-001.
// Adapted from Go 1.25.0, commit
// 6e676ab2b809d46623acb5988248d95d1eb7939c,
// src/strconv/eisel_lemire.go. Copyright 2020 The Go Authors; BSD-3-Clause,
// see LICENSE-GO. Local changes specialize the float64 path and comments for
// strict JSON parsing; docs/provenance.md records the transitive lineage.
//
// eiselLemire64 converts the decimal significand man times 10**exp10 to the
// nearest float64, returning ok=false when the rounding is too close to a tie
// to decide from 128 bits of the power of ten.
//
// The caller must have already stripped the sign and must pass an untruncated
// man (all significant digits fit in the 64-bit value). When ok is false the
// caller falls back to a correctly rounding scalar parser, so a conservative
// false is always safe: eiselLemire64 never returns an incorrectly rounded
// value with ok=true.
func eiselLemire64(man uint64, exp10 int, neg bool) (f float64, ok bool) {
	// Zero and out-of-table powers defer to the caller.
	switch {
	case man == 0:
		if neg {
			return math.Float64frombits(0x8000000000000000), true
		}
		return 0, true
	case exp10 < detailedPowersOfTenMinExp10 || detailedPowersOfTenMaxExp10 < exp10:
		return 0, false
	}

	// Normalize the significand to have its high bit set.
	clz := bits.LeadingZeros64(man)
	man <<= uint(clz)

	// retExp2 is the base-2 exponent. 217706/2^16 approximates log2(10) and is
	// exact as floor(exp10*log2(10)) across the tabulated range; 1075 is the
	// float64 exponent bias (1023) plus the 52 fraction bits.
	const float64ExponentBias = 1023
	retExp2 := uint64((217706*exp10)>>16) + 64 + float64ExponentBias - uint64(clz)

	// Multiply the significand by the high 64 bits of the tabulated power.
	pow := detailedPowersOfTen[exp10-detailedPowersOfTenMinExp10]
	xHi, xLo := bits.Mul64(man, pow[1])

	// If the top bits are all ones, the low half of the power might change the
	// result: fold in the low 64 bits of the power for a 128-bit product.
	if xHi&0x1FF == 0x1FF && xLo+man < man {
		yHi, yLo := bits.Mul64(man, pow[0])
		mergedHi, mergedLo := xHi, xLo+yHi
		if mergedLo < xLo {
			mergedHi++
		}
		// Still ambiguous after 128 bits: defer to the slow path.
		if mergedHi&0x1FF == 0x1FF && mergedLo+1 == 0 && yLo+man < man {
			return 0, false
		}
		xHi, xLo = mergedHi, mergedLo
	}

	// The product occupies 128 bits; shift so 54 significant bits remain, one
	// more than the 53-bit float64 significand so the guard bit is available.
	msb := xHi >> 63
	retMantissa := xHi >> (msb + 9)
	retExp2 -= 1 ^ msb

	// Exactly halfway between two representable values with the round bit set:
	// the 128 bits cannot break the tie, so defer.
	if xLo == 0 && xHi&0x1FF == 0 && retMantissa&3 == 1 {
		return 0, false
	}

	// Round to nearest, ties to even, and renormalize on carry-out.
	retMantissa += retMantissa & 1
	retMantissa >>= 1
	if retMantissa>>53 > 0 {
		retMantissa >>= 1
		retExp2++
	}

	// Subnormal or overflow: let the slow path handle the edges.
	if retExp2-1 >= 0x7FF-1 {
		return 0, false
	}

	retBits := retExp2<<52 | retMantissa&0x000FFFFFFFFFFFFF
	if neg {
		retBits |= 0x8000000000000000
	}
	return math.Float64frombits(retBits), true
}
