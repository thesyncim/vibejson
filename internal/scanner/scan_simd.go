//go:build go1.27 && !go1.28 && goexperiment.simd && (arm64 || amd64)

package scanner

import (
	"encoding/binary"
	"math/bits"
	"simd/archsimd"
	"unicode/utf8"
	"unsafe"
)

// The dispatch thresholds start at max int, which disables the probe and
// vector stages until initStringScanner installs backend-specific values for a
// detected backend; without one the dispatchers run only their word loops.
var (
	scanStringSpecialBackend   = "scalar"
	scanStringSelectedMinBytes = int(^uint(0) >> 1)
	scanStringProbeMinBytes    = int(^uint(0) >> 1)
	scanStringVectorBytes      int
)

// scanEncodedHTMLMinBytes gates the HTML scanners straight into the vector
// kernels at one full block. They have no SWAR probe stage: each word mask
// must test three extra characters, roughly doubling its cost.
const scanEncodedHTMLMinBytes = 16

// A \uXXXX escape is six bytes, so escape structure repeats every
// lcm(6, 16) = 48 bytes. The three phase-shifted rows of each table below
// describe one 16-byte vector of that period: where the literal "\u" bytes
// fall, which lanes must hold hex digits, and which lane is the first hex
// digit of an escape (the surrogate probe position).
var unicodeEscapeExpected = [...][16]uint8{
	{'\\', 'u', 0, 0, 0, 0, '\\', 'u', 0, 0, 0, 0, '\\', 'u', 0, 0},
	{0, 0, '\\', 'u', 0, 0, 0, 0, '\\', 'u', 0, 0, 0, 0, '\\', 'u'},
	{0, 0, 0, 0, '\\', 'u', 0, 0, 0, 0, '\\', 'u', 0, 0, 0, 0},
}

var unicodeEscapeHexMasks = [...][16]int8{
	{0, 0, -1, -1, -1, -1, 0, 0, -1, -1, -1, -1, 0, 0, -1, -1},
	{-1, -1, 0, 0, -1, -1, -1, -1, 0, 0, -1, -1, -1, -1, 0, 0},
	{-1, -1, -1, -1, 0, 0, -1, -1, -1, -1, 0, 0, -1, -1, -1, -1},
}

var unicodeEscapeFirstMasks = [...][16]int8{
	{0, 0, -1, 0, 0, 0, 0, 0, -1, 0, 0, 0, 0, 0, -1, 0},
	{0, 0, 0, 0, -1, 0, 0, 0, 0, 0, -1, 0, 0, 0, 0, 0},
	{-1, 0, 0, 0, 0, 0, -1, 0, 0, 0, 0, 0, -1, 0, 0, 0},
}

func init() {
	initStringScanner()
}

func currentInfo() Info {
	return Info{
		Enabled:     scanStringSpecialBackend != "scalar",
		Backend:     scanStringSpecialBackend,
		VectorBytes: scanStringVectorBytes,
		MinBytes:    scanStringSelectedMinBytes,
	}
}

func scanStringSpecialLong(src []byte, i int) int {
	return scanStringSpecialRuntime(src, i)
}

// scanStringSyntax is scanStringSpecial without the non-ASCII stop class,
// staged the same way. Callers use it when multi-byte runes need no
// individual inspection: either the string is already known to be clean
// UTF-8, or the caller validates the skipped run afterwards.
func scanStringSyntax(src []byte, i int) int {
	start := i
	remaining := len(src) - i
	if remaining >= scanStringProbeMinBytes {
		if m := stringSyntaxMask(binary.LittleEndian.Uint64(src[i:])); m != 0 {
			return i + bits.TrailingZeros64(m)/8
		}
		i += 8
		if m := stringSyntaxMask(binary.LittleEndian.Uint64(src[i:])); m != 0 {
			return i + bits.TrailingZeros64(m)/8
		}
		i += 8
	}
	if len(src)-i >= scanStringSelectedMinBytes {
		return scanStringSyntaxRuntime(src, i)
	}
	for i+8 <= len(src) {
		if m := stringSyntaxMask(binary.LittleEndian.Uint64(src[i:])); m != 0 {
			return i + bits.TrailingZeros64(m)/8
		}
		i += 8
	}
	if i == len(src) {
		return i
	}
	if tail := len(src) - 8; tail >= start {
		// Overlapped final word: see scanStringSpecial.
		if m := stringSyntaxMask(binary.LittleEndian.Uint64(src[tail:])); m != 0 {
			return tail + bits.TrailingZeros64(m)/8
		}
		return len(src)
	}
	for i < len(src) {
		c := src[i]
		if c == '"' || c == '\\' || c < 0x20 {
			return i
		}
		i++
	}
	return len(src)
}

