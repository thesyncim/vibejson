package slopjson

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// stringTokenOracle reports whether src is exactly one strict JSON string
// token, using the scalar reference walker shared with the corpus tests.
func stringTokenOracle(src []byte) bool {
	if len(src) < 2 || src[0] != '"' {
		return false
	}
	next, ok := strictJSONStringEnd(src, 0)
	return ok && next == len(src)
}

var utf8TestPad = bytes.Repeat([]byte{'a'}, 128)

func quotedWithPad(dst []byte, prefix int, seq []byte) []byte {
	dst = append(dst[:0], '"')
	dst = append(dst, utf8TestPad[:prefix]...)
	dst = append(dst, seq...)
	return append(dst, '"')
}

// TestUTF8AllCodePointsValid encodes every Unicode scalar value and checks it
// is accepted inside a JSON string, both as a short token and straddling the
// 64-byte SIMD block boundary.
func TestUTF8AllCodePointsValid(t *testing.T) {
	t.Parallel() // pure differential: only local scratch and read-only fixtures
	buf := make([]byte, 0, 160)
	var enc [4]byte
	for r := rune(0x20); r <= 0x10FFFF; r++ {
		if r == '"' || r == '\\' || (0xD800 <= r && r <= 0xDFFF) {
			continue
		}
		n := utf8.EncodeRune(enc[:], r)
		for _, prefix := range []int{0, 62} {
			buf = quotedWithPad(buf, prefix, enc[:n])
			if !validString(buf) {
				t.Fatalf("code point U+%04X rejected at prefix %d: % x", r, prefix, enc[:n])
			}
		}
	}
}

// TestUTF8TwoByteExhaustive checks every two-byte sequence inside a quoted
// string against the scalar reference, at several block-boundary prefixes.
// This covers every malformed continuation, overlong two-byte form, truncated
// lead byte, and escape/control interaction in the two-byte space.
func TestUTF8TwoByteExhaustive(t *testing.T) {
	t.Parallel() // pure differential: only local scratch and read-only fixtures
	prefixes := []int{0, 1, 15, 16, 31, 32, 63, 64, 65}
	if testing.Short() {
		prefixes = []int{0, 63}
	}
	buf := make([]byte, 0, 160)
	doc := make([]byte, 0, 16)
	seq := make([]byte, 2)
	for hi := 0; hi < 256; hi++ {
		for lo := 0; lo < 256; lo++ {
			seq[0], seq[1] = byte(hi), byte(lo)
			var want bool
			for i, prefix := range prefixes {
				buf = quotedWithPad(buf, prefix, seq)
				oracle := stringTokenOracle(buf)
				if i == 0 {
					want = oracle
				} else if oracle != want {
					t.Fatalf("oracle verdict for % x depends on prefix %d", seq, prefix)
				}
				if got := validString(buf); got != oracle {
					t.Fatalf("validString(%% x = % x, prefix %d) = %v, oracle %v", seq, prefix, got, oracle)
				}
			}
			doc = append(append(append(doc[:0], '['), quotedWithPad(buf, 0, seq)...), ']')
			if got := Valid(doc); got != want {
				t.Fatalf("Valid([% x]) = %v, oracle %v", seq, got, want)
			}
			if hi%37 == 0 && lo%41 == 0 {
				buf = quotedWithPad(buf, 0, seq)
				if oracle := strictJSONValid(buf); oracle != want {
					t.Fatalf("reference oracles disagree on % x: strict %v, walker %v", seq, oracle, want)
				}
			}
		}
	}
}

// TestUTF8ThreeByteExhaustive sweeps the full three-byte space: every
// overlong three-byte form, every UTF-16 surrogate encoding, and every
// continuation-byte error combination.
func TestUTF8ThreeByteExhaustive(t *testing.T) {
	t.Parallel() // pure differential: only local scratch and read-only fixtures
	stride := 1
	if testing.Short() {
		stride = 7
	}
	buf := make([]byte, 0, 16)
	seq := make([]byte, 3)
	for first := 0; first < 256; first += stride {
		for second := 0; second < 256; second += stride {
			for third := 0; third < 256; third += stride {
				seq[0], seq[1], seq[2] = byte(first), byte(second), byte(third)
				buf = quotedWithPad(buf, 0, seq)
				oracle := stringTokenOracle(buf)
				if got := validString(buf); got != oracle {
					t.Fatalf("validString(% x) = %v, oracle %v", seq, got, oracle)
				}
			}
		}
	}
}

// utf8ClassAlphabet holds one byte per boundary class of the UTF-8 automaton
// plus the JSON string metacharacters, so four-byte combinations cover every
// class transition, including the planes beyond U+10FFFF and truncations by
// quotes, escapes, controls, and ASCII.
var utf8ClassAlphabet = []byte{
	0x00, 0x1F, 0x20, '"', '\\', 'a', 0x7F,
	0x80, 0x8F, 0x90, 0x9F, 0xA0, 0xBF,
	0xC0, 0xC1, 0xC2, 0xDF,
	0xE0, 0xE1, 0xEC, 0xED, 0xEE, 0xEF,
	0xF0, 0xF1, 0xF3, 0xF4, 0xF5, 0xF8, 0xFF,
}

