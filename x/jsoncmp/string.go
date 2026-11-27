package jsoncmp

import (
	"unicode/utf16"
	"unicode/utf8"

	"github.com/thesyncim/vibejson/document"
	"github.com/thesyncim/vibejson/x/byteview"
)

// hexTable maps ASCII hex digits to 0..15 and all other bytes to 0xFF.
var hexTable [256]uint8

// simpleEscapeTable maps the byte following '\' (other than 'u') to its
// decoded byte value.
var simpleEscapeTable [256]byte

func init() {
	for i := range hexTable {
		c := byte(i)
		switch {
		case c >= '0' && c <= '9':
			hexTable[i] = c - '0'
		case c >= 'a' && c <= 'f':
			hexTable[i] = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			hexTable[i] = c - 'A' + 10
		default:
			hexTable[i] = 0xFF
		}
		simpleEscapeTable[i] = c
	}
	simpleEscapeTable['b'] = '\b'
	simpleEscapeTable['f'] = '\f'
	simpleEscapeTable['n'] = '\n'
	simpleEscapeTable['r'] = '\r'
	simpleEscapeTable['t'] = '\t'
}

func indexBackslash(b []byte) int {
	if len(b) >= 16 {
		return indexBackslashSIMD(b)
	}
	for i := 0; i < len(b); i++ {
		if b[i] == '\\' {
			return i
		}
	}
	return -1
}

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
	c := it.Raw[it.i]
	if c != 'u' {
		it.i++
		return simpleEscapeTable[c], true
	}
	if it.i+5 > len(it.Raw) {
		return 0, false
	}
	u, ok := hex4(it.Raw[it.i+1:])
	if !ok {
		return 0, false
	}
	it.i += 5
	r := rune(u)
	if r < utf8.RuneSelf {
		return byte(r), true
	}
	if 0xD800 <= r && r <= 0xDBFF && it.i+6 <= len(it.Raw) &&
		it.Raw[it.i] == '\\' && it.Raw[it.i+1] == 'u' {
		if lo, ok2 := hex4(it.Raw[it.i+2:]); ok2 {
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

// decodeEscapeRune decodes the escape sequence starting at raw[i] == '\\',
// returning the decoded rune, the new index after the escape, and whether the
// escape was well-formed.
func decodeEscapeRune(raw []byte, i int) (rune, int, bool) {
	i++
	if i >= len(raw) {
		return 0, i, false
	}
	c := raw[i]
	if c != 'u' {
		return rune(simpleEscapeTable[c]), i + 1, true
	}
	if i+5 > len(raw) {
		return 0, i, false
	}
	u, ok := hex4(raw[i+1:])
	if !ok {
		return 0, i, false
	}
	i += 5
	r := rune(u)
	if 0xD800 <= r && r <= 0xDBFF && i+6 <= len(raw) &&
		raw[i] == '\\' && raw[i+1] == 'u' {
		if lo, ok2 := hex4(raw[i+2:]); ok2 {
			dec := utf16.DecodeRune(r, rune(lo))
			if dec != utf8.RuneError {
				r = dec
				i += 6
			}
		}
	}
	return r, i, true
}

// RawStringEqual reports whether two validated, quoted JSON string tokens
// decode to identical UTF-8 byte sequences, with zero heap allocations.
//
// aFlags and bFlags are the corresponding [document.IndexEntry.Flags] bits
// (only [document.TapeFlagEscaped] is inspected). Callers that do not track
// escape flags may pass [document.TapeFlagEscaped] to force streaming decode.
func RawStringEqual(a []byte, aFlags uint8, b []byte, bFlags uint8) bool {
	if (aFlags|bFlags)&document.TapeFlagEscaped == 0 {
		return len(a) >= 2 && byteview.String(a) == byteview.String(b)
	}
	return rawStringEqualEscaped(a, aFlags, b, bFlags)
}

func rawStringEqualEscaped(a []byte, aFlags uint8, b []byte, bFlags uint8) bool {
	if len(a) < 2 || len(b) < 2 {
		return false
	}
	if aFlags&document.TapeFlagEscaped == 0 {
		return rawStringEqualStringEscaped(b[1:len(b)-1], byteview.String(a[1:len(a)-1]))
	}
	if bFlags&document.TapeFlagEscaped == 0 {
		return rawStringEqualStringEscaped(a[1:len(a)-1], byteview.String(b[1:len(b)-1]))
	}
	if byteview.String(a) == byteview.String(b) {
		return true
	}
	ar := a[1 : len(a)-1]
	br := b[1 : len(b)-1]
	if len(ar) < (len(br)+5)/6 || len(br) < (len(ar)+5)/6 {
		return false
	}

	ai, bi := 0, 0
	for {
		if ai >= len(ar) || bi >= len(br) {
			return ai == len(ar) && bi == len(br)
		}
		ac, bc := ar[ai], br[bi]
		if ac != '\\' && bc != '\\' {
			// Fast path: match plain byte run up to next escape on either side.
			if ac != bc {
				return false
			}
			abs := indexBackslash(ar[ai:])
			bbs := indexBackslash(br[bi:])
			if abs < 0 {
				if bbs < 0 {
					return byteview.String(ar[ai:]) == byteview.String(br[bi:])
				}
				return rawStringEqualStringEscaped(br[bi:], byteview.String(ar[ai:]))
			}
			if bbs < 0 {
				return rawStringEqualStringEscaped(ar[ai:], byteview.String(br[bi:]))
			}
			m := abs
			if bbs < m {
				m = bbs
			}
			if byteview.String(ar[ai:ai+m]) != byteview.String(br[bi:bi+m]) {
				return false
			}
			ai += m
			bi += m
			continue
		}
		if ac == '\\' && bc == '\\' {
			// Both sides at an escape: compare decoded runes directly without
			// UTF-8 encoding!
			ra, nextA, okA := decodeEscapeRune(ar, ai)
			rb, nextB, okB := decodeEscapeRune(br, bi)
			if !okA || !okB || ra != rb {
				return false
			}
			ai, bi = nextA, nextB
			continue
		}
		// One side is an escape, the other is literal UTF-8 bytes.
		if ac == '\\' {
			ra, nextA, okA := decodeEscapeRune(ar, ai)
			if !okA {
				return false
			}
			ai = nextA
			if ra < utf8.RuneSelf {
				if br[bi] != byte(ra) {
					return false
				}
				bi++
				continue
			}
			var enc [utf8.UTFMax]byte
			n := utf8.EncodeRune(enc[:], ra)
			if bi+n > len(br) || byteview.String(enc[:n]) != byteview.String(br[bi:bi+n]) {
				return false
			}
			bi += n
		} else {
			rb, nextB, okB := decodeEscapeRune(br, bi)
			if !okB {
				return false
			}
			bi = nextB
			if rb < utf8.RuneSelf {
				if ar[ai] != byte(rb) {
					return false
				}
				ai++
				continue
			}
			var enc [utf8.UTFMax]byte
			n := utf8.EncodeRune(enc[:], rb)
			if ai+n > len(ar) || byteview.String(enc[:n]) != byteview.String(ar[ai:ai+n]) {
				return false
			}
			ai += n
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
	if flags&document.TapeFlagEscaped == 0 {
		return len(raw) >= 2 && byteview.String(raw[1:len(raw)-1]) == s
	}
	return rawStringEqualStringQuoted(raw, s)
}

func rawStringEqualStringQuoted(raw []byte, s string) bool {
	if len(raw) < 2 {
		return false
	}
	return rawStringEqualStringEscaped(raw[1:len(raw)-1], s)
}

func rawStringEqualStringEscaped(raw []byte, s string) bool {
	if len(s) > len(raw) || len(s) < (len(raw)+5)/6 {
		return false
	}
	i, si := 0, 0
	for {
		if i >= len(raw) {
			return si == len(s)
		}
		if raw[i] != '\\' {
			bs := indexBackslash(raw[i:])
			if bs < 0 {
				return byteview.String(raw[i:]) == s[si:]
			}
			if si+bs > len(s) || byteview.String(raw[i:i+bs]) != s[si:si+bs] {
				return false
			}
			i += bs
			si += bs
		}
		i++
		if i >= len(raw) {
			return false
		}
		c := raw[i]
		if c != 'u' {
			if si >= len(s) || s[si] != simpleEscapeTable[c] {
				return false
			}
			i++
			si++
			continue
		}
		if i+5 > len(raw) {
			return false
		}
		u, ok := hex4(raw[i+1:])
		if !ok {
			return false
		}
		i += 5
		r := rune(u)
		if r < utf8.RuneSelf {
			if si >= len(s) || s[si] != byte(r) {
				return false
			}
			si++
			continue
		}
		if 0xD800 <= r && r <= 0xDBFF && i+6 <= len(raw) &&
			raw[i] == '\\' && raw[i+1] == 'u' {
			if lo, ok2 := hex4(raw[i+2:]); ok2 {
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
}

func hex4(b []byte) (uint16, bool) {
	if len(b) < 4 {
		return 0, false
	}
	h0 := hexTable[b[0]]
	h1 := hexTable[b[1]]
	h2 := hexTable[b[2]]
	h3 := hexTable[b[3]]
	return uint16(h0)<<12 | uint16(h1)<<8 | uint16(h2)<<4 | uint16(h3), (h0|h1|h2|h3)&0x80 == 0
}