func scanStringSpecialSIMD(src []byte, i int) int {
	n := len(src)
	start := i
	quote := archsimd.BroadcastUint8x16('"')
	slash := archsimd.BroadcastUint8x16('\\')
	ctrlOrNonASCII := archsimd.BroadcastInt8x16(0x20)
	base := unsafe.Pointer(unsafe.SliceData(src))

	// Ramp: the span is the remaining document, so the match usually lands
	// near the entry point. Scan one single block before committing to
	// 64-byte strides.
	for k := 0; k < 1 && i+16 <= n; k++ {
		v := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i)))
		m := v.Equal(quote).
			Or(v.Equal(slash)).
			Or(v.BitsToInt8().Less(ctrlOrNonASCII))
		if nib := maskNibbles(m); nib != 0 {
			return i + maskLane(nib)
		}
		i += 16
	}

	for i+64 <= n {
		v0 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i)))
		v1 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i+16)))
		v2 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i+32)))
		v3 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i+48)))

		m0 := v0.Equal(quote).
			Or(v0.Equal(slash)).
			Or(v0.BitsToInt8().Less(ctrlOrNonASCII))
		m1 := v1.Equal(quote).
			Or(v1.Equal(slash)).
			Or(v1.BitsToInt8().Less(ctrlOrNonASCII))
		m2 := v2.Equal(quote).
			Or(v2.Equal(slash)).
			Or(v2.BitsToInt8().Less(ctrlOrNonASCII))
		m3 := v3.Equal(quote).
			Or(v3.Equal(slash)).
			Or(v3.BitsToInt8().Less(ctrlOrNonASCII))

		if maskHasAnyLane(m0.Or(m1).Or(m2).Or(m3)) {
			// Halve first so a hit costs at most one reduce plus two
			// extractions regardless of which vector holds it.
			if maskHasAnyLane(m0.Or(m1)) {
				if nib := maskNibbles(m0); nib != 0 {
					return i + maskLane(nib)
				}
				return i + 16 + maskLane(maskNibbles(m1))
			}
			if nib := maskNibbles(m2); nib != 0 {
				return i + 32 + maskLane(nib)
			}
			return i + 48 + maskLane(maskNibbles(m3))
		}
		i += 64
	}

	for i+16 <= n {
		v := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i)))
		m := v.Equal(quote).
			Or(v.Equal(slash)).
			Or(v.BitsToInt8().Less(ctrlOrNonASCII))
		if maskHasAnyLane(m) {
			return i + firstMaskLane(m)
		}
		i += 16
	}
	if i < n && n-16 >= start {
		// Overlapped tail: one final block ending at n. The bytes in
		// [tail, i) were already cleared above, and guarding on start keeps
		// the load inside this call's span — bytes before start were never
		// cleared — so the first match found is at or after i.
		tail := n - 16
		v := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, tail)))
		m := v.Equal(quote).
			Or(v.Equal(slash)).
			Or(v.BitsToInt8().Less(ctrlOrNonASCII))
		if lane := firstMaskLane(m); lane >= 0 {
			return tail + lane
		}
		return n
	}
	return scanStringSpecialScalar(src, i)
}

func scanStringSyntaxSIMD(src []byte, i int) int {
	n := len(src)
	start := i
	quote := archsimd.BroadcastUint8x16('"')
	slash := archsimd.BroadcastUint8x16('\\')
	ctrl := archsimd.BroadcastUint8x16(0x20)
	base := unsafe.Pointer(unsafe.SliceData(src))

	// Ramp: see scanStringSpecialSIMD.
	for k := 0; k < 1 && i+16 <= n; k++ {
		v := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i)))
		m := v.Equal(quote).Or(v.Equal(slash)).Or(v.Less(ctrl))
		if nib := maskNibbles(m); nib != 0 {
			return i + maskLane(nib)
		}
		i += 16
	}

	for i+64 <= n {
		v0 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i)))
		v1 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i+16)))
		v2 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i+32)))
		v3 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i+48)))

		m0 := v0.Equal(quote).Or(v0.Equal(slash)).Or(v0.Less(ctrl))
		m1 := v1.Equal(quote).Or(v1.Equal(slash)).Or(v1.Less(ctrl))
		m2 := v2.Equal(quote).Or(v2.Equal(slash)).Or(v2.Less(ctrl))
		m3 := v3.Equal(quote).Or(v3.Equal(slash)).Or(v3.Less(ctrl))

		if maskHasAnyLane(m0.Or(m1).Or(m2).Or(m3)) {
			// Halve first so a hit costs at most one reduce plus two
			// extractions regardless of which vector holds it.
			if maskHasAnyLane(m0.Or(m1)) {
				if nib := maskNibbles(m0); nib != 0 {
					return i + maskLane(nib)
				}
				return i + 16 + maskLane(maskNibbles(m1))
			}
			if nib := maskNibbles(m2); nib != 0 {
				return i + 32 + maskLane(nib)
			}
			return i + 48 + maskLane(maskNibbles(m3))
		}
		i += 64
	}

	for i+16 <= n {
		v := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i)))
		m := v.Equal(quote).Or(v.Equal(slash)).Or(v.Less(ctrl))
		if maskHasAnyLane(m) {
			return i + firstMaskLane(m)
		}
		i += 16
	}
	if i < n && n-16 >= start {
		// Overlapped tail: see scanStringSpecialSIMD.
		tail := n - 16
		v := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, tail)))
		m := v.Equal(quote).Or(v.Equal(slash)).Or(v.Less(ctrl))
		if lane := firstMaskLane(m); lane >= 0 {
			return tail + lane
		}
		return n
	}
	return scanStringSyntaxScalar(src, i)
}