// TestUTF8FourByteClassSweep enumerates every four-byte combination of the
// class alphabet at a short and a block-straddling prefix.
func TestUTF8FourByteClassSweep(t *testing.T) {
	t.Parallel() // pure differential: only local scratch and read-only fixtures
	prefixes := []int{0, 61}
	if testing.Short() {
		prefixes = []int{61}
	}
	buf := make([]byte, 0, 160)
	seq := make([]byte, 4)
	for _, a := range utf8ClassAlphabet {
		for _, b := range utf8ClassAlphabet {
			for _, c := range utf8ClassAlphabet {
				for _, d := range utf8ClassAlphabet {
					seq[0], seq[1], seq[2], seq[3] = a, b, c, d
					for _, prefix := range prefixes {
						buf = quotedWithPad(buf, prefix, seq)
						oracle := stringTokenOracle(buf)
						if got := validString(buf); got != oracle {
							t.Fatalf("validString(% x, prefix %d) = %v, oracle %v", seq, prefix, got, oracle)
						}
					}
				}
			}
		}
	}
}

// buildBitmapUTF8Document builds a whitespace-heavy document large enough for
// the bitmap validation engine, containing one long ASCII pad string whose
// bytes can be overwritten in place. It returns the document and the pad's
// byte range.
func buildBitmapUTF8Document(t *testing.T) (doc []byte, padStart, padEnd int) {
	t.Helper()
	pad := strings.Repeat("a", 96)
	type record struct {
		Name  string   `json:"name"`
		Text  string   `json:"text"`
		Notes []string `json:"notes"`
	}
	records := make([]record, 280)
	for i := range records {
		records[i] = record{
			Name:  "record name with spaces",
			Text:  "plain body text é日本語",
			Notes: []string{"one", "two", "three", "four"},
		}
	}
	records[80].Text = pad
	doc, err := json.MarshalIndent(records, "", "        ")
	if err != nil {
		t.Fatal(err)
	}
	if len(doc) < validBitmapMinBytes {
		t.Fatalf("bitmap UTF-8 document too small: %d", len(doc))
	}
	padStart = bytes.Index(doc, []byte(pad))
	if padStart < 0 {
		t.Fatal("pad string not found")
	}
	if ok, decided := validBitmap(doc); !decided || !ok {
		t.Fatalf("bitmap engine did not accept the clean document (ok %v, decided %v)", ok, decided)
	}
	return doc, padStart, padStart + len(pad)
}

// TestValidBitmapUTF8ClassPhases splices representative valid and invalid
// UTF-8 sequences into a bitmap-engine document at every 64-byte block phase,
// checking the engine agrees with the scalar validator on each.
func TestValidBitmapUTF8ClassPhases(t *testing.T) {
	doc, padStart, padEnd := buildBitmapUTF8Document(t)
	cases := []struct {
		name  string
		seq   []byte
		valid bool
	}{
		{"two-byte", []byte{0xC3, 0xB1}, true},
		{"three-byte", []byte{0xE2, 0x82, 0xA1}, true},
		{"four-byte", []byte{0xF0, 0x90, 0x8C, 0xBC}, true},
		{"max code point", []byte{0xF4, 0x8F, 0xBF, 0xBF}, true},
		{"bare continuation", []byte{0x80, 'a'}, false},
		{"overlong two-byte", []byte{0xC0, 0xAF}, false},
		{"overlong three-byte", []byte{0xE0, 0x9F, 0xBF}, false},
		{"surrogate", []byte{0xED, 0xA0, 0x81}, false},
		{"overlong four-byte", []byte{0xF0, 0x8F, 0xBF, 0xBF}, false},
		{"beyond max plane", []byte{0xF4, 0x90, 0x80, 0x80}, false},
		{"invalid lead F5", []byte{0xF5, 0x80, 0x80, 0x80}, false},
		{"five-byte form", []byte{0xF8, 0x88, 0x80, 0x80, 0x80}, false},
		{"truncated by ascii", []byte{0xE2, 0x82, 'a'}, false},
		{"truncated four-byte", []byte{0xF0, 0x90, 0x8C, 'a'}, false},
	}
	patched := make([]byte, len(doc))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for offset := padStart; offset+len(tc.seq) <= padEnd && offset < padStart+64; offset++ {
				copy(patched, doc)
				copy(patched[offset:], tc.seq)
				got, decided := validBitmap(patched)
				if !decided {
					t.Fatalf("engine declined patched document at offset %d", offset)
				}
				if got != tc.valid {
					t.Fatalf("validBitmap at phase %d = %v, want %v", offset%64, got, tc.valid)
				}
				if scalar := validateOptions(patched, Options{}); (scalar == nil) != tc.valid {
					t.Fatalf("scalar Validate at offset %d = %v, want valid %v", offset, scalar, tc.valid)
				}
			}
		})
	}
}
