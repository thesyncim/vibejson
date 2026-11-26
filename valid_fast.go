package vibejson

import (
	"encoding/binary"
	"math/bits"

	"github.com/thesyncim/vibejson/x/scanner"
)

// validFast is the bool-only validation path: a recursive descent machine
// with an inline word-at-a-time fast path for short clean strings. Depth is
// bounded like Validate. Builds with an accelerated stage-1 classifier divert
// eligible large documents to the bitmap engine, which skips whitespace and
// string interiors in 64-byte masks. Portable builds keep the faster recursive
// path.
func validFast(src []byte) bool {
	if validBitmapAccelerated() && len(src) >= ValidBitmapMinBytes {
		if ok, decided := validBitmap(src); decided {
			return ok
		}
	}
	i, c := nextSignificantFast(src, 0)
	if i >= len(src) {
		return false
	}
	i, ok := validValueFast(src, i, c, 0)
	if !ok {
		return false
	}
	return skipSpaceFast(src, i) == len(src)
}

// validRootValueFast validates one JSON value starting at i with byte c.
func validRootValueFast(src []byte, _ int, i int, c byte) (int, bool) {
	return validValueFast(src, i, c, 0)
}

// validStringFast consumes a string starting at the opening quote, taking two
// SWAR words inline for the short clean case before jumping straight to the
// SIMD scanner when enough bytes remain.
func validStringFast(src []byte, i int) (int, bool) {
	n := uint(len(src))
	start := uint(i) + 1
	if start+8 <= n {
		w := src[start : start+8]
		if m := stringSpecialMask(binary.LittleEndian.Uint64(w)); m != 0 {
			off := uint(bits.TrailingZeros64(m)) >> 3
			if w[off] == '"' {
				return int(start + off + 1), true
			}
		} else {
			start += 8
			if start+8 <= n {
				w2 := src[start : start+8]
				if m := stringSpecialMask(binary.LittleEndian.Uint64(w2)); m != 0 {
					off := uint(bits.TrailingZeros64(m)) >> 3
					if w2[off] == '"' {
						return int(start + off + 1), true
					}
				} else {
					start += 8
				}
			}
			// Finish the ordinary ASCII case after the word probes without
			// entering the escape/Unicode state machine; resume that
			// machine at the first exceptional byte when one is present.
			var end int
			if n-start >= 32 {
				end = scanner.IndexStringSpecialLong(src, int(start))
			} else {
				end = scanStringSpecial(src, int(start))
			}
			if uint(end) < n && src[uint(end)] == '"' {
				return end + 1, true
			}
			end, _, ok := scanJSONStringFastFrom(src, end)
			return end, ok
		}
	}
	end, _, ok := scanJSONStringFastLong(src, i)
	return end, ok
}

