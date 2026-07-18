//go:build go1.27 && !go1.28 && goexperiment.simd && (arm64 || amd64)

package simd

// Stage-1 structural scanning in the style of the simdjson paper: each
// 64-byte block is classified into one-bit-per-byte masks from which the
// caller derives string extents and structural positions.
//
// The production consumers read masks directly or build forward structural
// positions. Persistent index construction uses its own fused writer.

// Stage1Enabled reports whether this build provides the stage-1 kernel.
//
// Deprecated: Stage 1 is available on every supported build; this function
// always returns true.
func Stage1Enabled() bool { return true }

// stage1ClassLo and stage1ClassHi classify bytes by nibble lookup. A byte
// with low nibble l and high nibble h has class bits lo[l] & hi[h]. The
// bit products are exact: each bit's low-set x high-set cross product
// contains only the intended characters.
//
// bit 0: space (0x20)      bit 1: tab, LF, CR
// bit 2: unused            bit 3: colon
// bit 4: comma             bit 5: [ and {
// bit 6: ] and }
var stage1ClassLo = [16]uint8{
	1 << 0, 0, 0, 0, 0, 0, 0, 0,
	0, 1 << 1, 1<<1 | 1<<3, 1 << 5, 1 << 4, 1<<1 | 1<<6, 0, 0,
}

var stage1ClassHi = [16]uint8{
	1 << 1, 0, 1<<0 | 1<<4, 1 << 3, 0, 1<<5 | 1<<6, 0, 1<<5 | 1<<6,
	0, 0, 0, 0, 0, 0, 0, 0,
}

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

const (
	// Bit 2 is deliberately unused in the full table. Including it in the
	// whitespace mask makes the same value the unsigned-comparison floor used
	// by both full and colon-eliding classifiers.
	stage1WhitespaceBits = 1<<0 | 1<<1 | 1<<2
	stage1StructuralBits = 1<<3 | 1<<4 | 1<<5 | 1<<6
)
