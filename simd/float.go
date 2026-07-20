// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in LICENSE-GO.

// Provenance: GO-FLOATFMT-001. Adapted from Go commit
// 03845e30f7b73d1703bd8c21017297f6eecb76d6,
// src/internal/strconv/uscale.go and ftoa.go. Local changes specialize shortest
// formatting for JSON and caller-owned buffers; see docs/provenance.md.

// This file holds the shortest-decimal float formatter: the pinned Go
// implementation's unrounded fixed-point scaling algorithm (see
// https://research.swtch.com/fp), specialized for JSON spellings.

package simd

import (
	"math"
	"math/bits"
)

const (
	float32MantBits = 23
	float32MinExp   = -189
	float64MantBits = 52
	float64MinExp   = -1085
)

// AppendFloat64 appends the shortest JSON representation of value. The
// result matches encoding/json's exponent thresholds and spelling. It returns
// false, without changing dst, for NaN and infinities. The returned slice is
// caller-owned and may reuse or grow dst.
func AppendFloat64(dst []byte, value float64) ([]byte, bool) {
	b := math.Float64bits(value)
	exp := int(b>>float64MantBits) & 0x7ff
	mant := b & (1<<float64MantBits - 1)
	if exp == 0x7ff {
		return dst, false
	}
	neg := b>>63 != 0
	if exp == 0 {
		exp++
	} else {
		mant |= 1 << float64MantBits
	}
	if mant == 0 {
		if neg {
			return append(dst, '-', '0'), true
		}
		return append(dst, '0'), true
	}
	exp -= 1023
	shift := 64 - bits.Len64(mant)
	d, p := shortestFloat(mant<<shift, exp-shift-float64MantBits, float64MantBits, float64MinExp)
	abs := math.Float64frombits(b &^ (1 << 63))
	return appendFloatDigits(dst, neg, d, p, abs < 1e-6 || abs >= 1e21), true
}

// AppendFloat32 appends the shortest JSON representation of value. The
// result matches encoding/json's exponent thresholds and spelling. It returns
// false, without changing dst, for NaN and infinities. The returned slice is
// caller-owned and may reuse or grow dst.
func AppendFloat32(dst []byte, value float32) ([]byte, bool) {
	b := math.Float32bits(value)
	exp := int(b>>float32MantBits) & 0xff
	mant := uint64(b & (1<<float32MantBits - 1))
	if exp == 0xff {
		return dst, false
	}
	neg := b>>31 != 0
	if exp == 0 {
		exp++
	} else {
		mant |= 1 << float32MantBits
	}
	if mant == 0 {
		if neg {
			return append(dst, '-', '0'), true
		}
		return append(dst, '0'), true
	}
	exp -= 127
	shift := 64 - bits.Len64(mant)
	d, p := shortestFloat(mant<<shift, exp-shift-float32MantBits, float32MantBits, float32MinExp)
	abs := math.Float32frombits(b &^ (1 << 31))
	return appendFloatDigits(dst, neg, d, p, abs < 1e-6 || abs >= 1e21), true
}

func appendFloatDigits(dst []byte, neg bool, d uint64, p int, scientific bool) []byte {
	nd := decimalDigits(d)
	dp := nd + p
	for d%10 == 0 {
		d /= 10
		nd--
	}
	if !scientific && (nd == 16 || nd == 17) {
		bodyBytes := nd
		if dp <= 0 {
			bodyBytes += 2 - dp
		} else if dp < nd {
			bodyBytes++
		} else {
			bodyBytes = dp
		}
		extra := bodyBytes
		if neg {
			extra++
		}
		if cap(dst)-len(dst) >= extra {
			start := len(dst)
			dst = dst[:start+extra]
			out := dst[start:]
			i := 0
			if neg {
				out[i] = '-'
				i++
			}
			if dp <= 0 {
				out[i], out[i+1] = '0', '.'
				i += 2
				for range -dp {
					out[i] = '0'
					i++
				}
			}
			digitStart := i
			if nd == 17 {
				out[i] = byte(d/1e16) + '0'
				i++
				d %= 1e16
			}
			Store16Digits((*[16]byte)(out[i:]), d)
			if dp > 0 {
				if dp < nd {
					copy(out[digitStart+dp+1:], out[digitStart+dp:digitStart+nd])
					out[digitStart+dp] = '.'
				} else {
					for i := digitStart + nd; i < digitStart+dp; i++ {
						out[i] = '0'
					}
				}
			}
			return dst
		}
	}

	var digits [20]byte
	digitStart := 0
	switch {
	case nd == 17:
		digits[0] = byte(d/1e16) + '0'
		Store16Digits((*[16]byte)(digits[1:]), d%1e16)
	case nd == 16:
		Store16Digits((*[16]byte)(digits[:]), d)
	case nd > 8:
		Store16Digits((*[16]byte)(digits[:]), d)
		digitStart = 16 - nd
	case nd == 8:
		Store8Digits((*[8]byte)(digits[:]), d)
	default:
		for i := nd - 1; i >= 0; i-- {
			q := d / 10
			digits[i] = byte(d-q*10) + '0'
			d = q
		}
	}
	digitBytes := digits[digitStart : digitStart+nd]
	if neg {
		dst = append(dst, '-')
	}
	if scientific {
		dst = append(dst, digitBytes[0])
		if nd > 1 {
			dst = append(dst, '.')
			dst = append(dst, digitBytes[1:]...)
		}
		dst = append(dst, 'e')
		exponent := dp - 1
		if exponent < 0 {
			dst = append(dst, '-')
			exponent = -exponent
		} else {
			dst = append(dst, '+')
		}
		return appendSmallUint(dst, uint64(exponent))
	}

	if dp <= 0 {
		dst = append(dst, '0', '.')
		for range -dp {
			dst = append(dst, '0')
		}
		return append(dst, digitBytes...)
	}
	integerDigits := min(nd, dp)
	dst = append(dst, digitBytes[:integerDigits]...)
	for range dp - integerDigits {
		dst = append(dst, '0')
	}
	if integerDigits < nd {
		dst = append(dst, '.')
		dst = append(dst, digitBytes[integerDigits:]...)
	}
	return dst
}
func appendSmallUint(dst []byte, value uint64) []byte {
	if value < 10 {
		return append(dst, byte(value)+'0')
	}
	if value < 100 {
		return append(dst, byte(value/10)+'0', byte(value%10)+'0')
	}
	return append(dst, byte(value/100)+'0', byte(value/10)%10+'0', byte(value%10)+'0')
}

