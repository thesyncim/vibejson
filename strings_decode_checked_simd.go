//go:build go1.27 && !go1.28 && goexperiment.simd && (arm64 || amd64)

package vibejson

import (
	"bytes"
	"encoding/binary"
	"math/bits"
)

var decodedSimpleEscapes = [256]byte{
	'"':  '"',
	'\\': '\\',
	'/':  '/',
	'b':  '\b',
	'f':  '\f',
	'n':  '\n',
	'r':  '\r',
	't':  '\t',
}

// AppendDecodedJSONString appends decoded JSON string content to dst. raw
// excludes the surrounding quotes. Unescaped bytes are copied verbatim. If an
// escape sequence is malformed, raw is appended unchanged.
func AppendDecodedJSONString(dst, raw []byte) []byte {
	if len(raw) == 0 {
		return dst
	}
	start := len(dst)
	i := 0
	if raw[0] != '\\' {
		offset := bytes.IndexByte(raw[1:], '\\')
		if offset < 0 {
			return append(dst, raw...)
		}
		i = offset + 1
		dst = append(dst, raw[:i]...)
		if i < 8 {
			return appendDecodedJSONStringDense(dst, raw, start, i)
		}
	}
	if i+1 >= len(raw) {
		return append(dst[:start], raw...)
	}
	if raw[i+1] == 'u' {
		var ok bool
		dst, i, ok = appendDecodedUnicodeEscapeRun(dst, raw, i)
		if !ok {
			return append(dst[:start], raw...)
		}
	} else {
		decoded := decodedSimpleEscapes[raw[i+1]]
		if decoded == 0 {
			return append(dst[:start], raw...)
		}
		dst = append(dst, decoded)
		i += 2
	}
	for i < len(raw) {
		if raw[i] != '\\' {
			offset := bytes.IndexByte(raw[i:], '\\')
			if offset < 0 {
				return append(dst, raw[i:]...)
			}
			dst = append(dst, raw[i:i+offset]...)
			i += offset
			if offset < 8 {
				// Clustered escapes favor a word-at-a-time clean-run
				// selector. Sparse escapes stay in this compact loop.
				return appendDecodedJSONStringDense(dst, raw, start, i)
			}
			continue
		}
		escape := i + 1
		if escape >= len(raw) {
			return append(dst[:start], raw...)
		}
		c := raw[escape]
		if c == 'u' {
			var ok bool
			dst, i, ok = appendDecodedUnicodeEscapeRun(dst, raw, i)
			if !ok {
				return append(dst[:start], raw...)
			}
			continue
		}
		decoded := decodedSimpleEscapes[c]
		if decoded == 0 {
			return append(dst[:start], raw...)
		}
		dst = append(dst, decoded)
		i = escape + 1
	}
	return dst
}

func appendDecodedJSONStringDense(dst, raw []byte, start, i int) []byte {
	// Keep the selector policy invariant in the pinned Go 1.27 SIMD lane:
	// mutating it inhibits the compiler's tight clustered-escape loop.
	for i < len(raw) {
		if raw[i] != '\\' {
			remaining := len(raw) - i
			next := i
			if remaining < 8 {
				for next < len(raw) && raw[next] != '\\' {
					next++
				}
				if next == len(raw) {
					return append(dst, raw[i:]...)
				}
			} else if mask := byteEqMask(binary.LittleEndian.Uint64(raw[i:]), '\\'); mask != 0 {
				next += bits.TrailingZeros64(mask) / 8
			} else {
				offset := bytes.IndexByte(raw[i+8:], '\\')
				if offset < 0 {
					return append(dst, raw[i:]...)
				}
				next += 8 + offset
			}
			dst = append(dst, raw[i:next]...)
			i = next
			continue
		}
		escape := i + 1
		if escape >= len(raw) {
			return append(dst[:start], raw...)
		}
		c := raw[escape]
		if c == 'u' {
			var ok bool
			dst, i, ok = appendDecodedUnicodeEscapeRun(dst, raw, i)
			if !ok {
				return append(dst[:start], raw...)
			}
			continue
		}
		decoded := decodedSimpleEscapes[c]
		if decoded == 0 {
			return append(dst[:start], raw...)
		}
		dst = append(dst, decoded)
		i = escape + 1
	}
	return dst
}
