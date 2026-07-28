package vibejson

import (
	"bytes"
	"unicode/utf16"
	"unicode/utf8"
)

func appendDecodedUnicodeEscapeRun(dst, raw []byte, i int) ([]byte, int, bool) {
	for i+1 < len(raw) && raw[i] == '\\' && raw[i+1] == 'u' {
		escape := i + 1
		if escape+5 > len(raw) {
			return dst, i, false
		}
		if raw[escape+1] == '0' && raw[escape+2] == '0' {
			hi := raw[escape+3] - '0'
			lo := hexNibbleTable[raw[escape+4]]
			if hi < 8 && lo < 0x10 {
				dst = append(dst, hi<<4|lo)
				i = escape + 5
				continue
			}
			if hexNibbleTable[raw[escape+3]]|lo >= 0x10 {
				return dst, i, false
			}
		}
		u, ok := hex4(raw, escape+1)
		if !ok {
			return dst, i, false
		}
		i = escape + 5
		switch {
		case u < 0x0800:
			dst = append(dst,
				0xC0|byte(u>>6),
				0x80|byte(u)&0x3F,
			)
		case u < 0xD800:
			dst = append(dst,
				0xE0|byte(u>>12),
				0x80|byte(u>>6)&0x3F,
				0x80|byte(u)&0x3F,
			)
		case u <= 0xDBFF:
			if i+6 > len(raw) || raw[i] != '\\' || raw[i+1] != 'u' {
				return dst, i, false
			}
			a := hexNibbleTable[raw[i+3]]
			b := hexNibbleTable[raw[i+4]]
			c := hexNibbleTable[raw[i+5]]
			if raw[i+2]|0x20 != 'd' || a < 0xC || a|b|c >= 0x10 {
				return dst, i, false
			}
			lo := uint16(0xD000) | uint16(a)<<8 | uint16(b)<<4 | uint16(c)
			r := 0x10000 + (uint32(u)-0xD800)<<10 + uint32(lo) - 0xDC00
			dst = append(dst,
				0xF0|byte(r>>18),
				0x80|byte(r>>12)&0x3F,
				0x80|byte(r>>6)&0x3F,
				0x80|byte(r)&0x3F,
			)
			i += 6
		case u <= 0xDFFF:
			return dst, i, false
		default:
			dst = append(dst,
				0xE0|byte(u>>12),
				0x80|byte(u>>6)&0x3F,
				0x80|byte(u)&0x3F,
			)
		}
	}
	return dst, i, true
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