func validValueFast(src []byte, idx int, c byte, depth int) (int, bool) {
	n := uint(len(src))
	i := uint(idx)
	switch c {
	case '{':
		if depth >= DefaultMaxDepth {
			return int(i), false
		}
		i++
		if i < n && src[i] > ' ' {
			c = src[i]
		} else {
			var ni int
			ni, c = nextSignificantFast(src, int(i))
			i = uint(ni)
			if i >= n {
				return int(i), false
			}
		}
		if c == '}' {
			return int(i + 1), true
		}
		for {
			if c != '"' {
				return int(i), false
			}
			// Inline 8-byte SWAR probe for short clean object keys.
			if start := i + 1; start+8 <= n {
				w := src[start : start+8]
				if m := stringSpecialMask(binary.LittleEndian.Uint64(w)); m != 0 {
					off := uint(bits.TrailingZeros64(m)) >> 3
					if w[off] == '"' {
						i = start + off + 1
						goto afterKey
					}
				}
			}
			{
				ni, ok := validStringFast(src, int(i))
				if !ok {
					return ni, false
				}
				i = uint(ni)
			}
		afterKey:
			if i < n && src[i] == ':' {
				i++
			} else {
				ni, nc := nextSignificantFast(src, int(i))
				i = uint(ni)
				if i >= n || nc != ':' {
					return int(i), false
				}
				i++
			}
			if i < n && src[i] > ' ' {
				c = src[i]
			} else {
				var ni int
				ni, c = nextSignificantFast(src, int(i))
				i = uint(ni)
				if i >= n {
					return int(i), false
				}
			}
			// Handle scalar values inline to avoid recursive calls per field.
			switch c {
			case '"':
				ni, ok := validStringFast(src, int(i))
				if !ok {
					return ni, false
				}
				i = uint(ni)
			case 't':
				if i+4 > n || binary.LittleEndian.Uint32(src[i:i+4]) != wordTrueLE {
					return int(i), false
				}
				i += 4
			case 'f':
				if i+5 > n || binary.LittleEndian.Uint32(src[i+1:i+5]) != wordAlseLE {
					return int(i), false
				}
				i += 5
			case 'n':
				if i+4 > n || binary.LittleEndian.Uint32(src[i:i+4]) != wordNullLE {
					return int(i), false
				}
				i += 4
			case '{', '[':
				ni, ok := validValueFast(src, int(i), c, depth+1)
				if !ok {
					return ni, false
				}
				i = uint(ni)
			default:
				if c >= '1' && c <= '9' {
					j := i + 1
					for j < n && IsDigit(src[j]) {
						j++
					}
					if j < n {
						next := src[j]
						if next == ',' || next == '}' {
							i = j
							c = next
							goto afterValue
						}
						if next != '.' && next != 'e' && next != 'E' {
							i = j
							break
						}
					} else {
						i = j
						break
					}
				} else if c != '-' && c != '0' {
					return int(i), false
				}
				ni, ok := scanNumberFast(src, int(i))
				if !ok {
					return ni, false
				}
				i = uint(ni)
			}
			if i < n && src[i] > ' ' {
				c = src[i]
			} else {
				var ni int
				ni, c = nextSignificantFast(src, int(i))
				i = uint(ni)
				if i >= n {
					return int(i), false
				}
			}
		afterValue:
			if c == ',' {
				i++
				if i < n && src[i] > ' ' {
					c = src[i]
				} else {
					var ni int
					ni, c = nextSignificantFast(src, int(i))
					i = uint(ni)
					if i >= n {
						return int(i), false
					}
				}
				continue
			}
			if c == '}' {
				return int(i + 1), true
			}
			return int(i), false
		}
	case '[':
		if depth >= DefaultMaxDepth {
			return int(i), false
		}
		i++
		if i < n && src[i] > ' ' {
			c = src[i]
		} else {
			var ni int
			ni, c = nextSignificantFast(src, int(i))
			i = uint(ni)
			if i >= n {
				return int(i), false
			}
		}
		if c == ']' {
			return int(i + 1), true
		}
		for {
			switch c {
			case '"':
				ni, ok := validStringFast(src, int(i))
				if !ok {
					return ni, false
				}
				i = uint(ni)
			case 't':
				if i+4 > n || binary.LittleEndian.Uint32(src[i:i+4]) != wordTrueLE {
					return int(i), false
				}
				i += 4
			case 'f':
				if i+5 > n || binary.LittleEndian.Uint32(src[i+1:i+5]) != wordAlseLE {
					return int(i), false
				}
				i += 5
			case 'n':
				if i+4 > n || binary.LittleEndian.Uint32(src[i:i+4]) != wordNullLE {
					return int(i), false
				}
				i += 4
			case '{', '[':
				ni, ok := validValueFast(src, int(i), c, depth+1)
				if !ok {
					return ni, false
				}
				i = uint(ni)
			default:
				// Inline short numbers inside arrays (integers, 1-decimal, or
				// simple signed exponent) before falling back to scanNumberFast.
				if c >= '1' && c <= '9' {
					j := i + 1
					for j < n && IsDigit(src[j]) {
						j++
					}
					if j < n {
						next := src[j]
						if next == ',' || next == ']' {
							i = j
							c = next
							goto afterArrayValue
						}
						if next == '.' && j+1 < n && IsDigit(src[j+1]) {
							k := j + 2
							for k < n && IsDigit(src[k]) {
								k++
							}
							if k < n {
								after := src[k]
								if after == ',' || after == ']' {
									i = k
									c = after
									goto afterArrayValue
								}
							}
						}
					} else {
						i = j
						break
					}
				} else if c == '-' && i+1 < n && isOneNine(src[i+1]) {
					j := i + 2
					for j < n && IsDigit(src[j]) {
						j++
					}
					if j < n {
						next := src[j]
						if next == ',' || next == ']' {
							i = j
							c = next
							goto afterArrayValue
						}
						if (next == 'e' || next == 'E') && j+1 < n && IsDigit(src[j+1]) {
							k := j + 2
							for k < n && IsDigit(src[k]) {
								k++
							}
							if k < n {
								after := src[k]
								if after == ',' || after == ']' {
									i = k
									c = after
									goto afterArrayValue
								}
							}
						}
					}
				} else if c != '-' && c != '0' {
					return int(i), false
				}
				ni, ok := scanNumberFast(src, int(i))
				if !ok {
					return ni, false
				}
				i = uint(ni)
			}
			if i < n && src[i] > ' ' {
				c = src[i]
			} else {
				var ni int
				ni, c = nextSignificantFast(src, int(i))
				i = uint(ni)
				if i >= n {
					return int(i), false
				}
			}
		afterArrayValue:
			if c == ',' {
				i++
				if i < n && src[i] > ' ' {
					c = src[i]
				} else {
					var ni int
					ni, c = nextSignificantFast(src, int(i))
					i = uint(ni)
					if i >= n {
						return int(i), false
					}
				}
				continue
			}
			if c == ']' {
				return int(i + 1), true
			}
			return int(i), false
		}
	case '"':
		return validStringFast(src, int(i))
	case 't':
		if i+4 > n || binary.LittleEndian.Uint32(src[i:i+4]) != wordTrueLE {
			return int(i), false
		}
		return int(i + 4), true
	case 'f':
		if i+5 > n || binary.LittleEndian.Uint32(src[i+1:i+5]) != wordAlseLE {
			return int(i), false
		}
		return int(i + 5), true
	case 'n':
		if i+4 > n || binary.LittleEndian.Uint32(src[i:i+4]) != wordNullLE {
			return int(i), false
		}
		return int(i + 4), true
	default:
		if c != '-' && !IsDigit(c) {
			return int(i), false
		}
		return scanNumberFast(src, int(i))
	}
}