func scanEncodedHTMLSpecialFast(src []byte, i int) int {
	if len(src)-i >= scanEncodedHTMLMinBytes {
		return scanEncodedHTMLSpecialRuntime(src, i)
	}
	return scanEncodedHTMLSpecialScalar(src, i)
}

func scanEncodedHTMLSyntaxFast(src []byte, i int) int {
	if len(src)-i >= scanEncodedHTMLMinBytes {
		return scanEncodedHTMLSyntaxRuntime(src, i)
	}
	return scanEncodedHTMLSyntaxScalar(src, i)
}

func scanEncodedHTMLSpecialSIMD(src []byte, i int) int {
	n := len(src)
	start := i
	slash := archsimd.BroadcastUint8x16('\\')
	gt := archsimd.BroadcastUint8x16('>')
	amp := archsimd.BroadcastUint8x16('&')
	bit2 := archsimd.BroadcastUint8x16(2)
	bit4 := archsimd.BroadcastUint8x16(4)
	ctrlOrNonASCII := archsimd.BroadcastInt8x16(0x20)
	base := unsafe.Pointer(unsafe.SliceData(src))

	// Ramp: see scanStringSpecialSIMD.
	for k := 0; k < 1 && i+16 <= n; k++ {
		v := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i)))
		if nib := maskNibbles(encodedHTMLSpecialMask(v, slash, gt, amp, bit2, bit4, ctrlOrNonASCII)); nib != 0 {
			return i + maskLane(nib)
		}
		i += 16
	}

	for i+64 <= n {
		v0 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i)))
		v1 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i+16)))
		v2 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i+32)))
		v3 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i+48)))
		m0 := encodedHTMLSpecialMask(v0, slash, gt, amp, bit2, bit4, ctrlOrNonASCII)
		m1 := encodedHTMLSpecialMask(v1, slash, gt, amp, bit2, bit4, ctrlOrNonASCII)
		m2 := encodedHTMLSpecialMask(v2, slash, gt, amp, bit2, bit4, ctrlOrNonASCII)
		m3 := encodedHTMLSpecialMask(v3, slash, gt, amp, bit2, bit4, ctrlOrNonASCII)
		if maskHasAnyLane(m0.Or(m1).Or(m2).Or(m3)) {
			// Halve first so a hit costs at most one reduce plus two
			// extractions regardless of which vector holds it.
			if maskHasAnyLane(m0.Or(m1)) {
				if nib := maskNibbles(m0); nib != 0 {
					return i + maskLane(nib)
				}
				return i + 16 + maskLane(maskNibbles(m1))
			}
			if nib := maskNibbles(m2); nib != 0 {
				return i + 32 + maskLane(nib)
			}
			return i + 48 + maskLane(maskNibbles(m3))
		}
		i += 64
	}
	for i+16 <= n {
		v := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i)))
		if m := encodedHTMLSpecialMask(v, slash, gt, amp, bit2, bit4, ctrlOrNonASCII); maskHasAnyLane(m) {
			return i + firstMaskLane(m)
		}
		i += 16
	}
	if i < n && n-16 >= start {
		// Overlapped tail: see scanStringSpecialSIMD.
		tail := n - 16
		v := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, tail)))
		if lane := firstMaskLane(encodedHTMLSpecialMask(v, slash, gt, amp, bit2, bit4, ctrlOrNonASCII)); lane >= 0 {
			return tail + lane
		}
		return n
	}
	return scanEncodedHTMLSpecialScalar(src, i)
}

func encodedHTMLSpecialMask(v, slash, gt, amp, bit2, bit4 archsimd.Uint8x16, ctrlOrNonASCII archsimd.Int8x16) archsimd.Mask8x16 {
	quoteOrAmp := v.Or(bit4).Equal(amp)
	angle := v.Or(bit2).Equal(gt)
	return quoteOrAmp.
		Or(angle).
		Or(v.Equal(slash)).
		Or(v.BitsToInt8().Less(ctrlOrNonASCII))
}

