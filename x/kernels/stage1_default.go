//go:build go1.28 || !goexperiment.simd || (!arm64 && !amd64)

package kernels

// Stage1Backend identifies the structural classifier selected by this build.
const Stage1Backend = "scalar"

// Stage1Block classifies one full 64-byte block with the portable SWAR kernel.
func Stage1Block(block *[64]byte, masks *Stage1Masks) {
	stage1BlockPortable(block, masks)
}

// Stage1BlockBrackets classifies one full 64-byte block into structural-skip
// masks with the portable table-driven kernel.
func Stage1BlockBrackets(block *[64]byte, masks *Stage1BracketMasks) {
	stage1BlockBracketsPortable(block, masks)
}

// Stage1SIMDEnabled reports whether structural classification is accelerated.
func Stage1SIMDEnabled() bool { return false }

// CurrentStage1Backend reports the effective structural classifier.
func CurrentStage1Backend() string { return Stage1Backend }