func scanNumberFast(src []byte, i int) (int, bool) {
	end, _, ok := scanNumberFastTagged(src, i)
	return end, ok
}

// scanNumberFastTagged is scanNumberFast, additionally reporting whether the
// number is a plain integer (optional minus, then digits only, no fraction or
// exponent). The classification falls out of the branches the scan already
// takes; the tape builders record it so integer reads skip re-inspection.
func scanNumberFastTagged(src []byte, i int) (end int, integer, ok bool) {
	n := len(src)
	if src[i] == '-' {
		i++
		if i >= n {
			return i, false, false
		}
	}
	if src[i] == '0' {
		i++
	} else if isOneNine(src[i]) {
		// Unlike the fraction below, the integer part stays byte-at-a-time;
		// the common short run avoids a speculative fixed-width load.
		for i++; i < n && IsDigit(src[i]); i++ {
		}
	} else {
		return i, false, false
	}
	integer = true
	if i < n && src[i] == '.' {
		integer = false
		i++
		if i >= n || !IsDigit(src[i]) {
			return i, false, false
		}
		if n-i >= 8 && IsDigit(src[i+7]) {
			i = scanDigitsLong(src, i)
		} else {
			for i++; i < n && IsDigit(src[i]); i++ {
			}
		}
	}
	if i < n && (src[i] == 'e' || src[i] == 'E') {
		integer = false
		i++
		if i < n && (src[i] == '+' || src[i] == '-') {
			i++
		}
		if i >= n || !IsDigit(src[i]) {
			return i, false, false
		}
		for i++; i < n && IsDigit(src[i]); i++ {
		}
	}
	return i, integer, true
}