func scanEncodedHTMLSyntaxSIMD(src []byte, i int) int {
	n := len(src)
	start := i
	slash := archsimd.BroadcastUint8x16('\\')
	gt := archsimd.BroadcastUint8x16('>')
	amp := archsimd.BroadcastUint8x16('&')
	bit2 := archsimd.BroadcastUint8x16(2)
	bit4 := archsimd.BroadcastUint8x16(4)
	ctrl := archsimd.BroadcastUint8x16(0x20)
	base := unsafe.Pointer(unsafe.SliceData(src))

	// Ramp: see scanStringSpecialSIMD.
	for k := 0; k < 1 && i+16 <= n; k++ {
		v := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i)))
		if nib := maskNibbles(encodedHTMLSyntaxMask(v, slash, gt, amp, bit2, bit4, ctrl)); nib != 0 {
			return i + maskLane(nib)
		}
		i += 16
	}

	for i+64 <= n {
		v0 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i)))
		v1 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i+16)))
		v2 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i+32)))
		v3 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i+48)))
		m0 := encodedHTMLSyntaxMask(v0, slash, gt, amp, bit2, bit4, ctrl)
		m1 := encodedHTMLSyntaxMask(v1, slash, gt, amp, bit2, bit4, ctrl)
		m2 := encodedHTMLSyntaxMask(v2, slash, gt, amp, bit2, bit4, ctrl)
		m3 := encodedHTMLSyntaxMask(v3, slash, gt, amp, bit2, bit4, ctrl)
		if maskHasAnyLane(m0.Or(m1).Or(m2).Or(m3)) {
			// Halve first so a hit costs at most one reduce plus two
			// extractions regardless of which vector holds it.
			if maskHasAnyLane(m0.Or(m1)) {
				if nib := maskNibbles(m0); nib != 0 {
					return i + maskLane(nib)
				}
				return i + 16 + maskLane(maskNibbles(m1))
			}
			if nib := maskNibbles(m2); nib != 0 {
				return i + 32 + maskLane(nib)
			}
			return i + 48 + maskLane(maskNibbles(m3))
		}
		i += 64
	}
	for i+16 <= n {
		v := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i)))
		if m := encodedHTMLSyntaxMask(v, slash, gt, amp, bit2, bit4, ctrl); maskHasAnyLane(m) {
			return i + firstMaskLane(m)
		}
		i += 16
	}
	if i < n && n-16 >= start {
		// Overlapped tail: see scanStringSpecialSIMD.
		tail := n - 16
		v := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, tail)))
		if lane := firstMaskLane(encodedHTMLSyntaxMask(v, slash, gt, amp, bit2, bit4, ctrl)); lane >= 0 {
			return tail + lane
		}
		return n
	}
	return scanEncodedHTMLSyntaxScalar(src, i)
}

func encodedHTMLSyntaxMask(v, slash, gt, amp, bit2, bit4, ctrl archsimd.Uint8x16) archsimd.Mask8x16 {
	quoteOrAmp := v.Or(bit4).Equal(amp)
	angle := v.Or(bit2).Equal(gt)
	return quoteOrAmp.
		Or(angle).
		Or(v.Equal(slash)).
		Or(v.Less(ctrl))
}

