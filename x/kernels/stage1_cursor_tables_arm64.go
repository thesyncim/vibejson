//go:build go1.27 && !go1.28 && goexperiment.simd && arm64

package kernels

// The forward decoder does not need colon positions: its packed key match
// validates the common case directly, and the generic cursor validates the
// raw bytes between the closing quote and value. Swapping comma and colon's
// class ranks makes colon a non-emitted separator without another SIMD
// compare or movemask. It still participates in sig, so it cannot become a
// scalar start. Treating it like whitespace in ws is harmless because ws is
// only intersected with the control-byte mask.
var stage1CursorClassLo = [16]uint8{
	1 << 0, 0, 0, 0, 0, 0, 0, 0,
	0, 1 << 1, 1<<1 | 1<<2, 1 << 5, 1 << 4, 1<<1 | 1<<6, 0, 0,
}

var stage1CursorClassHi = [16]uint8{
	1 << 1, 0, 1<<0 | 1<<4, 1 << 2, 0, 1<<5 | 1<<6, 0, 1<<5 | 1<<6,
	0, 0, 0, 0, 0, 0, 0, 0,
}
