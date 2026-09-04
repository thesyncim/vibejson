//go:build !go1.28 && goexperiment.simd && amd64 && !amd64.v3

package kernels

import "simd/archsimd"

// Stage1Backend identifies this build's runtime-dispatched structural classifier.
// Use CurrentStage1Backend for the effective implementation on this CPU.
const Stage1Backend = "amd64-runtime"

var stage1HasAVX2 = archsimd.X86.AVX2()

// Stage1SIMDEnabled reports whether structural classification is accelerated.
func Stage1SIMDEnabled() bool { return stage1HasAVX2 }

// CurrentStage1Backend reports the effective structural classifier.
func CurrentStage1Backend() string {
	if Stage1SIMDEnabled() {
		return "amd64-avx2"
	}
	return "scalar"
}

// Stage1Block classifies one full 64-byte block using a CPU-safe static call.
func Stage1Block(p *[64]byte, m *Stage1Masks) {
	if Stage1SIMDEnabled() {
		stage1BlockAVX2(p, m)
		return
	}
	stage1BlockPortable(p, m)
}

// Stage1BlockBrackets classifies the masks needed by structural skips.
func Stage1BlockBrackets(p *[64]byte, m *Stage1BracketMasks) {
	if Stage1SIMDEnabled() {
		stage1BlockBracketsAVX2(p, m)
		return
	}
	stage1BlockBracketsPortable(p, m)
}
