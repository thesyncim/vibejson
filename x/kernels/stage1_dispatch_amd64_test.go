//go:build !go1.28 && goexperiment.simd && amd64

package kernels

import (
	"os"
	"simd/archsimd"
	"testing"
)

func TestStage1AMD64Dispatch(t *testing.T) {
	if stage1BatchAvailable() != Stage1SIMDEnabled() {
		t.Fatal("batch dispatch differs from block dispatch")
	}
	if os.Getenv("VIBEJSON_REQUIRE_AVX2") == "1" && !Stage1SIMDEnabled() {
		t.Fatal("required AVX2 backend unavailable")
	}
	t.Logf("selected backend: %s", CurrentStage1Backend())

	want := "scalar"
	if archsimd.X86.AVX2() {
		want = "amd64-avx2"
	}
	if got := CurrentStage1Backend(); got != want {
		t.Fatalf("backend = %s, want %s", got, want)
	}
	if Stage1SIMDEnabled() != archsimd.X86.AVX2() {
		t.Fatal("dispatch does not match CPU capability")
	}
	var block [64]byte
	var masks Stage1Masks
	var brackets Stage1BracketMasks
	if allocs := testing.AllocsPerRun(100, func() {
		Stage1Block(&block, &masks)
		Stage1BlockBrackets(&block, &brackets)
	}); allocs != 0 {
		t.Fatalf("dispatch allocated %v times", allocs)
	}
}

func BenchmarkStage1Dispatch(b *testing.B) {
	var block [64]byte
	copy(block[:], `{"key": "value", "n": 12345, "flag": true, "arr": [1,2,3]}   `)
	b.Run("selected", func(b *testing.B) {
		b.SetBytes(64)
		b.ReportAllocs()
		for range b.N {
			Stage1Block(&block, &stage1BenchSink)
		}
	})
	b.Run("portable", func(b *testing.B) {
		b.SetBytes(64)
		b.ReportAllocs()
		for range b.N {
			stage1BlockPortable(&block, &stage1BenchSink)
		}
	})
}
