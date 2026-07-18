//go:build go1.27 && !go1.28 && goexperiment.simd && amd64

package simd

import (
	"math/bits"
	"simd/archsimd"
)

// maskNibbles returns a one-bit-per-lane mask; the name matches the arm64
// helper whose extraction carries four bits per lane. maskLane recovers the
// lane index from a non-zero value on either encoding.
func maskNibbles(m archsimd.Mask8x16) uint64 {
	return uint64(m.ToBits())
}

// maskLane converts a non-zero maskNibbles value to its lane index.
func maskLane(nib uint64) int {
	return bits.TrailingZeros64(nib)
}

func firstMaskLane(m archsimd.Mask8x16) int {
	b := m.ToBits()
	if b == 0 {
		return -1
	}
	return bits.TrailingZeros16(b)
}

func maskHasAnyLane(m archsimd.Mask8x16) bool {
	return m.ToBits() != 0
}

func maskNot(m archsimd.Mask8x16) archsimd.Mask8x16 {
	ones := archsimd.BroadcastUint8x16(0xff)
	return m.ToInt8x16().ToBits().Xor(ones).BitsToInt8().ToMask()
}
