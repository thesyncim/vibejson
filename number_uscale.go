// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in LICENSE-GO.

// Provenance: GO-USCALE-001. Adapted from Go commit
// d468ad3648be469ffc4090e4586c29709182d6b6,
// src/internal/strconv/uscale.go. Local changes specialize the parsing half
// for JSON decimal exponents; see docs/provenance.md.

package slopjson

// This file contains the parsing half of the pinned Go implementation's fast
// unrounded scaling algorithm, specialized to negative decimal exponents.
// The original implementation is BSD licensed by the Go Authors. See
// https://research.swtch.com/fp for the algorithm and proof.

import (
	"math"
	"math/bits"
)

type jsonPow10 struct {
	hi uint64
	lo uint64
}

// Scaled 128-bit mantissas for 1e-22 through 1e-1.
var jsonNegativePow10 = [...]jsonPow10{
	{0xf1c90080baf72cb2, 0xacdb3974ed229cc7},
	{0x971da05074da7bef, 0x2c0903e91435a1fc},
	{0xbce5086492111aeb, 0x770b44e359430a7b},
	{0xec1e4a7db69561a6, 0xd4ce161c2f93cd1a},
	{0x9392ee8e921d5d08, 0xc500cdd19dbc6030},
	{0xb877aa3236a4b44a, 0xf6410146052b783d},
	{0xe69594bec44de15c, 0xb3d141978676564c},
	{0x901d7cf73ab0acda, 0xf062c8feb409f5ef},
	{0xb424dc35095cd810, 0xac7b7b3e610c736b},
	{0xe12e13424bb40e14, 0xd79a5a0df94f9046},
	{0x8cbccc096f5088cc, 0x06c07848bbd1ba2c},
	{0xafebff0bcb24aaff, 0x0870965aeac628b7},
	{0xdbe6fecebdedd5bf, 0x4a8cbbf1a577b2e4},
	{0x89705f4136b4a598, 0xce97f577076acfcf},
	{0xabcc77118461cefd, 0x023df2d4c94583c2},
	{0xd6bf94d5e57a42bd, 0xc2cd6f89fb96e4b3},
	{0x8637bd05af6c69b6, 0x59c065b63d3e4ef0},
	{0xa7c5ac471b478424, 0xf0307f23cc8de2ac},
	{0xd1b71758e219652c, 0x2c3c9eecbfb15b57},
	{0x83126e978d4fdf3c, 0x9ba5e353f7ced916},
	{0xa3d70a3d70a3d70b, 0xc28f5c28f5c28f5c},
	{0xcccccccccccccccd, 0x3333333333333333},
}

type jsonUnrounded uint64

func (u jsonUnrounded) round() uint64 {
	return uint64((u + 1 + (u>>2)&1) >> 2)
}

func jsonLog2Pow10(exponent int) int {
	return (exponent * 108853) >> 15
}

// scaleJSONFloat64 rounds mantissa*10^exponent exactly as strconv. Its table
// is intentionally bounded; callers fall back to strconv outside this range.
func scaleJSONFloat64(mantissa uint64, exponent int, negative bool) (float64, bool) {
	if exponent < -22 || exponent >= 0 || mantissa == 0 {
		return 0, false
	}
	b := bits.Len64(mantissa)
	lp := jsonLog2Pow10(exponent)
	e := min(1074, 53-b-lp)
	shift := -(e - (64 - b) + lp + 3)
	if shift >= 64 {
		if negative {
			return math.Copysign(0, -1), true
		}
		return 0, true
	}
	if shift < 0 {
		return 0, false
	}

	power := jsonNegativePow10[exponent+22]
	hi, mid := bits.Mul64(mantissa<<(64-b), power.hi)
	s := shift & 63
	var scaled jsonUnrounded
	if hi>>s<<s != hi {
		scaled = jsonUnrounded(hi>>s | 1)
	} else {
		mid2, _ := bits.Mul64(mantissa<<(64-b), power.lo)
		if mid < mid2 {
			hi--
		}
		sticky := uint64(0)
		if mid-mid2 > 1 {
			sticky = 1
		}
		scaled = jsonUnrounded(hi>>s | sticky)
	}

	if scaled >= jsonUnrounded(1<<53<<2-2) {
		scaled = scaled>>1 | scaled&1
		e--
	}
	packed := scaled.round()
	if negative {
		packed |= 1 << 63
	}
	if packed&(1<<52) == 0 {
		return math.Float64frombits(packed), true
	}
	if -e >= 0x7ff-1075 {
		return 0, false
	}
	bits64 := packed&^(uint64(1)<<52) | uint64(1075-e)<<52
	return math.Float64frombits(bits64), true
}

// scaleJSONFloat64Fixed is the exact negative-power conversion used by typed
// fixed-shape fast paths. Callers prove a nonzero, normal result and supply the
// already-selected power and binary exponent bias.
func scaleJSONFloat64Fixed(mantissa, powerHi, powerLo uint64, exponentBias int, negative bool) float64 {
	b := bits.Len64(mantissa)
	normalized := mantissa << (64 - b)
	hi, mid := bits.Mul64(normalized, powerHi)

	var scaled jsonUnrounded
	if hi&0xff != 0 {
		scaled = jsonUnrounded(hi>>8 | 1)
	} else {
		mid2, _ := bits.Mul64(normalized, powerLo)
		if mid < mid2 {
			hi--
		}
		sticky := uint64(0)
		if mid-mid2 > 1 {
			sticky = 1
		}
		scaled = jsonUnrounded(hi>>8 | sticky)
	}

	if scaled >= jsonUnrounded(1<<53<<2-2) {
		scaled = scaled>>1 | scaled&1
		exponentBias++
	}
	packed := scaled.round()
	if negative {
		packed |= 1 << 63
	}
	packed = packed&^(uint64(1)<<52) | uint64(exponentBias+b)<<52
	return math.Float64frombits(packed)
}
