package jsoncmp

import (
	"strings"
	"testing"

	"github.com/thesyncim/vibejson/document"
)

var benchBoolSink bool
var benchByteSink byte

func BenchmarkNumberEqual(b *testing.B) {
	cases := []struct {
		name string
		a, c []byte
	}{
		{"IdenticalInt", []byte("12345678"), []byte("12345678")},
		{"DiffFirstDigit", []byte("12345678"), []byte("22345678")},
		{"DiffSign", []byte("12345678"), []byte("-12345678")},
		{"DiffPlainIntSamePrefix", []byte("12345678"), []byte("12345679")},
		{"DiffPlainIntLong", []byte("123456789012345678901234567890"), []byte("123456789012345678901234567891")},
		{"ZeroNegZero", []byte("0"), []byte("-0")},
		{"ZeroFracExp", []byte("-0.000e+5"), []byte("0e-10")},
		{"IntVsExp", []byte("100"), []byte("1e2")},
		{"IntVsFracExp", []byte("100"), []byte("1.00e+2")},
		{"DecimalVsExp", []byte("-12.3400"), []byte("-1.234e1")},
		{"LongHighPrecEqual", []byte("123456789012345678901234567890"), []byte("1.2345678901234567890123456789e29")},
		{"SmallFracDiff", []byte("0.1"), []byte("0.10000000000000001")},
		{"SmallFracEqual", []byte("0.00012345"), []byte("1.2345e-4")},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			var sink bool
			for b.Loop() {
				sink = NumberEqual(tc.a, tc.c)
			}
			benchBoolSink = sink
		})
	}
}

func BenchmarkRawStringEqual(b *testing.B) {
	const esc = document.TapeFlagEscaped
	longPrefix := strings.Repeat("abcdefghij", 20) // 200 bytes
	cases := []struct {
		name   string
		a      []byte
		aFlags uint8
		c      []byte
		cFlags uint8
	}{
		{"UnescapedShortEqual", []byte(`"organization_id"`), 0, []byte(`"organization_id"`), 0},
		{"UnescapedShortDiff", []byte(`"organization_id"`), 0, []byte(`"organization_no"`), 0},
		{"EscapedIdenticalShort", []byte(`"hello\nworld"`), esc, []byte(`"hello\nworld"`), esc},
		{"EscapedIdenticalLong", []byte(`"` + longPrefix + `\nend"`), esc, []byte(`"` + longPrefix + `\nend"`), esc},
		{"EscapedBothUnicodeShort", []byte(`"caf\u00e9 \uD83D\uDE00"`), esc, []byte(`"caf\u00E9 \ud83d\ude00"`), esc},
		{"EscapedBothLongPrefix", []byte(`"` + longPrefix + `\u00e9"`), esc, []byte(`"` + longPrefix + `\u00E9"`), esc},
		{"EscapedBothManySimple", []byte(`"a\nb\tc\rd\be\ff\"g\\h\/i"`), esc, []byte(`"a\u000ab\u0009c\u000dd\u0008e\u000cf\u0022g\u005ch\u002fi"`), esc},
		{"EscapedVsUnescapedLong", []byte(`"` + longPrefix + `\u00e9"`), esc, []byte(`"` + longPrefix + `é"`), 0},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			var sink bool
			for b.Loop() {
				sink = RawStringEqual(tc.a, tc.aFlags, tc.c, tc.cFlags)
			}
			benchBoolSink = sink
		})
	}
}

func BenchmarkRawStringEqualString(b *testing.B) {
	const esc = document.TapeFlagEscaped
	longPrefix := strings.Repeat("abcdefghij", 20)
	cases := []struct {
		name  string
		raw   []byte
		flags uint8
		s     string
	}{
		{"UnescapedShort", []byte(`"organization_id"`), 0, "organization_id"},
		{"EscapedConservativeUnescapedLong", []byte(`"` + longPrefix + `"`), esc, longPrefix},
		{"EscapedSimpleLong", []byte(`"` + longPrefix + `\nend"`), esc, longPrefix + "\nend"},
		{"EscapedUnicodeShort", []byte(`"caf\u00e9 \uD83D\uDE00"`), esc, "café 😀"},
		{"EscapedLengthMismatch", []byte(`"` + longPrefix + `\n"`), esc, "short"},
		{"EscapedMismatchAtEnd", []byte(`"` + longPrefix + `\nend"`), esc, longPrefix + "\nfin"},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			var sink bool
			for b.Loop() {
				sink = RawStringEqualString(tc.raw, tc.flags, tc.s)
			}
			benchBoolSink = sink
		})
	}
}

func BenchmarkStringByteIter(b *testing.B) {
	longPlain := []byte(strings.Repeat("abcdefghij", 20))
	escaped := []byte(`a\nb\tc\rd\be\ff\"g\\h\/i caf\u00e9 \uD83D\uDE00`)
	b.Run("LongPlain", func(b *testing.B) {
		var acc byte
		for b.Loop() {
			it := StringByteIter{Raw: longPlain}
			for {
				c, ok := it.Next()
				if !ok {
					break
				}
				acc ^= c
			}
		}
		benchByteSink = acc
	})
	b.Run("MixedEscapes", func(b *testing.B) {
		var acc byte
		for b.Loop() {
			it := StringByteIter{Raw: escaped}
			for {
				c, ok := it.Next()
				if !ok {
					break
				}
				acc ^= c
			}
		}
		benchByteSink = acc
	})
}