func copyStringPrefix(dst, src []byte) int {
	quote := archsimd.BroadcastUint8x16('"')
	slash := archsimd.BroadcastUint8x16('\\')
	ctrlOrNonASCII := archsimd.BroadcastInt8x16(0x20)
	srcBase := unsafe.Pointer(unsafe.SliceData(src))
	dstBase := unsafe.Pointer(unsafe.SliceData(dst))
	i := 0
	for i+64 <= len(src) {
		v0 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(srcBase, i)))
		v1 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(srcBase, i+16)))
		v2 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(srcBase, i+32)))
		v3 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(srcBase, i+48)))
		m0 := v0.Equal(quote).Or(v0.Equal(slash)).Or(v0.BitsToInt8().Less(ctrlOrNonASCII))
		m1 := v1.Equal(quote).Or(v1.Equal(slash)).Or(v1.BitsToInt8().Less(ctrlOrNonASCII))
		m2 := v2.Equal(quote).Or(v2.Equal(slash)).Or(v2.BitsToInt8().Less(ctrlOrNonASCII))
		m3 := v3.Equal(quote).Or(v3.Equal(slash)).Or(v3.BitsToInt8().Less(ctrlOrNonASCII))
		if maskHasAnyLane(m0.Or(m1).Or(m2).Or(m3)) {
			if lane := firstMaskLane(m0); lane >= 0 {
				copy(dst[i:], src[i:i+lane])
				return i + lane
			}
			v0.StoreArray((*[16]uint8)(unsafe.Add(dstBase, i)))
			if lane := firstMaskLane(m1); lane >= 0 {
				copy(dst[i+16:], src[i+16:i+16+lane])
				return i + 16 + lane
			}
			v1.StoreArray((*[16]uint8)(unsafe.Add(dstBase, i+16)))
			if lane := firstMaskLane(m2); lane >= 0 {
				copy(dst[i+32:], src[i+32:i+32+lane])
				return i + 32 + lane
			}
			v2.StoreArray((*[16]uint8)(unsafe.Add(dstBase, i+32)))
			lane := firstMaskLane(m3)
			copy(dst[i+48:], src[i+48:i+48+lane])
			return i + 48 + lane
		}
		v0.StoreArray((*[16]uint8)(unsafe.Add(dstBase, i)))
		v1.StoreArray((*[16]uint8)(unsafe.Add(dstBase, i+16)))
		v2.StoreArray((*[16]uint8)(unsafe.Add(dstBase, i+32)))
		v3.StoreArray((*[16]uint8)(unsafe.Add(dstBase, i+48)))
		i += 64
	}
	for i+16 <= len(src) {
		v := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(srcBase, i)))
		m := v.Equal(quote).Or(v.Equal(slash)).Or(v.BitsToInt8().Less(ctrlOrNonASCII))
		if maskHasAnyLane(m) {
			lane := firstMaskLane(m)
			copy(dst[i:], src[i:i+lane])
			return i + lane
		}
		v.StoreArray((*[16]uint8)(unsafe.Add(dstBase, i)))
		i += 16
	}
	if i < len(src) && i >= 16 {
		tail := len(src) - 16
		v := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(srcBase, tail)))
		m := v.Equal(quote).Or(v.Equal(slash)).Or(v.BitsToInt8().Less(ctrlOrNonASCII))
		if lane := firstMaskLane(m); lane >= 0 {
			end := tail + lane
			copy(dst[i:end], src[i:end])
			return end
		}
		v.StoreArray((*[16]uint8)(unsafe.Add(dstBase, tail)))
		return len(src)
	}
	end := scanStringSpecialScalar(src, i)
	copy(dst[i:], src[i:end])
	return end
}

func copyHTMLStringPrefix(dst, src []byte) int {
	slash := archsimd.BroadcastUint8x16('\\')
	gt := archsimd.BroadcastUint8x16('>')
	amp := archsimd.BroadcastUint8x16('&')
	bit2 := archsimd.BroadcastUint8x16(2)
	bit4 := archsimd.BroadcastUint8x16(4)
	ctrlOrNonASCII := archsimd.BroadcastInt8x16(0x20)
	srcBase := unsafe.Pointer(unsafe.SliceData(src))
	dstBase := unsafe.Pointer(unsafe.SliceData(dst))
	i := 0
	for i+64 <= len(src) {
		v0 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(srcBase, i)))
		v1 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(srcBase, i+16)))
		v2 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(srcBase, i+32)))
		v3 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(srcBase, i+48)))
		m0 := encodedHTMLSpecialMask(v0, slash, gt, amp, bit2, bit4, ctrlOrNonASCII)
		m1 := encodedHTMLSpecialMask(v1, slash, gt, amp, bit2, bit4, ctrlOrNonASCII)
		m2 := encodedHTMLSpecialMask(v2, slash, gt, amp, bit2, bit4, ctrlOrNonASCII)
		m3 := encodedHTMLSpecialMask(v3, slash, gt, amp, bit2, bit4, ctrlOrNonASCII)
		if maskHasAnyLane(m0.Or(m1).Or(m2).Or(m3)) {
			if lane := firstMaskLane(m0); lane >= 0 {
				copy(dst[i:], src[i:i+lane])
				return i + lane
			}
			v0.StoreArray((*[16]uint8)(unsafe.Add(dstBase, i)))
			if lane := firstMaskLane(m1); lane >= 0 {
				copy(dst[i+16:], src[i+16:i+16+lane])
				return i + 16 + lane
			}
			v1.StoreArray((*[16]uint8)(unsafe.Add(dstBase, i+16)))
			if lane := firstMaskLane(m2); lane >= 0 {
				copy(dst[i+32:], src[i+32:i+32+lane])
				return i + 32 + lane
			}
			v2.StoreArray((*[16]uint8)(unsafe.Add(dstBase, i+32)))
			lane := firstMaskLane(m3)
			copy(dst[i+48:], src[i+48:i+48+lane])
			return i + 48 + lane
		}
		v0.StoreArray((*[16]uint8)(unsafe.Add(dstBase, i)))
		v1.StoreArray((*[16]uint8)(unsafe.Add(dstBase, i+16)))
		v2.StoreArray((*[16]uint8)(unsafe.Add(dstBase, i+32)))
		v3.StoreArray((*[16]uint8)(unsafe.Add(dstBase, i+48)))
		i += 64
	}
	for i+16 <= len(src) {
		v := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(srcBase, i)))
		m := encodedHTMLSpecialMask(v, slash, gt, amp, bit2, bit4, ctrlOrNonASCII)
		if maskHasAnyLane(m) {
			lane := firstMaskLane(m)
			copy(dst[i:], src[i:i+lane])
			return i + lane
		}
		v.StoreArray((*[16]uint8)(unsafe.Add(dstBase, i)))
		i += 16
	}
	if i < len(src) && i >= 16 {
		tail := len(src) - 16
		v := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(srcBase, tail)))
		m := encodedHTMLSpecialMask(v, slash, gt, amp, bit2, bit4, ctrlOrNonASCII)
		if lane := firstMaskLane(m); lane >= 0 {
			end := tail + lane
			copy(dst[i:end], src[i:end])
			return end
		}
		v.StoreArray((*[16]uint8)(unsafe.Add(dstBase, tail)))
		return len(src)
	}
	end := scanEncodedHTMLSpecialScalar(src, i)
	copy(dst[i:], src[i:end])
	return end
}

