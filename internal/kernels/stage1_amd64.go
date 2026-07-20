//go:build go1.27 && !go1.28 && goexperiment.simd && amd64.v3

package kernels

import (
	"simd/archsimd"
	"unsafe"
)

// Stage1Backend identifies the structural classifier selected by this build.
const Stage1Backend = "amd64-avx2"

// Stage1Block classifies one full 64-byte block starting at p. GOAMD64=v3 is
// part of the build constraint because this kernel lowers to AVX instructions;
// v1 and v2 builds select the portable implementation at compile time.
//
// The nibble
// lookups use PermuteOrZero, which lowers to the AVX-baseline byte shuffle
// where Permute would require the AVX-512 VBMI byte permute; the nibble
// indexes never set the shuffle's zeroing bit, so the OrZero semantics are
// a plain lookup here. The high nibble comes from a halfword shift because
// amd64 has no per-byte shift.
func Stage1Block(p *[64]byte, m *Stage1Masks) {
	base := unsafe.Pointer(p)
	v0 := archsimd.LoadUint8x16Array((*[16]uint8)(base))
	v1 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, 16)))
	v2 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, 32)))
	v3 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, 48)))

	quote := archsimd.BroadcastUint8x16('"')
	slash := archsimd.BroadcastUint8x16('\\')
	ctrl := archsimd.BroadcastUint8x16(0x20)
	lowNibble := archsimd.BroadcastUint8x16(0x0f)
	loTable := archsimd.LoadUint8x16Array(&stage1ClassLo)
	hiTable := archsimd.LoadUint8x16Array(&stage1ClassHi)
	wsBits := archsimd.BroadcastUint8x16(stage1WhitespaceBits)
	structBits := archsimd.BroadcastUint8x16(stage1StructuralBits)
	zero := archsimd.BroadcastUint8x16(0)

	hi0 := v0.ReshapeToUint16s().ShiftAllRight(4).ReshapeToUint8s().And(lowNibble)
	hi1 := v1.ReshapeToUint16s().ShiftAllRight(4).ReshapeToUint8s().And(lowNibble)
	hi2 := v2.ReshapeToUint16s().ShiftAllRight(4).ReshapeToUint8s().And(lowNibble)
	hi3 := v3.ReshapeToUint16s().ShiftAllRight(4).ReshapeToUint8s().And(lowNibble)

	c0 := loTable.PermuteOrZero(v0.And(lowNibble).BitsToInt8()).And(hiTable.PermuteOrZero(hi0.BitsToInt8()))
	c1 := loTable.PermuteOrZero(v1.And(lowNibble).BitsToInt8()).And(hiTable.PermuteOrZero(hi1.BitsToInt8()))
	c2 := loTable.PermuteOrZero(v2.And(lowNibble).BitsToInt8()).And(hiTable.PermuteOrZero(hi2.BitsToInt8()))
	c3 := loTable.PermuteOrZero(v3.And(lowNibble).BitsToInt8()).And(hiTable.PermuteOrZero(hi3.BitsToInt8()))

	m.Whitespace = stage1Movemask64(
		c0.And(wsBits).NotEqual(zero),
		c1.And(wsBits).NotEqual(zero),
		c2.And(wsBits).NotEqual(zero),
		c3.And(wsBits).NotEqual(zero),
	)
	m.Structural = stage1Movemask64(
		c0.And(structBits).NotEqual(zero),
		c1.And(structBits).NotEqual(zero),
		c2.And(structBits).NotEqual(zero),
		c3.And(structBits).NotEqual(zero),
	)
	m.Quote = stage1Movemask64(v0.Equal(quote), v1.Equal(quote), v2.Equal(quote), v3.Equal(quote))
	m.Backslash = stage1Movemask64(v0.Equal(slash), v1.Equal(slash), v2.Equal(slash), v3.Equal(slash))
	m.Control = stage1Movemask64(v0.Less(ctrl), v1.Less(ctrl), v2.Less(ctrl), v3.Less(ctrl))
	highBit := archsimd.BroadcastUint8x16(0x80)
	m.NonASCII = v0.Or(v1).Or(v2).Or(v3).And(highBit).NotEqual(zero).ToBits() != 0
}

// stage1Movemask64 folds four 16-lane compare masks into one 64-bit mask,
// one bit per byte, using the native mask-to-bits conversion.
func stage1Movemask64(m0, m1, m2, m3 archsimd.Mask8x16) uint64 {
	return uint64(m0.ToBits()) |
		uint64(m1.ToBits())<<16 |
		uint64(m2.ToBits())<<32 |
		uint64(m3.ToBits())<<48
}
