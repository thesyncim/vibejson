package simdjson

import (
	"encoding/binary"
	"math/bits"
	"unsafe"
)

// validFast is the bool-only validation path: a recursive descent machine
// with an inline word-at-a-time fast path for short clean strings. Depth is
// bounded like Validate. Large indentation-heavy documents divert to the
// stage-1 bitmap engine, which skips whitespace and string interiors in
// 64-byte masks.
func validFast(src []byte) bool {
	if len(src) >= validBitmapMinBytes {
		if ok, decided := validBitmap(src); decided {
			return ok
		}
	}
	n := len(src)
	base := sliceBase(src)
	i, c := nextSignificantFast(base, n, 0)
	if i >= n {
		return false
	}
	i, ok := validValueFast(src, base, n, i, c, 0)
	if !ok {
		return false
	}
	return skipSpaceFast(base, n, i) == n
}

// validRootValueFast keeps slice-base derivation inside the validation engine.
// The caller supplies len(src) and the already loaded byte at the in-range value
// offset.
func validRootValueFast(src []byte, n, i int, c byte) (int, bool) {
	return validValueFast(src, sliceBase(src), n, i, c, 0)
}

// validStringFast consumes a string starting at the opening quote, taking one
// SWAR word inline for the short clean case before the general scanner.
func validStringFast(src []byte, base unsafe.Pointer, n, i int) (int, bool) {
	start := i + 1
	if start+8 <= n {
		if m := stringSpecialMask(binary.LittleEndian.Uint64(src[start:])); m != 0 {
			j := start + bits.TrailingZeros64(m)/8
			if fastByteAt(base, j) == '"' {
				return j + 1, true
			}
		} else {
			start += 8
			if start+8 <= n {
				if m := stringSpecialMask(binary.LittleEndian.Uint64(src[start:])); m != 0 {
					j := start + bits.TrailingZeros64(m)/8
					if fastByteAt(base, j) == '"' {
						return j + 1, true
					}
				} else {
					start += 8
				}
			}
			end, _, ok := scanJSONStringFastFrom(src, base, start)
			return end, ok
		}
	}
	end, _, ok := scanJSONStringFastLong(src, base, i)
	return end, ok
}

func validValueFast(src []byte, base unsafe.Pointer, n, i int, c byte, depth int) (int, bool) {
	switch c {
	case '{':
		if depth >= defaultMaxDepth {
			return i, false
		}
		i++
		i, c = nextSignificantFast(base, n, i)
		if i >= n {
			return i, false
		}
		if c == '}' {
			return i + 1, true
		}
		for {
			if c != '"' {
				return i, false
			}
			var ok bool
			i, ok = validStringFast(src, base, n, i)
			if !ok {
				return i, false
			}
			i, c = nextSignificantFast(base, n, i)
			if i >= n || c != ':' {
				return i, false
			}
			i, c = nextSignificantFast(base, n, i+1)
			if i >= n {
				return i, false
			}
			i, ok = validValueFast(src, base, n, i, c, depth+1)
			if !ok {
				return i, false
			}
			i, c = nextSignificantFast(base, n, i)
			if i >= n {
				return i, false
			}
			if c == ',' {
				i, c = nextSignificantFast(base, n, i+1)
				if i >= n {
					return i, false
				}
				continue
			}
			if c == '}' {
				return i + 1, true
			}
			return i, false
		}
	case '[':
		if depth >= defaultMaxDepth {
			return i, false
		}
		i++
		i, c = nextSignificantFast(base, n, i)
		if i >= n {
			return i, false
		}
		if c == ']' {
			return i + 1, true
		}
		for {
			var ok bool
			i, ok = validValueFast(src, base, n, i, c, depth+1)
			if !ok {
				return i, false
			}
			i, c = nextSignificantFast(base, n, i)
			if i >= n {
				return i, false
			}
			if c == ',' {
				i, c = nextSignificantFast(base, n, i+1)
				if i >= n {
					return i, false
				}
				continue
			}
			if c == ']' {
				return i + 1, true
			}
			return i, false
		}
	case '"':
		return validStringFast(src, base, n, i)
	case 't':
		if i+4 > n || loadUint32LE(unsafe.Add(base, i)) != wordTrueLE {
			return i, false
		}
		return i + 4, true
	case 'f':
		if i+5 > n || loadUint32LE(unsafe.Add(base, i+1)) != wordAlseLE {
			return i, false
		}
		return i + 5, true
	case 'n':
		if i+4 > n || loadUint32LE(unsafe.Add(base, i)) != wordNullLE {
			return i, false
		}
		return i + 4, true
	default:
		if c != '-' && !isDigit(c) {
			return i, false
		}
		return scanNumberFast(base, n, i)
	}
}

