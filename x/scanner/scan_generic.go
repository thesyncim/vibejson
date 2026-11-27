package scanner

import (
	"bytes"
	"encoding/binary"
	"math/bits"
	"unicode/utf8"
)

const (
	htmlQuoteAmpFold     = 0x0404040404040404
	htmlAngleBracketFold = 0x0202020202020202
)

func scanStringSpecialScalar(src []byte, i int) int {
	return scanStringSpecialScalarUntil(src, i, len(src))
}

func scanStringSpecialScalarUntil(src []byte, i, limit int) int {
	for i+16 <= limit {
		w := src[i : i+16]
		if m := stringSpecialMask(binary.LittleEndian.Uint64(w[:8])); m != 0 {
			return i + bits.TrailingZeros64(m)>>3
		}
		if m := stringSpecialMask(binary.LittleEndian.Uint64(w[8:16])); m != 0 {
			return i + 8 + bits.TrailingZeros64(m)>>3
		}
		i += 16
	}
	if i+8 <= limit {
		if m := stringSpecialMask(binary.LittleEndian.Uint64(src[i : i+8])); m != 0 {
			return i + bits.TrailingZeros64(m)>>3
		}
		i += 8
	}
	for i < limit {
		c := src[i]
		if c == '"' || c == '\\' || c < 0x20 || c >= 0x80 {
			return i
		}
		i++
	}
	return limit
}

func scanStringSyntaxScalar(src []byte, i int) int {
	limit := len(src)
	for i+16 <= limit {
		x := binary.LittleEndian.Uint64(src[i:])
		if m := stringSyntaxMask(x); m != 0 {
			return i + bits.TrailingZeros64(m)/8
		}
		x = binary.LittleEndian.Uint64(src[i+8:])
		if m := stringSyntaxMask(x); m != 0 {
			return i + 8 + bits.TrailingZeros64(m)/8
		}
		i += 16
	}
	if i+8 <= limit {
		x := binary.LittleEndian.Uint64(src[i:])
		if m := stringSyntaxMask(x); m != 0 {
			return i + bits.TrailingZeros64(m)/8
		}
		i += 8
	}
	for i < limit {
		c := src[i]
		if c == '"' || c == '\\' || c < 0x20 {
			return i
		}
		i++
	}
	return limit
}

func scanEncodedHTMLSyntaxScalar(src []byte, i int) int {
	const highBits = 0x8080808080808080
	for i+8 <= len(src) {
		x := binary.LittleEndian.Uint64(src[i:])
		m := byteEqMask(x|htmlQuoteAmpFold, '&') |
			byteEqMask(x, '\\') |
			((x - 0x2020202020202020) & ^x & highBits) |
			byteEqMask(x|htmlAngleBracketFold, '>')
		if m != 0 {
			return i + bits.TrailingZeros64(m)/8
		}
		i += 8
	}
	for i < len(src) {
		c := src[i]
		if c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' || c < 0x20 {
			return i
		}
		i++
	}
	return len(src)
}

func scanEncodedHTMLSpecialScalar(src []byte, i int) int {
	limit := len(src)
	for i+16 <= limit {
		x := binary.LittleEndian.Uint64(src[i:])
		if m := htmlStringSpecialMask(x); m != 0 {
			return i + bits.TrailingZeros64(m)/8
		}
		x = binary.LittleEndian.Uint64(src[i+8:])
		if m := htmlStringSpecialMask(x); m != 0 {
			return i + 8 + bits.TrailingZeros64(m)/8
		}
		i += 16
	}
	if i+8 <= limit {
		x := binary.LittleEndian.Uint64(src[i:])
		if m := htmlStringSpecialMask(x); m != 0 {
			return i + bits.TrailingZeros64(m)/8
		}
		i += 8
	}
	for i < limit {
		c := src[i]
		if c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' || c < 0x20 || c >= 0x80 {
			return i
		}
		i++
	}
	return limit
}

