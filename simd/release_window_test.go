//go:build slopjson_future_compiler_test && (!go1.27 || go1.28) && goexperiment.simd

package simd

import "testing"

func TestUnvalidatedCompilerReleaseUsesPortableBackends(t *testing.T) {
	info := Current()
	if info.Enabled || info.StringBackend != "scalar" || info.FormatBackend != "scalar" {
		t.Fatalf("Current() = %+v, want portable backends for an unvalidated compiler release", info)
	}
	if info.StringVectorBytes != 0 || info.FormatVectorBytes != 0 {
		t.Fatalf("Current() = %+v, want no vector widths for an unvalidated compiler release", info)
	}
}