func scanNumberFast(base unsafe.Pointer, n, i int) (int, bool) {
	end, _, ok := scanNumberFastTagged(base, n, i)
	return end, ok
}

// scanNumberFastTagged is scanNumberFast, additionally reporting whether the
// number is a plain integer (optional minus, then digits only, no fraction or
// exponent). The classification falls out of the branches the scan already
// takes; the tape builders record it so integer reads skip re-inspection.
func scanNumberFastTagged(base unsafe.Pointer, n, i int) (end int, integer, ok bool) {
	if fastByteAt(base, i) == '-' {
		i++
		if i >= n {
			return i, false, false
		}
	}
	if fastByteAt(base, i) == '0' {
		i++
	} else if isOneNine(fastByteAt(base, i)) {
		// Unlike the fraction below, the integer part stays byte-at-a-time;
		// the common short run avoids a speculative fixed-width load.
		for i++; i < n && isDigit(fastByteAt(base, i)); i++ {
		}
	} else {
		return i, false, false
	}
	integer = true
	if i < n && fastByteAt(base, i) == '.' {
		integer = false
		i++
		if i >= n || !isDigit(fastByteAt(base, i)) {
			return i, false, false
		}
		if i+8 <= n && isDigit(fastByteAt(base, i+7)) {
			i = scanDigitsLong(base, n, i)
		} else {
			for i++; i < n && isDigit(fastByteAt(base, i)); i++ {
			}
		}
	}
	if i < n && (fastByteAt(base, i) == 'e' || fastByteAt(base, i) == 'E') {
		integer = false
		i++
		if i < n && (fastByteAt(base, i) == '+' || fastByteAt(base, i) == '-') {
			i++
		}
		if i >= n || !isDigit(fastByteAt(base, i)) {
			return i, false, false
		}
		for i++; i < n && isDigit(fastByteAt(base, i)); i++ {
		}
	}
	return i, integer, true
}

func scanNumberFastTaggedSWAR(base unsafe.Pointer, n, i int) (end int, integer, ok bool) {
	if fastByteAt(base, i) == '-' {
		i++
		if i >= n {
			return i, false, false
		}
	}
	if fastByteAt(base, i) == '0' {
		i++
	} else if isOneNine(fastByteAt(base, i)) {
		i++
		if i+8 <= n && nonDigitMask8(loadUint64LE(unsafe.Add(base, i))) == 0 {
			i += 8
		}
		for ; i < n && isDigit(fastByteAt(base, i)); i++ {
		}
	} else {
		return i, false, false
	}
	integer = true
	if i < n && fastByteAt(base, i) == '.' {
		integer = false
		i++
		if i >= n || !isDigit(fastByteAt(base, i)) {
			return i, false, false
		}
		if i+8 <= n && isDigit(fastByteAt(base, i+7)) {
			i = scanDigitsLong(base, n, i)
		} else {
			for i++; i < n && isDigit(fastByteAt(base, i)); i++ {
			}
		}
	}
	if i < n && (fastByteAt(base, i) == 'e' || fastByteAt(base, i) == 'E') {
		integer = false
		i++
		if i < n && (fastByteAt(base, i) == '+' || fastByteAt(base, i) == '-') {
			i++
		}
		if i >= n || !isDigit(fastByteAt(base, i)) {
			return i, false, false
		}
		for i++; i < n && isDigit(fastByteAt(base, i)); i++ {
		}
	}
	return i, integer, true
}

func scanJSONStringFast(src []byte, base unsafe.Pointer, i int, short bool) (int, bool, bool) {
	if short {
		if end, ok := scanShortJSONString(base, len(src), i); ok {
			return end, false, true
		}
	}
	return scanJSONStringFastLong(src, base, i)
}

func scanJSONStringFastLong(src []byte, base unsafe.Pointer, i int) (int, bool, bool) {
	return scanJSONStringFastFrom(src, base, i+1)
}

