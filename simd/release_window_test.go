//go:build vibejson_future_compiler_test && goexperiment.simd

// Run with -tags="go1.28 vibejson_future_compiler_test". Do not require
// go1.28 in this file: that would require a Go 1.28 compiler to type-check
// the test that simulates its source selection on Go 1.27.
package simd

import "testing"

func TestUnvalidatedCompilerReleaseUsesPortableBackends(t *testing.T) {
	info := Current()
	if info.Enabled || info.StringBackend != "scalar" || info.FormatBackend != "scalar" || info.StructuralBackend != "scalar" {
		t.Fatalf("Current() = %+v, want portable backends for an unvalidated compiler release", info)
	}
	if info.StringVectorBytes != 0 || info.FormatVectorBytes != 0 || info.StructuralVectorBytes != 0 {
		t.Fatalf("Current() = %+v, want no vector widths for an unvalidated compiler release", info)
	}
}
