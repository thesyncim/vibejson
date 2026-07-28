package vibejson

import (
	"bytes"
	"unicode/utf16"
	"unicode/utf8"
)

// AppendDecodedJSONString appends the decoded contents of a JSON string to
// dst. raw excludes the surrounding quotes. Invalid string contents are
// appended unchanged, making the public helper lossless and panic-free even
// when called without prior validation.
func AppendDecodedJSONString(dst, raw []byte) []byte {
	// Keep the overwhelmingly common unescaped case identical to the trusted
	// decoder: one vectorized search followed by one append.
	if bytes.IndexByte(raw, '\\') < 0 {
		return append(dst, raw...)
	}
	if !validJSONStringContent(raw) {
		return append(dst, raw...)
	}
	return appendDecodedJSONStringTrusted(dst, raw)
}

// appendDecodedJSONStringTrusted is used by parser-backed accessors whose
// source has already passed the full JSON string validator.
func appendDecodedJSONStringTrusted(dst, raw []byte) []byte {
	for i := 0; i < len(raw); {
		if raw[i] != '\\' {
			// The next backslash bounds a clean run; IndexByte finds it a
			// vector at a time. raw was validated, so nothing else needs
			// inspection.
			j := bytes.IndexByte(raw[i:], '\\')
			if j < 0 {
				return append(dst, raw[i:]...)
			}
			dst = append(dst, raw[i:i+j]...)
			i += j
			continue
		}
		i++
		if raw[i] != 'u' {
			dst = append(dst, decodedSimpleEscape(raw[i]))
			i++
			continue
		}
		u, _ := hex4(raw, i+1)
		i += 5
		r := rune(u)
		if 0xD800 <= r && r <= 0xDBFF {
			lo, _ := hex4(raw, i+2)
			r = utf16.DecodeRune(r, rune(lo))
			i += 6
		}
		dst = utf8.AppendRune(dst, r)
	}
	return dst
}

func validJSONStringContent(raw []byte) bool {
	for i := 0; i < len(raw); {
		j := scanStringSpecial(raw, i)
		if j >= len(raw) {
			return true
		}
		switch c := raw[j]; {
		case c == '"', c < 0x20:
			return false
		case c == '\\':
			j++
			if j >= len(raw) {
				return false
			}
			v := validator{src: raw, i: j, maxDepth: DefaultMaxDepth}
			if v.validateEscape() != nil {
				return false
			}
			i = v.i
		default:
			next, bad := scanStringUnicodeRun(raw, j)
			if bad >= 0 {
				return false
			}
			i = next
		}
	}
	return true
}

func decodedSimpleEscape(c byte) byte {
	switch c {
	case 'b':
		return '\b'
	case 'f':
		return '\f'
	case 'n':
		return '\n'
	case 'r':
		return '\r'
	case 't':
		return '\t'
	default:
		return c
	}
}