func scanNumberFastTaggedSWAR(src []byte, i int) (end int, integer, ok bool) {
	n := len(src)
	if src[i] == '-' {
		i++
		if i >= n {
			return i, false, false
		}
	}
	if src[i] == '0' {
		i++
	} else if isOneNine(src[i]) {
		i++
		if n-i >= 8 && nonDigitMask8(binary.LittleEndian.Uint64(src[i:])) == 0 {
			i += 8
		}
		for ; i < n && IsDigit(src[i]); i++ {
		}
	} else {
		return i, false, false
	}
	integer = true
	if i < n && src[i] == '.' {
		integer = false
		i++
		if i >= n || !IsDigit(src[i]) {
			return i, false, false
		}
		if n-i >= 8 && IsDigit(src[i+7]) {
			i = scanDigitsLong(src, i)
		} else {
			for i++; i < n && IsDigit(src[i]); i++ {
			}
		}
	}
	if i < n && (src[i] == 'e' || src[i] == 'E') {
		integer = false
		i++
		if i < n && (src[i] == '+' || src[i] == '-') {
			i++
		}
		if i >= n || !IsDigit(src[i]) {
			return i, false, false
		}
		for i++; i < n && IsDigit(src[i]); i++ {
		}
	}
	return i, integer, true
}

func scanJSONStringFast(src []byte, i int, short bool) (int, bool, bool) {
	if short {
		if end, ok := scanShortJSONString(src, i); ok {
			return end, false, true
		}
	}
	return scanJSONStringFastLong(src, i)
}

func scanJSONStringFastLong(src []byte, i int) (int, bool, bool) {
	return scanJSONStringFastFrom(src, i+1)
}

func scanJSONStringFastFrom(src []byte, i int) (int, bool, bool) {
	escaped := false
	for {
		if i+1 < len(src) && src[i] == '\\' && src[i+1] == 'u' {
			if end, ok := scanUnicodeEscapeRun(src, i); !ok {
				return i, escaped, false
			} else if end != i {
				escaped = true
				i = end
			}
		}
		for len(src)-i >= 6 && src[i] == '\\' && src[i+1] == 'u' {
			escaped = true
			u, ok := hex4(src, i+2)
			if !ok {
				return i, escaped, false
			}
			i += 6
			switch {
			case u >= 0xD800 && u <= 0xDBFF:
				if len(src)-i < 6 || src[i] != '\\' || src[i+1] != 'u' {
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
		if j >= len(src) || src[j] != '\\' {
			j = scanStringSpecial(src, j)
		}
		if j >= len(src) {
			return len(src), escaped, false
		}
		switch c := src[j]; {
		case c == '"':
			return j + 1, escaped, true
		case c == '\\':
			escaped = true
			j++
			if j >= len(src) {
				return j, escaped, false
			}
			switch src[j] {
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
					if len(src)-i < 6 || src[i] != '\\' || src[i+1] != 'u' {
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

func scanShortJSONString(src []byte, quote int) (int, bool) {
	limit := quote + 9
	if limit > len(src) {
		limit = len(src)
	}
	for i := quote + 1; i < limit; i++ {
		c := src[i]
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
// It must stay inlineable into the validation loops.
func nextSignificantFast(src []byte, i int) (int, byte) {
	for i < len(src) {
		c := src[i]
		if c > ' ' || (c != ' ' && c != '\n' && c != '\r' && c != '\t') {
			return i, c
		}
		i++
	}
	return i, 0
}

// skipSpaceFast is nextSignificantFast for callers that only need the
// position.
func skipSpaceFast(src []byte, i int) int {
	for i < len(src) {
		c := src[i]
		if c > ' ' || (c != ' ' && c != '\n' && c != '\r' && c != '\t') {
			return i
		}
		i++
	}
	return i
}
