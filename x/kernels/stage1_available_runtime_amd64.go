//go:build !go1.28 && goexperiment.simd && amd64 && !amd64.v3

package kernels

func stage1BatchAvailable() bool { return Stage1SIMDEnabled() }

// Avoid feature-dispatched math/bits calls spilling live vector constants in
// baseline binaries. Sampling uses this small inline SWAR reduction.
func stage1SamplePopcount(x uint64) int {
	x -= (x >> 1) & 0x5555555555555555
	x = (x & 0x3333333333333333) + ((x >> 2) & 0x3333333333333333)
	x = (x + (x >> 4)) & 0x0f0f0f0f0f0f0f0f
	return int(x * 0x0101010101010101 >> 56)
}
