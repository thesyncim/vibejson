package jsoncmp

import (
	"unicode/utf16"
	"unicode/utf8"

	"github.com/thesyncim/vibejson/document"
	"github.com/thesyncim/vibejson/x/byteview"
)

// StringByteIter streams the decoded UTF-8 bytes of a validated JSON string
// interior (the bytes strictly between the enclosing quotes) with zero heap
// allocations.
type StringByteIter struct {
	Raw     []byte
	i       int
	encoded [utf8.UTFMax]byte
	pos     uint8
	n       uint8
}

// Next returns the next decoded UTF-8 byte, or (0, false) when the string
// interior is exhausted.
func (it *StringByteIter) Next() (byte, bool) {
	if it.pos < it.n {
		b := it.encoded[it.pos]
		it.pos++
		return b, true
	}
	if it.i >= len(it.Raw) {
		return 0, false
	}
	b := it.Raw[it.i]
	if b != '\\' {
		it.i++
		return b, true
	}
	it.i++
	if it.i >= len(it.Raw) {
		return 0, false
	}
	if it.Raw[it.i] != 'u' {
		b = decodedSimpleEscape(it.Raw[it.i])
		it.i++
		return b, true
	}
	if it.i+5 > len(it.Raw) {
		return 0, false
	}
	u, ok := hex4(it.Raw, it.i+1)
	if !ok {
		return 0, false
	}
	it.i += 5
	r := rune(u)
	if 0xD800 <= r && r <= 0xDBFF && it.i+6 <= len(it.Raw) &&
		it.Raw[it.i] == '\\' && it.Raw[it.i+1] == 'u' {
		if lo, ok2 := hex4(it.Raw, it.i+2); ok2 {
			dec := utf16.DecodeRune(r, rune(lo))
			if dec != utf8.RuneError {
				r = dec
				it.i += 6
			}
		}
	}
	it.n = uint8(utf8.EncodeRune(it.encoded[:], r))
	it.pos = 1
	return it.encoded[0], true
}

// RawStringEqual reports whether two validated, quoted JSON string tokens
// decode to identical UTF-8 byte sequences, with zero heap allocations.
//
// aFlags and bFlags are the corresponding [document.IndexEntry.Flags] bits
// (only [document.TapeFlagEscaped] is inspected). Callers that do not track
// escape flags may pass [document.TapeFlagEscaped] to force streaming decode.
func RawStringEqual(a []byte, aFlags uint8, b []byte, bFlags uint8) bool {
	if len(a) < 2 || len(b) < 2 {
		return false
	}
	aEscaped := aFlags&document.TapeFlagEscaped != 0
	bEscaped := bFlags&document.TapeFlagEscaped != 0
	switch {
	case !aEscaped && !bEscaped:
		return byteview.String(a[1:len(a)-1]) == byteview.String(b[1:len(b)-1])
	case !aEscaped:
		return RawStringEqualString(b, bFlags, byteview.String(a[1:len(a)-1]))
	case !bEscaped:
		return RawStringEqualString(a, aFlags, byteview.String(b[1:len(b)-1]))
	}

	ai := StringByteIter{Raw: a[1 : len(a)-1]}
	bi := StringByteIter{Raw: b[1 : len(b)-1]}
	for {
		ab, aok := ai.Next()
		bb, bok := bi.Next()
		if aok != bok || aok && ab != bb {
			return false
		}
		if !aok {
			return true
		}
	}
}

// RawStringEqualString reports whether a validated, quoted JSON string token
// decodes to the UTF-8 string s, with zero heap allocations.
//
// flags is the corresponding [document.IndexEntry.Flags] value (only
// [document.TapeFlagEscaped] is inspected). Callers that do not track escape
// flags may pass [document.TapeFlagEscaped].
func RawStringEqualString(raw []byte, flags uint8, s string) bool {
	if len(raw) < 2 {
		return false
	}
	if flags&document.TapeFlagEscaped == 0 {
		return byteview.String(raw[1:len(raw)-1]) == s
	}
	raw = raw[1 : len(raw)-1]
	si := 0
	for i := 0; i < len(raw); {
		if raw[i] != '\\' {
			if si >= len(s) || raw[i] != s[si] {
				return false
			}
			i++
			si++
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			c := decodedSimpleEscape(raw[i])
			if si >= len(s) || s[si] != c {
				return false
			}
			i++
			si++
			continue
		}
		if i+5 > len(raw) {
			return false
		}
		u, ok := hex4(raw, i+1)
		if !ok {
			return false
		}
		i += 5
		r := rune(u)
		if 0xD800 <= r && r <= 0xDBFF && i+6 <= len(raw) &&
			raw[i] == '\\' && raw[i+1] == 'u' {
			if lo, ok2 := hex4(raw, i+2); ok2 {
				dec := utf16.DecodeRune(r, rune(lo))
				if dec != utf8.RuneError {
					r = dec
					i += 6
				}
			}
		}
		var encoded [utf8.UTFMax]byte
		n := utf8.EncodeRune(encoded[:], r)
		if si+n > len(s) || byteview.String(encoded[:n]) != s[si:si+n] {
			return false
		}
		si += n
	}
	return si == len(s)
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

func hex4(src []byte, i int) (uint16, bool) {
	if i+4 > len(src) {
		return 0, false
	}
	var value uint16
	for k := 0; k < 4; k++ {
		c := src[i+k]
		var digit uint16
		switch {
		case c >= '0' && c <= '9':
			digit = uint16(c - '0')
		case c >= 'a' && c <= 'f':
			digit = uint16(c-'a') + 10
		case c >= 'A' && c <= 'F':
			digit = uint16(c-'A') + 10
		default:
			return 0, false
		}
		value = value<<4 | digit
	}
	return value, true
}