// copyHTMLStringPrefixSWAR scans and copies an HTML-safe ASCII prefix in a
// single pass using 64-bit SWAR loads and stores. dst and src must have equal
// length and not overlap.
func copyHTMLStringPrefixSWAR(dst, src []byte) int {
	n := len(src)
	if n >= 8 && n <= 16 {
		w0 := binary.LittleEndian.Uint64(src[:8])
		w1 := binary.LittleEndian.Uint64(src[n-8 : n])
		if htmlStringSpecialMask(w0)|htmlStringSpecialMask(w1) == 0 {
			binary.LittleEndian.PutUint64(dst[:8], w0)
			binary.LittleEndian.PutUint64(dst[n-8:n], w1)
			return n
		}
	}
	i := 0
	for i+16 <= n {
		w0 := binary.LittleEndian.Uint64(src[i:])
		if m := htmlStringSpecialMask(w0); m != 0 {
			k := bits.TrailingZeros64(m) >> 3
			for j := 0; j < k; j++ {
				dst[i+j] = src[i+j]
			}
			return i + k
		}
		binary.LittleEndian.PutUint64(dst[i:], w0)
		w1 := binary.LittleEndian.Uint64(src[i+8:])
		if m := htmlStringSpecialMask(w1); m != 0 {
			k := bits.TrailingZeros64(m) >> 3
			for j := 0; j < k; j++ {
				dst[i+8+j] = src[i+8+j]
			}
			return i + 8 + k
		}
		binary.LittleEndian.PutUint64(dst[i+8:], w1)
		i += 16
	}
	if i+8 <= n {
		w := binary.LittleEndian.Uint64(src[i:])
		if m := htmlStringSpecialMask(w); m != 0 {
			k := bits.TrailingZeros64(m) >> 3
			for j := 0; j < k; j++ {
				dst[i+j] = src[i+j]
			}
			return i + k
		}
		binary.LittleEndian.PutUint64(dst[i:], w)
		i += 8
	}
	if i < n && i >= 8 {
		tail := n - 8
		w := binary.LittleEndian.Uint64(src[tail:n])
		if m := htmlStringSpecialMask(w); m == 0 {
			binary.LittleEndian.PutUint64(dst[tail:n], w)
			return n
		}
	}
	for i < n {
		c := src[i]
		if c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' || c < 0x20 || c >= 0x80 {
			return i
		}
		dst[i] = c
		i++
	}
	return n
}

// copyStringPrefixSWAR scans and copies a JSON-safe ASCII prefix in a single
// pass using 64-bit SWAR loads and stores. dst and src must have equal length
// and not overlap.
func copyStringPrefixSWAR(dst, src []byte) int {
	n := len(src)
	if n >= 8 && n <= 16 {
		w0 := binary.LittleEndian.Uint64(src[:8])
		w1 := binary.LittleEndian.Uint64(src[n-8 : n])
		if stringSpecialMask(w0)|stringSpecialMask(w1) == 0 {
			binary.LittleEndian.PutUint64(dst[:8], w0)
			binary.LittleEndian.PutUint64(dst[n-8:n], w1)
			return n
		}
	}
	i := 0
	for i+16 <= n {
		w0 := binary.LittleEndian.Uint64(src[i:])
		if m := stringSpecialMask(w0); m != 0 {
			k := bits.TrailingZeros64(m) >> 3
			for j := 0; j < k; j++ {
				dst[i+j] = src[i+j]
			}
			return i + k
		}
		binary.LittleEndian.PutUint64(dst[i:], w0)
		w1 := binary.LittleEndian.Uint64(src[i+8:])
		if m := stringSpecialMask(w1); m != 0 {
			k := bits.TrailingZeros64(m) >> 3
			for j := 0; j < k; j++ {
				dst[i+8+j] = src[i+8+j]
			}
			return i + 8 + k
		}
		binary.LittleEndian.PutUint64(dst[i+8:], w1)
		i += 16
	}
	if i+8 <= n {
		w := binary.LittleEndian.Uint64(src[i:])
		if m := stringSpecialMask(w); m != 0 {
			k := bits.TrailingZeros64(m) >> 3
			for j := 0; j < k; j++ {
				dst[i+j] = src[i+j]
			}
			return i + k
		}
		binary.LittleEndian.PutUint64(dst[i:], w)
		i += 8
	}
	if i < n && i >= 8 {
		tail := n - 8
		w := binary.LittleEndian.Uint64(src[tail:n])
		if m := stringSpecialMask(w); m == 0 {
			binary.LittleEndian.PutUint64(dst[tail:n], w)
			return n
		}
	}
	for i < n {
		c := src[i]
		if c == '"' || c == '\\' || c < 0x20 || c >= 0x80 {
			return i
		}
		dst[i] = c
		i++
	}
	return n
}

