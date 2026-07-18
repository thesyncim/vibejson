//go:build go1.27 && !go1.28 && goexperiment.simd && arm64

package simd

import (
	"math/bits"
	"simd/archsimd"
)

// nibbleShift holds the per-halfword shift count for the shrn idiom below.
// Loading it from rodata is cheaper than materializing the vector from an
// immediate (MOVD+VMOV+VDUP) at each extraction site.
var nibbleShift = [8]int16{-4, -4, -4, -4, -4, -4, -4, -4}

// maskNibbles extracts a 4-bits-per-lane nibble mask (the aarch64 shrn
// idiom: shift halfword lanes right by four, then XTN) so one
// vector-to-GPR transfer covers all 16 lanes.
func maskNibbles(m archsimd.Mask8x16) uint64 {
	shift := archsimd.LoadInt16x8Array(&nibbleShift)
	return m.ToInt8x16().ToBits().ReshapeToUint16s().Shift(shift).TruncToUint8().ReshapeToUint64s().GetElem(0)
}

// maskLane converts a non-zero maskNibbles value to its lane index; each
// lane owns four mask bits, so the trailing-zero count divides by four.
func maskLane(nib uint64) int {
	return bits.TrailingZeros64(nib) >> 2
}

func firstMaskLane(m archsimd.Mask8x16) int {
	nib := maskNibbles(m)
	if nib == 0 {
		return -1
	}
	return maskLane(nib)
}

func maskHasAnyLane(m archsimd.Mask8x16) bool {
	return m.ToInt8x16().ToBits().ReduceMax() != 0
}

func maskNot(m archsimd.Mask8x16) archsimd.Mask8x16 {
	return m.Not()
}
