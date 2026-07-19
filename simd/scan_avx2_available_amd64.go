//go:build go1.27 && !go1.28 && goexperiment.simd && amd64 && !amd64.v3

package simd

// scanAVX2Available is runtime-selected for binaries that may run on pre-AVX2
// processors. The helper inlines into scanner call sites.
func scanAVX2Available() bool {
	return scanAMD64Level == scanLevelAVX2
}