func hasJSONLineSeparatorScalar(src []byte, start int) bool {
	src = src[start:]
	offset := bytes.IndexByte(src, 0xe2)
	if offset < 0 {
		return false
	}
	i := offset
	if i+2 < len(src) && src[i+1] == 0x80 && (src[i+2] == 0xa8 || src[i+2] == 0xa9) {
		return true
	}
	i++

	for i+10 <= len(src) {
		window := src[i : i+10]
		candidates := byteEqMask(binary.LittleEndian.Uint64(window), 0xe2)
		for candidates != 0 {
			offset := bits.TrailingZeros64(candidates) / 8
			if offset >= 8 {
				break
			}
			if window[offset+1] == 0x80 && (window[offset+2] == 0xa8 || window[offset+2] == 0xa9) {
				return true
			}
			candidates &= candidates - 1
		}
		i += 8
	}
	for i+2 < len(src) {
		if src[i] == 0xe2 && src[i+1] == 0x80 && (src[i+2] == 0xa8 || src[i+2] == 0xa9) {
			return true
		}
		i++
	}
	return false
}

func invalidUTF8Index(src []byte, start, end int) int {
	if validUTF8Fast(src[start:end]) {
		return -1
	}
	for start < end {
		r, n := utf8.DecodeRune(src[start:end])
		if r == utf8.RuneError && n == 1 {
			return start
		}
		start += n
	}
	return end
}

func scanStringUnicodeRun(src []byte, i int) (next, bad int) {
	if len(src)-i < 32 {
		r, n := utf8.DecodeRune(src[i:])
		if r == utf8.RuneError && n == 1 {
			return i, i
		}
		return i + n, -1
	}
	next = scanStringSyntax(src, i)
	return next, invalidUTF8Index(src, i, next)
}

func stringSpecialMask(x uint64) uint64 {
	const (
		ones     = 0x0101010101010101
		highBits = 0x8080808080808080
	)
	q := x ^ 0x2222222222222222
	s := x ^ 0x5c5c5c5c5c5c5c5c
	return (((q - ones) & ^q) | ((s - ones) & ^s) | (x - 0x2020202020202020) | x) & highBits
}

func stringSyntaxMask(x uint64) uint64 {
	const (
		ones     = 0x0101010101010101
		highBits = 0x8080808080808080
	)
	q := x ^ 0x2222222222222222
	s := x ^ 0x5c5c5c5c5c5c5c5c
	return (((q - ones) & ^q) | ((s - ones) & ^s) | ((x - 0x2020202020202020) & ^x)) & highBits
}

func htmlStringSpecialMask(x uint64) uint64 {
	const (
		ones     = 0x0101010101010101
		highBits = 0x8080808080808080
	)
	a := (x | htmlQuoteAmpFold) ^ 0x2626262626262626
	b := x ^ 0x5c5c5c5c5c5c5c5c
	c := (x | htmlAngleBracketFold) ^ 0x3e3e3e3e3e3e3e3e
	return (((a - ones) & ^a) | ((b - ones) & ^b) | ((c - ones) & ^c) | (x - 0x2020202020202020) | x) & highBits
}

func byteEqMask(x uint64, b byte) uint64 {
	const highBits = 0x8080808080808080
	y := x ^ (uint64(b) * 0x0101010101010101)
	return (y - 0x0101010101010101) & ^y & highBits
}
