//go:build !go1.27 || go1.28 || !goexperiment.simd || (amd64 && !amd64.v3) || (!arm64 && !amd64)

package kernels

// Stage1Block classifies one full 64-byte block with the portable SWAR kernel.
func Stage1Block(block *[64]byte, masks *Stage1Masks) {
	stage1BlockPortable(block, masks)
}
