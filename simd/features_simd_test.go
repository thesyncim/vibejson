//go:build go1.27 && !go1.28 && goexperiment.simd && (arm64 || amd64)

package simd

import (
	"runtime"
	"testing"
)

func TestCurrentReportsCombinedSIMDBackends(t *testing.T) {
	info := Current()
	if runtime.GOARCH == "arm64" {
		if info.StringBackend != "arm64-neon" {
			t.Fatalf("Current().StringBackend = %q on arm64, want arm64-neon", info.StringBackend)
		}
		if info.ParseBackend != "scalar" || info.ParseVectorBytes != 0 || info.FormatBackend != "arm64-neon" || info.FormatVectorBytes != 16 {
			t.Fatalf("Current decimal backends = parse %q/%d format %q/%d on arm64, want scalar/0 and arm64-neon/16", info.ParseBackend, info.ParseVectorBytes, info.FormatBackend, info.FormatVectorBytes)
		}
	}
	if info.StringBackend != "scalar" {
		if info.StringVectorBytes < 16 || info.StringMinBytes < 16 {
			t.Fatalf("selected scanner has invalid runtime info: %+v", info)
		}
		if runtime.GOARCH == "arm64" && !info.Features.Has(CPUFeatureNEON) {
			t.Fatalf("arm64 runtime features = %v, want NEON", info.Features)
		}
		if runtime.GOARCH == "amd64" && !info.Features.Has(CPUFeatureAVX2) {
			t.Fatalf("amd64 SIMD backend features = %v, want AVX2", info.Features)
		}
	}
	if info.ParseBackend != "scalar" && info.ParseVectorBytes != 16 {
		t.Fatalf("vector parse backend %q reports vector bytes %d, want 16", info.ParseBackend, info.ParseVectorBytes)
	}
	if info.FormatBackend != "scalar" && info.FormatVectorBytes != 16 {
		t.Fatalf("vector format backend %q reports vector bytes %d, want 16", info.FormatBackend, info.FormatVectorBytes)
	}
	wantEnabled := info.StringBackend != "scalar" || info.ParseBackend != "scalar" || info.FormatBackend != "scalar"
	if info.Enabled != wantEnabled {
		t.Fatalf("Current().Enabled = %v, want %v for %+v", info.Enabled, wantEnabled, info)
	}
}