// scanUnicodeEscapeRun validates complete groups of eight contiguous
// \uXXXX escapes. It stops before a partial block and before any escape
// whose first hex digit is d or D — the 0xDxxx range holding the
// surrogates — so the scalar path can preserve precise pair semantics.
func scanUnicodeEscapeRun(src []byte, i int) (int, bool) {
	if len(src)-i < 48 || src[i] != '\\' || src[i+1] != 'u' {
		return i, true
	}
	base := unsafe.Pointer(unsafe.SliceData(src))
	zero := archsimd.BroadcastUint8x16('0')
	ten := archsimd.BroadcastUint8x16(10)
	lower := archsimd.BroadcastUint8x16(0x20)
	a := archsimd.BroadcastUint8x16('a')
	six := archsimd.BroadcastUint8x16(6)
	d := archsimd.BroadcastUint8x16('d')
	expected0 := archsimd.LoadUint8x16Array(&unicodeEscapeExpected[0])
	expected1 := archsimd.LoadUint8x16Array(&unicodeEscapeExpected[1])
	expected2 := archsimd.LoadUint8x16Array(&unicodeEscapeExpected[2])
	hexMask0 := archsimd.LoadInt8x16Array(&unicodeEscapeHexMasks[0]).ToMask()
	hexMask1 := archsimd.LoadInt8x16Array(&unicodeEscapeHexMasks[1]).ToMask()
	hexMask2 := archsimd.LoadInt8x16Array(&unicodeEscapeHexMasks[2]).ToMask()
	firstMask0 := archsimd.LoadInt8x16Array(&unicodeEscapeFirstMasks[0]).ToMask()
	firstMask1 := archsimd.LoadInt8x16Array(&unicodeEscapeFirstMasks[1]).ToMask()
	firstMask2 := archsimd.LoadInt8x16Array(&unicodeEscapeFirstMasks[2]).ToMask()

	for i+48 <= len(src) {
		v0 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i)))
		v1 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i+16)))
		v2 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i+32)))
		prefix0, invalid0, surrogate0 := unicodeEscapeVectorMasks(v0, expected0, hexMask0, firstMask0, zero, ten, lower, a, six, d)
		prefix1, invalid1, surrogate1 := unicodeEscapeVectorMasks(v1, expected1, hexMask1, firstMask1, zero, ten, lower, a, six, d)
		prefix2, invalid2, surrogate2 := unicodeEscapeVectorMasks(v2, expected2, hexMask2, firstMask2, zero, ten, lower, a, six, d)
		if maskHasAnyLane(prefix0.Or(prefix1).Or(prefix2)) {
			return i, true
		}
		if maskHasAnyLane(invalid0.Or(invalid1).Or(invalid2).
			Or(surrogate0).Or(surrogate1).Or(surrogate2)) {
			return i, true
		}
		i += 48
	}
	return i, true
}

func unicodeEscapeVectorMasks(v, expected archsimd.Uint8x16, hexMask, firstMask archsimd.Mask8x16, zero, ten, lower, a, six, d archsimd.Uint8x16) (prefix, invalid, surrogate archsimd.Mask8x16) {
	prefix = maskNot(v.Equal(expected).Or(hexMask))
	hex := v.Sub(zero).Less(ten).Or(v.Or(lower).Sub(a).Less(six))
	invalid = maskNot(hex).And(hexMask)
	surrogate = v.Or(lower).Equal(d).And(firstMask)
	return prefix, invalid, surrogate
}