func scanJSONStringFastFrom(src []byte, base unsafe.Pointer, i int) (int, bool, bool) {
	escaped := false
	for {
		if i+1 < len(src) && fastByteAt(base, i) == '\\' && fastByteAt(base, i+1) == 'u' {
			if end, ok := scanUnicodeEscapeRun(src, i); !ok {
				return i, escaped, false
			} else if end != i {
				escaped = true
				i = end
			}
		}
		for i+6 <= len(src) && fastByteAt(base, i) == '\\' && fastByteAt(base, i+1) == 'u' {
			escaped = true
			u, ok := hex4(src, i+2)
			if !ok {
				return i, escaped, false
			}
			i += 6
			switch {
			case u >= 0xD800 && u <= 0xDBFF:
				if i+6 > len(src) || fastByteAt(base, i) != '\\' || fastByteAt(base, i+1) != 'u' {
					return i, escaped, false
				}
				lo, ok := hex4(src, i+2)
				if !ok || lo < 0xDC00 || lo > 0xDFFF {
					return i, escaped, false
				}
				i += 6
			case u >= 0xDC00 && u <= 0xDFFF:
				return i, escaped, false
			}
		}
		j := i
		if j >= len(src) || fastByteAt(base, j) != '\\' {
			j = scanStringSpecial(src, j)
		}
		if j >= len(src) {
			return len(src), escaped, false
		}
		switch c := fastByteAt(base, j); {
		case c == '"':
			return j + 1, escaped, true
		case c == '\\':
			escaped = true
			j++
			if j >= len(src) {
				return j, escaped, false
			}
			switch fastByteAt(base, j) {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				i = j + 1
			case 'u':
				u, ok := hex4(src, j+1)
				if !ok {
					return j, escaped, false
				}
				i = j + 5
				switch {
				case 0xD800 <= u && u <= 0xDBFF:
					if i+6 > len(src) || fastByteAt(base, i) != '\\' || fastByteAt(base, i+1) != 'u' {
						return i, escaped, false
					}
					lo, ok := hex4(src, i+2)
					if !ok || lo < 0xDC00 || lo > 0xDFFF {
						return i, escaped, false
					}
					i += 6
				case 0xDC00 <= u && u <= 0xDFFF:
					return i, escaped, false
				}
			default:
				return j, escaped, false
			}
		case c < 0x20:
			return j, escaped, false
		default:
			next, bad := scanStringUnicodeRun(src, j)
			if bad >= 0 {
				return bad, escaped, false
			}
			i = next
		}
	}
}

func scanShortJSONString(base unsafe.Pointer, n, quote int) (int, bool) {
	limit := quote + 9
	if limit > n {
		limit = n
	}
	for i := quote + 1; i < limit; i++ {
		c := fastByteAt(base, i)
		if c == '"' {
			return i + 1, true
		}
		if c == '\\' || c < 0x20 || c >= 0x80 {
			return 0, false
		}
	}
	return 0, false
}

// nextSignificantFast skips insignificant whitespace and returns the first
// significant byte, saving the reload every caller performed afterwards.
// The c > ' ' test resolves nearly every significant byte in one compare.
// It must stay inlineable into the validation loops: the inlining budget
// is 80 and one call to a non-inlineable function costs 57 by itself, so
// almost any addition here de-inlines every call site.
func nextSignificantFast(base unsafe.Pointer, n, i int) (int, byte) {
	for i < n {
		c := fastByteAt(base, i)
		if c > ' ' || (c != ' ' && c != '\n' && c != '\r' && c != '\t') {
			return i, c
		}
		i++
	}
	return i, 0
}

// skipSpaceFast is nextSignificantFast for callers that only need the
// position. The same inlining budget applies: keep the cost under 80,
// where one non-inlined call alone counts 57.
func skipSpaceFast(base unsafe.Pointer, n, i int) int {
	for i < n {
		c := fastByteAt(base, i)
		if c > ' ' || (c != ' ' && c != '\n' && c != '\r' && c != '\t') {
			return i
		}
		i++
	}
	return i
}

func fastByteAt(base unsafe.Pointer, index int) byte {
	return *(*byte)(unsafe.Add(base, uintptr(index)))
}

// sliceBase derives the base pointer of b for bounded pointer arithmetic.
// Every caller proves its own offsets in bounds and must keep b alive and
// immutable for as long as it dereferences derived pointers, per its own
// contract.
func sliceBase(b []byte) unsafe.Pointer {
	return unsafe.Pointer(unsafe.SliceData(b))
}
