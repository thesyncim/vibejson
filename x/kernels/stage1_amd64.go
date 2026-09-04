//go:build !go1.28 && goexperiment.simd && amd64

package kernels

import (
	"simd/archsimd"
	"unsafe"
)

// Each AVX2 byte shuffle looks up within a 128-bit lane. Replicate both
// classifier tables so the high lane has the same exact nibble mapping.
var stage1ClassLoAVX2 = [32]byte{
	1, 0, 0, 0, 0, 0, 0, 0, 0, 2, 10, 32, 16, 66, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 2, 10, 32, 16, 66, 0, 0,
}
var stage1ClassHiAVX2 = [32]byte{
	2, 0, 17, 8, 0, 96, 0, 96, 0, 0, 0, 0, 0, 0, 0, 0, 2, 0, 17, 8, 0, 96, 0, 96, 0, 0, 0, 0, 0, 0, 0, 0,
}

// stage1BlockAVX2 classifies two full 32-byte vectors. The caller proves AVX2
// support; keeping this entry out of line prevents AVX in baseline wrappers.
// PermuteOrZeroGrouped uses lane-local VPSHUFB, without requiring AVX-512 VBMI.
//
//go:noinline
func stage1BlockAVX2(p *[64]byte, m *Stage1Masks) {
	base := unsafe.Pointer(p)
	v0 := archsimd.LoadUint8x32Array((*[32]byte)(base))
	v1 := archsimd.LoadUint8x32Array((*[32]byte)(unsafe.Add(base, 32)))
	nibble := archsimd.BroadcastUint8x32(15)
	lo := archsimd.LoadUint8x32Array(&stage1ClassLoAVX2)
	hi := archsimd.LoadUint8x32Array(&stage1ClassHiAVX2)
	h0 := v0.ReshapeToUint16s().ShiftAllRight(4).ReshapeToUint8s().And(nibble)
	h1 := v1.ReshapeToUint16s().ShiftAllRight(4).ReshapeToUint8s().And(nibble)
	c0 := lo.PermuteOrZeroGrouped(v0.And(nibble).BitsToInt8()).And(hi.PermuteOrZeroGrouped(h0.BitsToInt8()))
	c1 := lo.PermuteOrZeroGrouped(v1.And(nibble).BitsToInt8()).And(hi.PermuteOrZeroGrouped(h1.BitsToInt8()))
	zero := archsimd.BroadcastUint8x32(0)
	ws := archsimd.BroadcastUint8x32(stage1WhitespaceBits)
	structural := archsimd.BroadcastUint8x32(stage1StructuralBits)
	m.Whitespace = uint64(c0.And(ws).NotEqual(zero).ToBits()) | uint64(c1.And(ws).NotEqual(zero).ToBits())<<32
	m.Structural = uint64(c0.And(structural).NotEqual(zero).ToBits()) | uint64(c1.And(structural).NotEqual(zero).ToBits())<<32
	quote := archsimd.BroadcastUint8x32('"')
	slash := archsimd.BroadcastUint8x32('\\')
	ctrl := archsimd.BroadcastUint8x32(0x20)
	m.Quote = uint64(v0.Equal(quote).ToBits()) | uint64(v1.Equal(quote).ToBits())<<32
	m.Backslash = uint64(v0.Equal(slash).ToBits()) | uint64(v1.Equal(slash).ToBits())<<32
	m.Control = uint64(v0.Less(ctrl).ToBits()) | uint64(v1.Less(ctrl).ToBits())<<32
	m.NonASCII = v0.Or(v1).BitsToInt8().ToMask().ToBits() != 0
	archsimd.ClearAVXUpperBits()
}

// stage1BlockBracketsAVX2 folds each bracket pair with the ASCII case bit.
// Only the six intended bytes can match these classes.
//
//go:noinline
func stage1BlockBracketsAVX2(p *[64]byte, m *Stage1BracketMasks) {
	base := unsafe.Pointer(p)
	v0 := archsimd.LoadUint8x32Array((*[32]byte)(base))
	v1 := archsimd.LoadUint8x32Array((*[32]byte)(unsafe.Add(base, 32)))
	quote := archsimd.BroadcastUint8x32('"')
	slash := archsimd.BroadcastUint8x32('\\')
	caseBit := archsimd.BroadcastUint8x32(0x20)
	open := archsimd.BroadcastUint8x32('{')
	close := archsimd.BroadcastUint8x32('}')
	f0, f1 := v0.Or(caseBit), v1.Or(caseBit)
	m.Quote = uint64(v0.Equal(quote).ToBits()) | uint64(v1.Equal(quote).ToBits())<<32
	m.Backslash = uint64(v0.Equal(slash).ToBits()) | uint64(v1.Equal(slash).ToBits())<<32
	m.Open = uint64(f0.Equal(open).ToBits()) | uint64(f1.Equal(open).ToBits())<<32
	m.Close = uint64(f0.Equal(close).ToBits()) | uint64(f1.Equal(close).ToBits())<<32
	archsimd.ClearAVXUpperBits()
}