func validUTF8Fast(src []byte) bool {
	return validUTF8Runtime(src)
}

// validUTF8NoLineSeparatorFast combines the two predicates needed by the
// encoder so each full vector is loaded once. It reports false for malformed
// UTF-8 or for U+2028/U+2029, both of which require the scalar escaping path.
func validUTF8NoLineSeparatorFast(src []byte) bool {
	return validUTF8NoLineSeparatorRuntime(src)
}

// validUTF8NoLineSeparatorGeneric is the range-compare implementation used
// where no faster arch-specific kernel exists.
func validUTF8NoLineSeparatorGeneric(src []byte) bool {
	if len(src) < 16 {
		return utf8.Valid(src) && !hasJSONLineSeparatorScalar(src, 0)
	}
	base := unsafe.Pointer(unsafe.SliceData(src))
	b80 := archsimd.BroadcastUint8x16(0x80)
	b90 := archsimd.BroadcastUint8x16(0x90)
	ba0 := archsimd.BroadcastUint8x16(0xa0)
	bc0 := archsimd.BroadcastUint8x16(0xc0)
	bc2 := archsimd.BroadcastUint8x16(0xc2)
	be0 := archsimd.BroadcastUint8x16(0xe0)
	bed := archsimd.BroadcastUint8x16(0xed)
	bf0 := archsimd.BroadcastUint8x16(0xf0)
	bf4 := archsimd.BroadcastUint8x16(0xf4)
	bf5 := archsimd.BroadcastUint8x16(0xf5)
	e2 := archsimd.BroadcastUint8x16(0xe2)
	a8 := archsimd.BroadcastUint8x16(0xa8)
	a9 := archsimd.BroadcastUint8x16(0xa9)
	zero := archsimd.BroadcastUint8x16(0)
	prevLead := zero
	prevLead34 := zero
	prevLead4 := zero
	prevE0 := zero
	prevED := zero
	prevF0 := zero
	prevF4 := zero
	prevE2 := zero
	prev80 := zero

	i := 0
	for i+16 <= len(src) {
		v := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i)))
		continuation := v.GreaterEqual(b80).And(v.Less(bc0))
		lead2 := v.GreaterEqual(bc2).And(v.Less(be0))
		lead3 := v.GreaterEqual(be0).And(v.Less(bf0))
		lead4 := v.GreaterEqual(bf0).And(v.Less(bf5))
		invalid := v.GreaterEqual(bc0).And(v.Less(bc2)).Or(v.GreaterEqual(bf5))

		lead := lead2.Or(lead3).Or(lead4).ToInt8x16().ToBits()
		lead34 := lead3.Or(lead4).ToInt8x16().ToBits()
		lead4Bytes := lead4.ToInt8x16().ToBits()
		expected := lead.ConcatShiftBytesRight(prevLead, 15).
			Or(lead34.ConcatShiftBytesRight(prevLead34, 14)).
			Or(lead4Bytes.ConcatShiftBytesRight(prevLead4, 13))
		actual := continuation.ToInt8x16().ToBits()
		invalid = invalid.Or(actual.NotEqual(expected))

		e0Bits := v.Equal(be0).ToInt8x16().ToBits()
		edBits := v.Equal(bed).ToInt8x16().ToBits()
		f0Bits := v.Equal(bf0).ToInt8x16().ToBits()
		f4Bits := v.Equal(bf4).ToInt8x16().ToBits()
		afterE0 := e0Bits.ConcatShiftBytesRight(prevE0, 15).BitsToInt8().ToMask()
		afterED := edBits.ConcatShiftBytesRight(prevED, 15).BitsToInt8().ToMask()
		afterF0 := f0Bits.ConcatShiftBytesRight(prevF0, 15).BitsToInt8().ToMask()
		afterF4 := f4Bits.ConcatShiftBytesRight(prevF4, 15).BitsToInt8().ToMask()
		invalid = invalid.Or(afterE0.And(v.Less(ba0)).
			Or(afterED.And(v.GreaterEqual(ba0))).
			Or(afterF0.And(v.Less(b90))).
			Or(afterF4.And(v.GreaterEqual(b90))))

		e2Bits := v.Equal(e2).ToInt8x16().ToBits()
		b80Bits := v.Equal(b80).ToInt8x16().ToBits()
		precededByE280 := e2Bits.ConcatShiftBytesRight(prevE2, 14).
			And(b80Bits.ConcatShiftBytesRight(prev80, 15))
		lineEnd := v.Equal(a8).Or(v.Equal(a9)).ToInt8x16().ToBits()
		invalid = invalid.Or(lineEnd.And(precededByE280).BitsToInt8().ToMask())
		if maskHasAnyLane(invalid) {
			return false
		}

		prevLead = lead
		prevLead34 = lead34
		prevLead4 = lead4Bytes
		prevE0 = e0Bits
		prevED = edBits
		prevF0 = f0Bits
		prevF4 = f4Bits
		prevE2 = e2Bits
		prev80 = b80Bits
		i += 16
	}

	// One zero-padded final block continues the streamed state: the padding
	// reads as ASCII NUL, so a multi-byte sequence dangling at the true end
	// of input surfaces as a missing continuation, and the carried prevE2 and
	// prev80 vectors keep U+2028/U+2029 detection working across the
	// boundary. It runs even when the input ends exactly on a block boundary,
	// where an all-zero block plays the same role for a sequence dangling out
	// of the last full block.
	var tailBlock [16]uint8
	copy(tailBlock[:], src[i:])
	v := archsimd.LoadUint8x16Array(&tailBlock)
	continuation := v.GreaterEqual(b80).And(v.Less(bc0))
	lead2 := v.GreaterEqual(bc2).And(v.Less(be0))
	lead3 := v.GreaterEqual(be0).And(v.Less(bf0))
	lead4 := v.GreaterEqual(bf0).And(v.Less(bf5))
	invalid := v.GreaterEqual(bc0).And(v.Less(bc2)).Or(v.GreaterEqual(bf5))

	lead := lead2.Or(lead3).Or(lead4).ToInt8x16().ToBits()
	lead34 := lead3.Or(lead4).ToInt8x16().ToBits()
	lead4Bytes := lead4.ToInt8x16().ToBits()
	expected := lead.ConcatShiftBytesRight(prevLead, 15).
		Or(lead34.ConcatShiftBytesRight(prevLead34, 14)).
		Or(lead4Bytes.ConcatShiftBytesRight(prevLead4, 13))
	actual := continuation.ToInt8x16().ToBits()
	invalid = invalid.Or(actual.NotEqual(expected))

	afterE0 := v.Equal(be0).ToInt8x16().ToBits().ConcatShiftBytesRight(prevE0, 15).BitsToInt8().ToMask()
	afterED := v.Equal(bed).ToInt8x16().ToBits().ConcatShiftBytesRight(prevED, 15).BitsToInt8().ToMask()
	afterF0 := v.Equal(bf0).ToInt8x16().ToBits().ConcatShiftBytesRight(prevF0, 15).BitsToInt8().ToMask()
	afterF4 := v.Equal(bf4).ToInt8x16().ToBits().ConcatShiftBytesRight(prevF4, 15).BitsToInt8().ToMask()
	invalid = invalid.Or(afterE0.And(v.Less(ba0)).
		Or(afterED.And(v.GreaterEqual(ba0))).
		Or(afterF0.And(v.Less(b90))).
		Or(afterF4.And(v.GreaterEqual(b90))))

	precededByE280 := v.Equal(e2).ToInt8x16().ToBits().ConcatShiftBytesRight(prevE2, 14).
		And(v.Equal(b80).ToInt8x16().ToBits().ConcatShiftBytesRight(prev80, 15))
	lineEnd := v.Equal(a8).Or(v.Equal(a9)).ToInt8x16().ToBits()
	invalid = invalid.Or(lineEnd.And(precededByE280).BitsToInt8().ToMask())
	return !maskHasAnyLane(invalid)
}

