package vibejson

import (
	"encoding/binary"
	"slices"
	"unicode/utf8"

	"github.com/thesyncim/vibejson/x/byteview"
	"github.com/thesyncim/vibejson/x/scanner"
)

const encodeHexDigits = "0123456789abcdef"

// appendShortCleanJSONBytes quotes short byte spans when no byte needs
// escaping, testing and copying word-at-a-time instead of byte-at-a-time or
// paying SIMD vector setup. The caller must guarantee 1 <= len(src) <= 32.
// ok reports whether it emitted; any flagged byte, or a dst without room for
// the quoted string, defers to the general path.
func appendShortCleanJSONBytes(dst, src []byte, escapeHTML bool) ([]byte, bool) {
	n := uint(len(src))
	if cap(dst)-len(dst) < int(n)+2 {
		return dst, false
	}
	if n > 16 {
		w0 := binary.LittleEndian.Uint64(src[0:8])
		w1 := binary.LittleEndian.Uint64(src[8:16])
		w2 := binary.LittleEndian.Uint64(src[n-16 : n-8])
		w3 := binary.LittleEndian.Uint64(src[n-8 : n])
		var mask uint64
		if escapeHTML {
			mask = scanner.HTMLStringSpecialMask64(w0) |
				scanner.HTMLStringSpecialMask64(w1) |
				scanner.HTMLStringSpecialMask64(w2) |
				scanner.HTMLStringSpecialMask64(w3)
		} else {
			mask = scanner.StringSpecialMask64(w0) |
				scanner.StringSpecialMask64(w1) |
				scanner.StringSpecialMask64(w2) |
				scanner.StringSpecialMask64(w3)
		}
		if mask != 0 {
			return dst, false
		}
		start := len(dst)
		dst = dst[:start+int(n)+2]
		out := dst[start : start+int(n)+2]
		out[0] = '"'
		binary.LittleEndian.PutUint64(out[1:9], w0)
		binary.LittleEndian.PutUint64(out[9:17], w1)
		binary.LittleEndian.PutUint64(out[int(n)-15:int(n)-7], w2)
		binary.LittleEndian.PutUint64(out[int(n)-7:int(n)+1], w3)
		out[int(n)+1] = '"'
		return dst, true
	}
	if n >= 8 {
		w0 := binary.LittleEndian.Uint64(src[:8])
		w1 := binary.LittleEndian.Uint64(src[n-8 : n])
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
		out := dst[start : start+int(n)+2]
		out[0] = '"'
		binary.LittleEndian.PutUint64(out[1:9], w0)
		binary.LittleEndian.PutUint64(out[int(n)-7:int(n)+1], w1)
		out[int(n)+1] = '"'
		return dst, true
	}
	// Overlapped halves build the exact zero-padded word image of src for one
	// mask probe; padding lanes are discarded from the mask.
	var w uint64
	switch {
	case n >= 4:
		w = uint64(binary.LittleEndian.Uint32(src[:4])) | uint64(binary.LittleEndian.Uint32(src[n-4:n]))<<((n-4)*8)
	case n >= 2:
		w = uint64(binary.LittleEndian.Uint16(src[:2])) | uint64(binary.LittleEndian.Uint16(src[n-2:n]))<<((n-2)*8)
	default:
		w = uint64(src[0])
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
	out := dst[start : start+int(n)+2]
	// Include the opening quote in the word so every store stays within the
	// result, even when the caller exposes more unused destination capacity.
	w = w<<8 | '"'
	switch {
	case n >= 6:
		binary.LittleEndian.PutUint64(out[:8], w)
	case n >= 2:
		binary.LittleEndian.PutUint32(out[:4], uint32(w))
		binary.LittleEndian.PutUint32(out[n-2:n+2], uint32(w>>((n-2)*8)))
	default:
		binary.LittleEndian.PutUint16(out[:2], uint16(w))
	}
	out[int(n)+1] = '"'
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
	return appendEncodedJSONBytes(dst, byteview.Bytes(s), escapeHTML)
}

// appendEncodedJSONBytes appends src as a quoted JSON string without
// converting src to a Go string.
func appendEncodedJSONBytes(dst, src []byte, escapeHTML bool) []byte {
	const shortCleanMaxBytes = 32
	const fusedCopyMinBytes = 16

	if len(src) == 0 {
		return append(dst, '"', '"')
	}
	if len(src) <= shortCleanMaxBytes {
		if out, ok := appendShortCleanJSONBytes(dst, src, escapeHTML); ok {
			return out
		}
	}
	if len(src) >= fusedCopyMinBytes {
		if cap(dst)-len(dst) < len(src)+2 {
			dst = slices.Grow(dst, len(src)+2)
		}
		start := len(dst)
		dst = dst[:start+len(src)+2]
		dst[start] = '"'
		var first int
		if escapeHTML {
			first = scanner.CopyHTMLStringPrefix(dst[start+1:start+1+len(src)], src)
		} else {
			first = scanner.CopyStringPrefix(dst[start+1:start+1+len(src)], src)
		}
		if first == len(src) {
			dst[start+1+len(src)] = '"'
			return dst
		}
		if first >= 0 {
			dst = dst[:start+1+first]
			return appendEncodedJSONBytesTail(dst, src, first, true, escapeHTML)
		}
		dst = dst[:start+1]
		return appendEncodedJSONBytesTail(dst, src, 0, false, escapeHTML)
	}
	dst = append(dst, '"')
	return appendEncodedJSONBytesTail(dst, src, 0, false, escapeHTML)
}

//go:noinline
func appendEncodedJSONBytesTail(dst, src []byte, first int, copiedPrefix, escapeHTML bool) []byte {
	if !copiedPrefix {
		if escapeHTML {
			first = scanEncodedHTMLSpecialFast(src, 0)
		} else {
			first = scanStringSpecial(src, 0)
		}
	}
	if first == len(src) {
		dst = append(dst, src...)
		return append(dst, '"')
	}
	unicodeClean := false
	if src[first] >= 0x80 {
		// The prefix scanner proved every earlier byte ASCII, so validating
		// only the non-ASCII tail preserves the complete UTF-8 certificate.
		unicodeClean = validUTF8NoLineSeparatorFast(src[first:])
	}
	start := 0
	if copiedPrefix {
		start = first
	}
	for i := first; i < len(src); {
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
		if i >= len(src) {
			break
		}
		c := src[i]
		if c < 0x80 {
			dst = append(dst, src[start:i]...)
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
		r, size := utf8.DecodeRune(src[i:])
		if r == utf8.RuneError && size == 1 {
			dst = append(dst, src[start:i]...)
			// Go 1.27 writes the replacement rune literally.
			dst = utf8.AppendRune(dst, utf8.RuneError)
			i++
			start = i
			continue
		}
		if r == '\u2028' || r == '\u2029' {
			dst = append(dst, src[start:i]...)
			dst = append(dst, '\\', 'u', '2', '0', '2', encodeHexDigits[r&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	dst = append(dst, src[start:]...)
	return append(dst, '"')
}
