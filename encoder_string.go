package simdjson

import (
	"slices"
	"unicode/utf8"
	"unsafe"

	"github.com/thesyncim/simdjson/internal/byteview"
	"github.com/thesyncim/simdjson/internal/scanner"
)

const encodeHexDigits = "0123456789abcdef"

// appendShortCleanJSONString quotes strings shorter than one vector when no
// byte needs escaping, testing and copying word-at-a-time instead of
// byte-at-a-time. The caller must guarantee 1 <= len(s) <= 16: an empty
// string has no data pointer to load and a longer one would skip its middle
// bytes. ok reports whether it emitted; any flagged byte, or a dst too full
// for its unconditional word stores, defers to the general path.
func appendShortCleanJSONString(dst []byte, s string, escapeHTML bool) ([]byte, bool) {
	n := uint(len(s))
	if cap(dst)-len(dst) < int(n)+10 {
		return dst, false
	}
	p := unsafe.Pointer(unsafe.StringData(s))
	if n >= 8 {
		w0 := loadUint64LE(p)
		w1 := loadUint64LE(unsafe.Add(p, n-8))
		var mask uint64
		if escapeHTML {
			mask = scanner.HTMLStringSpecialMask64(w0) | scanner.HTMLStringSpecialMask64(w1)
		} else {
			mask = scanner.StringSpecialMask64(w0) | scanner.StringSpecialMask64(w1)
		}
		if mask != 0 {
			return dst, false
		}
		start := len(dst)
		dst = dst[:start+int(n)+2]
		dst[start] = '"'
		base := unsafe.Pointer(unsafe.SliceData(dst))
		storeUint64LE(unsafe.Add(base, start+1), w0)
		storeUint64LE(unsafe.Add(base, start+1+int(n)-8), w1)
		dst[start+1+int(n)] = '"'
		return dst, true
	}
	// Overlapped halves build the exact zero-padded word image of s, so one
	// mask probe and one store cover any length; the padding lanes are
	// discarded from the mask and overwritten by the closing quote.
	var w uint64
	switch {
	case n >= 4:
		w = uint64(loadUint32LE(p)) | uint64(loadUint32LE(unsafe.Add(p, n-4)))<<((n-4)*8)
	case n >= 2:
		w = uint64(loadUint16LE(p)) | uint64(loadUint16LE(unsafe.Add(p, n-2)))<<((n-2)*8)
	default:
		w = uint64(*(*byte)(p))
	}
	var mask uint64
	if escapeHTML {
		mask = scanner.HTMLStringSpecialMask64(w)
	} else {
		mask = scanner.StringSpecialMask64(w)
	}
	if mask&(uint64(1)<<(8*n)-1) != 0 {
		return dst, false
	}
	start := len(dst)
	dst = dst[:start+int(n)+2]
	dst[start] = '"'
	storeUint64LE(unsafe.Add(unsafe.Pointer(unsafe.SliceData(dst)), start+1), w)
	dst[start+1+int(n)] = '"'
	return dst, true
}

// Provenance: GO-STRING-001.
// The scalar escape behavior and control-flow shape are conservatively treated
// as adapted from Go commit d468ad3648be469ffc4090e4586c29709182d6b6,
// src/encoding/json/encode.go appendString; BSD-3-Clause, see LICENSE-GO.
// SIMD prefix scanning, fused copies, and buffer integration are local changes.
//
// appendEncodedJSONString appends s as a quoted JSON string with
// encoding/json's spelling: control bytes, quotes, and backslashes are
// escaped, invalid UTF-8 uses the active encoding/json release's spelling,
// U+2028/U+2029 are escaped, and
// escapeHTML additionally escapes '<', '>', and '&'.
func appendEncodedJSONString(dst []byte, s string, escapeHTML bool) []byte {
	const fusedCopyMinBytes = 16

	if len(s) == 0 {
		return append(dst, '"', '"')
	}
	if len(s) < fusedCopyMinBytes {
		if out, ok := appendShortCleanJSONString(dst, s, escapeHTML); ok {
			return out
		}
	}
	if len(s) >= fusedCopyMinBytes {
		dst = slices.Grow(dst, len(s)+2)
	}
	dst = append(dst, '"')
	src := byteview.Bytes(s)
	first := 0
	copiedPrefix := false
	if len(s) >= fusedCopyMinBytes {
		start := len(dst)
		dst = dst[:start+len(s)]
		if escapeHTML {
			first = scanner.CopyHTMLStringPrefix(dst[start:], src)
		} else {
			first = scanner.CopyStringPrefix(dst[start:], src)
		}
		if first >= 0 {
			if first == len(src) {
				return append(dst, '"')
			}
			dst = dst[:start+first]
			copiedPrefix = true
		} else {
			dst = dst[:start]
		}
	}
	if !copiedPrefix {
		if escapeHTML {
			first = scanEncodedHTMLSpecialFast(src, 0)
		} else {
			first = scanStringSpecial(src, 0)
		}
	}
	if first == len(src) {
		dst = append(dst, s...)
		return append(dst, '"')
	}
	unicodeClean := false
	if src[first] >= 0x80 {
		unicodeClean = validUTF8NoLineSeparatorFast(src)
	}
	start := 0
	if copiedPrefix {
		start = first
	}
	for i := first; i < len(s); {
		// The scanners stop at exactly the escape-relevant set: quotes,
		// backslashes, control bytes, non-ASCII, and in HTML mode the
		// angle brackets and ampersand encoding/json escapes by default.
		if unicodeClean && escapeHTML {
			i = scanEncodedHTMLSyntaxFast(src, i)
		} else if unicodeClean {
			i = scanStringSyntax(src, i)
		} else if escapeHTML {
			i = scanEncodedHTMLSpecialFast(src, i)
		} else {
			i = scanStringSpecial(src, i)
		}
		if i >= len(s) {
			break
		}
		c := s[i]
		if c < 0x80 {
			dst = append(dst, s[start:i]...)
			switch c {
			case '"':
				dst = append(dst, '\\', '"')
			case '\\':
				dst = append(dst, '\\', '\\')
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', encodeHexDigits[c>>4], encodeHexDigits[c&0xF])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			dst = append(dst, s[start:i]...)
			if escapeInvalidUTF8 {
				dst = append(dst, '\\', 'u', 'f', 'f', 'f', 'd')
			} else {
				dst = utf8.AppendRune(dst, utf8.RuneError)
			}
			i++
			start = i
			continue
		}
		if r == '\u2028' || r == '\u2029' {
			dst = append(dst, s[start:i]...)
			dst = append(dst, '\\', 'u', '2', '0', '2', encodeHexDigits[r&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}
