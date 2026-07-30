//go:build !go1.27 || go1.28 || !goexperiment.simd || !arm64

package vibejson

import "unsafe"

func fixed16Uint64ArrayShape(_ []byte, _ int) (count, closePosition int, ok bool) {
	return 0, 0, false
}

func parseFixed16Uint64Array(_ unsafe.Pointer, _, _ int, _ unsafe.Pointer) {
	panic("vibejson: unreachable fixed-width SIMD parser")
}
