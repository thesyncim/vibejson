//go:build goexperiment.simd && arm64

package slopjson

import (
	"runtime"
	"strings"
	"testing"
)

func TestDecoderStructuralTapeDropsOversizedDocument(t *testing.T) {
	src := []byte("{\n\"pad\":\"" + strings.Repeat("x", decoderStructuralTapeRetentionBytes*2) + "\"\n}")
	var tape decoderStructuralTape
	tape.build(src)
	retainedBytes := cap(tape.positions) * 4
	if retainedBytes <= decoderStructuralTapeRetentionBytes {
		t.Fatalf("test tape retained %d bytes, want more than budget %d", retainedBytes, decoderStructuralTapeRetentionBytes)
	}

	tape.resetForPool()
	src = nil
	runtime.GC()
	if tape.positions != nil {
		t.Fatalf("oversized tape retained capacity %d after release", cap(tape.positions))
	}
}

func BenchmarkStructuralTapeTinyAfterHuge(b *testing.B) {
	huge := []byte("{\n\"pad\":\"" + strings.Repeat("x", decoderStructuralTapeRetentionBytes*2) + "\"\n}")
	tiny := []byte("{\n\"pad\":\"tiny\"\n}")
	var tape decoderStructuralTape
	tape.build(huge)
	tape.resetForPool()
	tape.build(tiny)
	tape.resetForPool()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		tape.build(tiny)
		tape.resetForPool()
	}
}
