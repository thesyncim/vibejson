//go:build !go1.28 && goexperiment.simd && amd64 && !amd64.v3

package kernels

import "testing"

func TestStage1AMD64ForcedFallback(t *testing.T) {
	original := stage1HasAVX2
	stage1HasAVX2 = false
	defer func() { stage1HasAVX2 = original }()
	if Stage1SIMDEnabled() || CurrentStage1Backend() != "scalar" {
		t.Fatal("fallback reported SIMD")
	}
	TestStage1BlockAllByteValues(t)
	TestStage1BlockBracketsAllByteValues(t)
	TestStage1BlockRandom(t)
}
