//go:build go1.27 && !go1.28 && goexperiment.simd && amd64 && amd64.v3

package scanner

// GOAMD64=v3 and newer binaries require AVX2-capable processors, so scanner
// call sites can compile directly to the AVX2 path.
func scanAVX2Available() bool {
	return true
}
