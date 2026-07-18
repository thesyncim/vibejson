//go:build !go1.27 || !goexperiment.simd || (!arm64 && !amd64)

package simd

// Stage1Enabled reports whether this build provides the stage-1 classifier.
// Scalar builds use the portable SWAR kernel.
//
// Deprecated: Stage 1 is available on every supported build; this function
// always returns true.
func Stage1Enabled() bool { return true }

// Stage1Block classifies one full 64-byte block with the portable SWAR kernel.
func Stage1Block(block *[64]byte, masks *Stage1Masks) {
	stage1BlockPortable(block, masks)
}