func decimalDigits(value uint64) int {
	n := log10Pow2(bits.Len64(value))
	if value >= decimalPowers[n] {
		n++
	}
	return n
}

var decimalPowers = [...]uint64{
	1, 10, 100, 1_000, 10_000, 100_000, 1_000_000, 10_000_000,
	100_000_000, 1_000_000_000, 10_000_000_000, 100_000_000_000,
	1_000_000_000_000, 10_000_000_000_000, 100_000_000_000_000,
	1_000_000_000_000_000, 10_000_000_000_000_000,
	100_000_000_000_000_000, 1_000_000_000_000_000_000,
	10_000_000_000_000_000_000,
}

type unrounded uint64

func (u unrounded) floor() uint64             { return uint64(u >> 2) }
func (u unrounded) round() uint64             { return uint64((u + 1 + (u>>2)&1) >> 2) }
func (u unrounded) ceil() uint64              { return uint64((u + 3) >> 2) }
func (u unrounded) nudge(delta int) unrounded { return u + unrounded(delta) }

func log10Pow2(x int) int { return (x * 78913) >> 18 }
func log2Pow10(x int) int { return (x * 108853) >> 15 }

func shortestFloat(m uint64, e, mantBits, minExp int) (d uint64, p int) {
	z := 63 - mantBits
	var minValue, maxValue uint64
	var odd uint64
	if m == 1<<63 && e > minExp {
		p = -skewed(e + z)
		minValue = m - 1<<(z-2)
		maxValue = m + 1<<(z-1)
		odd = m >> z & 1
	} else if e >= minExp {
		p = -log10Pow2(e + z)
		minValue = m - 1<<(z-1)
		maxValue = m + 1<<(z-1)
		odd = m >> z & 1
	} else {
		z += minExp - e
		p = -log10Pow2(e + z)
		minValue = m - 1<<(z-1)
		maxValue = m + 1<<(z-1)
		odd = m >> z & 1
	}

	var scale floatScaler
	prescale(&scale, e, p, log2Pow10(p))
	dmin := uscale(minValue, &scale).nudge(int(odd)).ceil()
	dmax := uscale(maxValue, &scale).nudge(-int(odd)).floor()
	if d = dmax / 10; d*10 >= dmin {
		return d, -(p - 1)
	}
	if d = dmin; d < dmax {
		d = uscale(m, &scale).round()
	}
	return d, -p
}

func skewed(e int) int { return (e*631305 - 261663) >> 21 }

type pmHiLo struct {
	hi uint64
	lo uint64
}

type floatScaler struct {
	pmHi uint64
	pmLo uint64
	s    int
}

func prescale(scale *floatScaler, e, p, lp int) {
	entry := floatPow10[p-floatPow10Min]
	scale.pmHi = entry.hi
	scale.pmLo = entry.lo
	scale.s = -(e + lp + 3)
}

func uscale(x uint64, scale *floatScaler) unrounded {
	hi, mid := bits.Mul64(x, scale.pmHi)
	shift := scale.s & 63
	if hi>>shift<<shift != hi {
		return unrounded(hi>>shift | 1)
	}
	mid2, _ := bits.Mul64(x, scale.pmLo)
	if mid < mid2 {
		hi--
	}
	sticky := uint64(0)
	if mid-mid2 > 1 {
		sticky = 1
	}
	return unrounded(hi>>shift | sticky)
}
