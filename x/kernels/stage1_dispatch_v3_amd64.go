//go:build !go1.28 && goexperiment.simd && amd64.v3

package kernels

// Stage1Backend identifies the structural classifier selected by this build.
const Stage1Backend = "amd64-avx2"

// Stage1SIMDEnabled is constant-folded for binaries that require AVX2.
func Stage1SIMDEnabled() bool { return true }

// CurrentStage1Backend reports the effective structural classifier.
func CurrentStage1Backend() string { return Stage1Backend }

// Stage1Block classifies one full 64-byte block with AVX2.
func Stage1Block(p *[64]byte, m *Stage1Masks) { stage1BlockAVX2(p, m) }

// Stage1BlockBrackets classifies the masks needed by structural skips.
func Stage1BlockBrackets(p *[64]byte, m *Stage1BracketMasks) { stage1BlockBracketsAVX2(p, m) }