func hasJSONLineSeparatorFast(src []byte, start int) bool {
	if len(src)-start < 16 {
		return hasJSONLineSeparatorScalar(src, start)
	}
	base := unsafe.Pointer(unsafe.SliceData(src))
	e2 := archsimd.BroadcastUint8x16(0xe2)
	b80 := archsimd.BroadcastUint8x16(0x80)
	a8 := archsimd.BroadcastUint8x16(0xa8)
	a9 := archsimd.BroadcastUint8x16(0xa9)
	zero := archsimd.BroadcastUint8x16(0)
	prevE2 := zero
	prev80 := zero
	i := start
	for i+16 <= len(src) {
		v := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, i)))
		e2Bits := v.Equal(e2).ToInt8x16().ToBits()
		b80Bits := v.Equal(b80).ToInt8x16().ToBits()
		precededByE280 := e2Bits.ConcatShiftBytesRight(prevE2, 14).
			And(b80Bits.ConcatShiftBytesRight(prev80, 15))
		last := v.Equal(a8).Or(v.Equal(a9)).ToInt8x16().ToBits()
		if maskHasAnyLane(last.And(precededByE280).BitsToInt8().ToMask()) {
			return true
		}
		prevE2 = e2Bits
		prev80 = b80Bits
		i += 16
	}
	tail := i - 2
	if tail < start {
		tail = start
	}
	return hasJSONLineSeparatorScalar(src, tail)
}
